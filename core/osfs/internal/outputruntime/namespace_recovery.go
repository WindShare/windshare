package outputruntime

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

// stagedData is the writable stage capability. Its distinct type prevents a
// caller from accidentally using the mutable link as publication authority.
type stagedData struct {
	file outputcap.File
}

func (stage stagedData) valid() bool  { return stage.file != nil }
func (stage stagedData) Close() error { return closeOutputV3File(stage.file) }
func (stage stagedData) WriteAt(data []byte, offset int64) (int, error) {
	return stage.file.WriteAt(data, offset)
}
func (stage stagedData) Sync() error { return stage.file.Sync() }
func (stage stagedData) SetModifiedTime(modified catalog.ModifiedTime) error {
	return stage.file.SetModifiedTime(modified)
}
func (stage stagedData) SameFile(anchor anchorWitness) (bool, error) {
	if !stage.valid() || !anchor.valid() {
		return false, outputcap.ErrUnsafeNamespace
	}
	return stage.file.SameFile(anchor.file)
}

// anchorWitness is the retained read-only, data-bearing link. Only this role
// can supply a source to the no-replace publication primitive.
type anchorWitness struct {
	file outputcap.File
}

func (anchor anchorWitness) valid() bool  { return anchor.file != nil }
func (anchor anchorWitness) Close() error { return closeOutputV3File(anchor.file) }

// publicationWitness proves that the freshly reopened private stage and anchor
// names still identify one record-sized object. It is intentionally not
// constructible from either role alone.
type publicationWitness struct {
	stage  stagedData
	anchor anchorWitness
}

func (witness *publicationWitness) Close() error {
	if witness == nil {
		return nil
	}
	var result error
	if witness.stage.valid() {
		result = errors.Join(result, witness.stage.Close())
		witness.stage = stagedData{}
	}
	if witness.anchor.valid() {
		result = errors.Join(result, witness.anchor.Close())
		witness.anchor = anchorWitness{}
	}
	return result
}

// outputV3FileNamespaceSnapshot contains names and immutable record authority,
// never open handles. Callers can therefore reuse one bounded preflight result
// without extending a shard handle's lifetime into mutation logic.
type outputV3FileNamespaceSnapshot struct {
	shards           []outputV3FileNamespaceShard
	records          []outputV3FileNamespaceRecord
	duplicateObjects []outputV3DuplicateOutputObject
	attention        []ResumeAttention
}

type outputV3FileNamespaceShard struct {
	name    string
	entries []outputV3FileNamespaceEntry
}

type outputV3FileNamespaceEntry struct {
	name           string
	classification resumestate.ClassifiedFileShardEntry
}

type outputV3FileNamespaceRecord struct {
	shardName  string
	recordName string
	bound      resumestate.BoundFileRecord
}

type outputV3DuplicateOutputObject struct {
	object  resumestate.OutputObjectID
	records []outputV3FileNamespaceRecord
}

// scanOutputV3FileNamespace must run while the session lock is held. It counts
// every direct name before reading record contents, so persisted bytes cannot
// grant authority until the complete namespace is proven to fit its selection-
// derived budget.
func scanOutputV3FileNamespace(
	session *Session,
) (outputV3FileNamespaceSnapshot, error) {
	if session == nil {
		return outputV3FileNamespaceSnapshot{}, outputfault.New(
			transfer.OutputFaultSession, transfer.OutputFaultContract, transfer.ErrOutputContract,
		)
	}
	session.mu.Lock()
	filesDirectory, lockHeld := session.filesDir, session.sessionLock != nil
	session.mu.Unlock()
	if filesDirectory == nil || !lockHeld {
		return outputV3FileNamespaceSnapshot{}, outputfault.New(
			transfer.OutputFaultSession, transfer.OutputFaultOwnership, outputfault.ErrSessionClosed,
		)
	}

	state := session.stateSnapshot()
	budget, err := resumestate.NewFileStateNamespaceBudget(state.Header().SelectedFileCount())
	if err != nil {
		return outputV3FileNamespaceSnapshot{}, outputfault.New(
			transfer.OutputFaultSession, transfer.OutputFaultContract, err,
		)
	}
	shardNames, err := filesDirectory.Names(resumestate.MaxFileStateShardDirectories + 1)
	if err != nil {
		return outputV3FileNamespaceSnapshot{}, outputV3FileNamespaceScanFault(err)
	}
	slices.Sort(shardNames)

	snapshot := outputV3FileNamespaceSnapshot{
		shards: make([]outputV3FileNamespaceShard, 0, min(len(shardNames), resumestate.MaxFileStateShardDirectories)),
	}
	for _, shardName := range shardNames {
		if err := budget.ObserveShard(); err != nil {
			return outputV3FileNamespaceSnapshot{}, outputV3FileNamespaceScanFault(err)
		}
		kind, exact, classifyErr := filesDirectory.ClassifyExactEntry(shardName)
		if classifyErr != nil {
			return outputV3FileNamespaceSnapshot{}, outputV3FileNamespaceScanFault(classifyErr)
		}
		if !validStateShard(shardName) || !exact || kind != outputcap.EntryDirectory {
			snapshot.attention = append(snapshot.attention, ResumeAttention{
				Scope: ResumeAttentionFile, Code: "unclassified-file-shard", State: shardName,
			})
			continue
		}
		shard, openErr := filesDirectory.OpenDirectory(shardName, true)
		if openErr != nil {
			return outputV3FileNamespaceSnapshot{}, outputV3FileNamespaceScanFault(openErr)
		}
		names, listErr := shard.Names(resumestate.MaxFileStateEntriesPerSession + 1)
		closeErr := shard.Close()
		if err := errors.Join(listErr, closeErr); err != nil {
			return outputV3FileNamespaceSnapshot{}, outputV3FileNamespaceScanFault(err)
		}
		slices.Sort(names)
		entries := make([]outputV3FileNamespaceEntry, 0, len(names))
		for _, name := range names {
			classified := resumestate.ClassifyFileShardEntry(shardName, name)
			if err := budget.ObserveEntry(classified); err != nil {
				return outputV3FileNamespaceSnapshot{}, outputV3FileNamespaceScanFault(err)
			}
			entries = append(entries, outputV3FileNamespaceEntry{name: name, classification: classified})
		}
		snapshot.shards = append(snapshot.shards, outputV3FileNamespaceShard{name: shardName, entries: entries})
	}

	// Decoding is deliberately a second pass. Even an early syntactically valid
	// record must not become authority if a later shard exceeds the global bound.
	for _, shard := range snapshot.shards {
		records, attention, scanErr := scanOutputV3FileNamespaceShard(filesDirectory, state, shard)
		if scanErr != nil {
			return outputV3FileNamespaceSnapshot{}, outputV3FileNamespaceScanFault(scanErr)
		}
		snapshot.records = append(snapshot.records, records...)
		snapshot.attention = append(snapshot.attention, attention...)
	}
	stableShardNames, err := filesDirectory.Names(resumestate.MaxFileStateShardDirectories + 1)
	if err != nil {
		return outputV3FileNamespaceSnapshot{}, outputV3FileNamespaceScanFault(err)
	}
	slices.Sort(stableShardNames)
	if !slices.Equal(shardNames, stableShardNames) {
		return outputV3FileNamespaceSnapshot{}, outputV3FileNamespaceScanFault(
			fmt.Errorf("%w: file-state shard namespace changed during preflight", outputcap.ErrUnsafeNamespace),
		)
	}

	snapshot.indexDuplicateOutputObjects()
	return snapshot, nil
}

func scanOutputV3FileNamespaceShard(
	filesDirectory outputcap.Directory,
	state resumestate.SessionAuthority,
	expected outputV3FileNamespaceShard,
) (_ []outputV3FileNamespaceRecord, _ []ResumeAttention, resultErr error) {
	kind, exact, err := filesDirectory.ClassifyExactEntry(expected.name)
	if err != nil || !exact || kind != outputcap.EntryDirectory {
		return nil, nil, errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	shard, err := filesDirectory.OpenDirectory(expected.name, true)
	if err != nil {
		return nil, nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, shard.Close()) }()
	if err := outputnamespace.VerifyPinnedDirectoryEntry(filesDirectory, expected.name, shard); err != nil {
		return nil, nil, err
	}
	names, err := shard.Names(resumestate.MaxFileStateEntriesPerSession + 1)
	if err != nil {
		return nil, nil, err
	}
	slices.Sort(names)
	expectedNames := make([]string, len(expected.entries))
	for index := range expected.entries {
		expectedNames[index] = expected.entries[index].name
	}
	if !slices.Equal(names, expectedNames) {
		return nil, nil, fmt.Errorf("%w: file-state shard changed during preflight", outputcap.ErrUnsafeNamespace)
	}

	records := make([]outputV3FileNamespaceRecord, 0)
	attention := make([]ResumeAttention, 0)
	for _, entry := range expected.entries {
		stateName := expected.name + "/" + entry.name
		entryKind, entryExact, classifyErr := shard.ClassifyExactEntry(entry.name)
		if classifyErr != nil || !entryExact || entryKind != outputcap.EntryRegularFile {
			attention = append(attention, fileNamespaceAttention(
				"unsafe-file-state-entry", stateName, classifyErr,
			))
			continue
		}
		switch entry.classification.Classification() {
		case resumestate.FileShardEntryRecord:
			readResult := outputnamespace.ReadRecordWithCleanup(
				shard, entry.name, resumestate.MaxFileStateBytes,
			)
			encoded, readErr, recordCloseErr := readResult.Encoded, readResult.ReadError, readResult.CloseError
			if readErr != nil {
				attention = append(attention, fileNamespaceAttention("unreadable-file-record", stateName, readErr))
				continue
			}
			record, decodeErr := resumestate.DecodeFileRecord(encoded)
			if decodeErr != nil || record.LocatorDigest() != entry.classification.Locator() {
				attention = append(attention, fileNamespaceAttention("corrupt-file-record", stateName, decodeErr))
				continue
			}
			bound, bindErr := resumestate.BindFileRecord(state, expected.name, entry.name, record)
			if bindErr != nil {
				attention = append(attention, fileNamespaceAttention("unbound-file-record", stateName, bindErr))
				continue
			}
			if recordCloseErr != nil {
				// Exact bytes still bind this object ID. Abort the entire admission
				// instead of degrading it to attention and opening a session whose
				// duplicate-object index silently omitted durable authority.
				return nil, nil, recordCloseErr
			}
			records = append(records, outputV3FileNamespaceRecord{
				shardName: expected.name, recordName: entry.name, bound: bound,
			})
		case resumestate.FileShardEntryUpdateTemporary:
			// A canonical update temporary is a bounded crash cut. Its target-bound
			// reducer, rather than namespace enumeration, decides whether to install
			// or remove it.
		case resumestate.FileShardEntryMalformedForLocator:
			attention = append(attention, fileNamespaceAttention("malformed-file-state-entry", stateName, nil))
		case resumestate.FileShardEntryOpaque:
			attention = append(attention, fileNamespaceAttention("opaque-file-state-entry", stateName, nil))
		}
	}
	if err := outputnamespace.VerifyPinnedDirectoryEntry(filesDirectory, expected.name, shard); err != nil {
		return nil, nil, err
	}
	return records, attention, nil
}

func (snapshot *outputV3FileNamespaceSnapshot) indexDuplicateOutputObjects() {
	groups := make(map[resumestate.OutputObjectID]int, len(snapshot.records))
	all := make([]outputV3DuplicateOutputObject, 0)
	for _, record := range snapshot.records {
		object := record.bound.Record().OutputObject()
		index, found := groups[object]
		if !found {
			groups[object] = len(all)
			all = append(all, outputV3DuplicateOutputObject{object: object, records: []outputV3FileNamespaceRecord{record}})
			continue
		}
		all[index].records = append(all[index].records, record)
	}
	for _, group := range all {
		if len(group.records) < 2 {
			continue
		}
		snapshot.duplicateObjects = append(snapshot.duplicateObjects, group)
		for _, record := range group.records {
			snapshot.attention = append(snapshot.attention, ResumeAttention{
				Scope: ResumeAttentionFile, Code: "duplicate-output-object",
				State: record.shardName + "/" + record.recordName, Detail: group.object.String(),
			})
		}
	}
}

// adoptFileNamespaceSnapshot reserves every persisted object identity before
// BeginFile can allocate a new one. Duplicate claims are then made durably
// non-resumable under the still-held session lock, so a crash between member
// updates can only leave a reducer-recognized partial quarantine cut.
func (session *Session) adoptFileNamespaceSnapshot(
	snapshot outputV3FileNamespaceSnapshot,
) error {
	session.mu.Lock()
	for _, scanned := range snapshot.records {
		record := scanned.bound.Record()
		if _, claimed := session.objectClaims[record.OutputObject()]; !claimed {
			session.objectClaims[record.OutputObject()] = record.LocatorDigest()
		}
	}
	for _, duplicate := range snapshot.duplicateObjects {
		for _, scanned := range duplicate.records {
			session.duplicateObjects[scanned.bound.Record().LocatorDigest()] = struct{}{}
		}
	}
	session.mu.Unlock()

	for _, duplicate := range snapshot.duplicateObjects {
		leader := duplicate.records[0]
		leaderSettled := false
		for _, member := range duplicate.records[1:] {
			decision, err := resumestate.ReduceDuplicateOutputObject(leader.bound, member.bound)
			if err != nil {
				return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultContract, err)
			}
			if !leaderSettled {
				next, err := resumestate.ApplyDuplicateOutputObjectDecision(leader.bound, decision)
				if err != nil {
					return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultContract, err)
				}
				if err := session.installScannedFileRecord(leader, next); err != nil {
					return err
				}
				leaderSettled = true
			}
			next, err := resumestate.ApplyDuplicateOutputObjectDecision(member.bound, decision)
			if err != nil {
				return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultContract, err)
			}
			if err := session.installScannedFileRecord(member, next); err != nil {
				return err
			}
		}
	}
	return nil
}

func (session *Session) installScannedFileRecord(
	scanned outputV3FileNamespaceRecord,
	next resumestate.BoundFileRecord,
) error {
	if next.Record().StateGeneration() == scanned.bound.Record().StateGeneration() {
		return nil
	}
	shard, err := session.filesDir.OpenDirectory(scanned.shardName, true)
	if err != nil {
		return fileOutputFault("open duplicate-object record shard", err)
	}
	if err := outputnamespace.VerifyPinnedDirectoryEntry(session.filesDir, scanned.shardName, shard); err != nil {
		_ = shard.Close()
		return fileOutputFault("pin duplicate-object record shard", err)
	}
	installErr := session.installFileRecord(shard, scanned.recordName, scanned.bound, next)
	closeErr := shard.Close()
	if closeErr != nil {
		closeErr = fileOutputFault("close duplicate-object record shard", closeErr)
	}
	if installErr != nil || closeErr != nil {
		return errors.Join(installErr, closeErr)
	}
	return nil
}

func fileNamespaceAttention(code, state string, cause error) ResumeAttention {
	detail := ""
	if cause != nil {
		detail = cause.Error()
	}
	return ResumeAttention{Scope: ResumeAttentionFile, Code: code, State: state, Detail: detail}
}

func outputV3FileNamespaceScanFault(err error) error {
	class := transfer.OutputFaultStateIO
	if errors.Is(err, outputcap.ErrUnsafeNamespace) || errors.Is(err, resumestate.ErrFileStateNamespaceLimit) {
		class = transfer.OutputFaultNamespaceUnsafe
	}
	return transfer.NewOutputSessionError(
		outputfault.New(transfer.OutputFaultSession, class, err),
		true,
	)
}

func isMissing(err error) bool { return errors.Is(err, fs.ErrNotExist) }

type outputV3RecoveryBoundary uint8

const (
	outputV3BeforeEntryEvidence outputV3RecoveryBoundary = iota + 1
	outputV3ExistingEntryUnclassified
	outputV3AuthorizedMutation
)

type outputV3RecoveryFailureDisposition uint8

const (
	outputV3RecoveryPauseRequired outputV3RecoveryFailureDisposition = iota + 1
	outputV3RecoveryAmbiguous
)

// classifyOutputV3RecoveryFailure separates lack of operational authority from
// ambiguous namespace evidence. The former preserves the deterministic cut for
// retry; the latter must never turn a pathname into cleanup authority.
func classifyOutputV3RecoveryFailure(
	cause error,
	boundary outputV3RecoveryBoundary,
) outputV3RecoveryFailureDisposition {
	if cause == nil {
		return 0
	}
	if boundary == outputV3ExistingEntryUnclassified || errors.Is(cause, outputnamespace.ErrPositiveEntryEvidence) ||
		errors.Is(cause, outputcap.ErrUnsafeNamespace) || errors.Is(cause, outputcap.ErrNamespaceCollision) {
		return outputV3RecoveryAmbiguous
	}
	return outputV3RecoveryPauseRequired
}

func recoveryFileOutputFault(
	operation string,
	cause error,
	boundary outputV3RecoveryBoundary,
) error {
	fault := fileOutputFault(operation, cause)
	if classifyOutputV3RecoveryFailure(cause, boundary) == outputV3RecoveryPauseRequired {
		return pauseRequiredFileOutputFault(fault)
	}
	return fault
}

func pauseRequiredFileOutputFault(cause error) error {
	return transfer.NewOutputSessionError(fileSettlementFailure(cause), true)
}

func pauseRequiredFileOperationFault(
	operation string,
	operationErr error,
	cleanupErr error,
) error {
	var result error
	if errors.Is(operationErr, errOutputAncestryUnsafe) {
		result = outputAncestryPauseFault(operation, operationErr)
	} else if operationErr != nil {
		result = pauseRequiredFileOutputFault(fileOutputFault(operation, operationErr))
	}
	if cleanupErr != nil {
		result = errors.Join(result, pauseRequiredFileOutputFault(outputfault.New(
			transfer.OutputFaultFile,
			transfer.OutputFaultStateIO,
			fmt.Errorf("clean up after %s: %w", operation, cleanupErr),
		)))
	}
	return result
}

type filesystemOutputFileSettlementTraceContext struct {
	boundary     FilesystemOutputFileSettlementBoundary
	pauseReason  transfer.FilePauseReason
	retireReason transfer.FileRetireReason
}

func (session *Session) traceReturnedFileStart(
	traceContext filesystemOutputFileSettlementTraceContext,
	start transfer.FileStart,
	resultErr error,
) {
	settlement, settled := start.ImmediateSettlement()
	if !settled {
		return
	}
	session.traceReturnedFileSettlement(traceContext, settlement, resultErr)
}

func (session *Session) traceReturnedFileSettlement(
	traceContext filesystemOutputFileSettlementTraceContext,
	settlement transfer.FileSettlement,
	resultErr error,
) {
	if session == nil || settlement.Kind() < transfer.FilePublished || settlement.Kind() > transfer.FileQuarantined {
		return
	}
	target := settlement.Target()
	event := FilesystemOutputTrace{
		Operation:              TraceFileSettlement,
		ResumeIntent:           session.resumeIntent,
		SessionID:              target.OutputSessionID(),
		LocatorDigest:          target.Locator().Digest(),
		FileSettlement:         settlement.Kind(),
		FileSettlementBoundary: traceContext.boundary,
		FilePauseReason:        traceContext.pauseReason,
		FileRetireReason:       traceContext.retireReason,
		Failed:                 resultErr != nil,
	}
	if binding, bound := settlement.OutputBinding(); bound {
		event.OutputObjectID = binding.ObjectIdentity()
	}
	if _, reason, quarantined := settlement.Quarantine(); quarantined {
		event.QuarantineReason = reason
	}
	event.FailureScope, event.FailureCode = filesystemOutputTraceFailure(resultErr)
	session.owner.trace(event)
}

func filesystemOutputTraceFailure(err error) (transfer.OutputFaultScope, transfer.OutputFaultCode) {
	var fault *transfer.OutputFault
	if !errors.As(err, &fault) {
		return 0, 0
	}
	return fault.Scope(), fault.Code()
}

func recoveryDecisionQuarantineReason(decision resumestate.RecoveryDecision) transfer.QuarantineReason {
	if decision.Action() != resumestate.RecoveryInstallQuarantine &&
		decision.Action() != resumestate.RecoveryHoldQuarantine || decision.QuarantineReason() == 0 {
		return 0
	}
	return mapQuarantineReason(decision.QuarantineReason())
}

func outputV3FileNamespaceEntryNames(entries []outputV3FileNamespaceEntry) []string {
	names := make([]string, len(entries))
	for index := range entries {
		names[index] = entries[index].name
	}
	return names
}

func outputV3CloseShardFault(err error) error {
	if err == nil {
		return nil
	}
	return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
}

func (session *Session) inspectAndRemoveEmptyShards() (bool, error) {
	attention := false
	for _, parent := range []outputcap.Directory{session.stagesDir, session.anchorsDir, session.filesDir} {
		names, err := parent.Names(resumestate.MaxFileStateShardDirectories + 1)
		if err != nil {
			return false, outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
		}
		for _, name := range names {
			if !validStateShard(name) {
				attention = true
				continue
			}
			shard, err := parent.OpenDirectory(name, true)
			if err != nil {
				return false, outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
			}
			children, listErr := shard.Names(1)
			if listErr != nil {
				_ = shard.Close()
				return false, outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, listErr)
			}
			if len(children) != 0 {
				attention = true
				_ = shard.Close()
				continue
			}
			removeErr := parent.RemoveDirectory(name, shard)
			syncErr := parent.Sync()
			closeErr := shard.Close()
			if err := errors.Join(removeErr, syncErr, closeErr); err != nil {
				return false, outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
			}
		}
	}
	return attention, nil
}
