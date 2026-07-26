package osfs

import (
	"errors"
	"fmt"
	"slices"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

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
	session *filesystemOutputSession,
) (outputV3FileNamespaceSnapshot, error) {
	if session == nil {
		return outputV3FileNamespaceSnapshot{}, outputFault(
			transfer.OutputFaultSession, transfer.OutputFaultContract, transfer.ErrOutputContract,
		)
	}
	session.mu.Lock()
	filesDirectory, lockHeld := session.filesDir, session.sessionLock != nil
	session.mu.Unlock()
	if filesDirectory == nil || !lockHeld {
		return outputV3FileNamespaceSnapshot{}, outputFault(
			transfer.OutputFaultSession, transfer.OutputFaultOwnership, errOutputSessionClosed,
		)
	}

	state := session.stateSnapshot()
	budget, err := resumestate.NewFileStateNamespaceBudget(state.Header().SelectedFileCount())
	if err != nil {
		return outputV3FileNamespaceSnapshot{}, outputFault(
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
		if !validStateShard(shardName) || !exact || kind != outputV3EntryDirectory {
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
			fmt.Errorf("%w: file-state shard namespace changed during preflight", errOutputV3Unsafe),
		)
	}

	snapshot.indexDuplicateOutputObjects()
	return snapshot, nil
}

func scanOutputV3FileNamespaceShard(
	filesDirectory outputV3Directory,
	state resumestate.SessionAuthority,
	expected outputV3FileNamespaceShard,
) (_ []outputV3FileNamespaceRecord, _ []ResumeAttention, resultErr error) {
	kind, exact, err := filesDirectory.ClassifyExactEntry(expected.name)
	if err != nil || !exact || kind != outputV3EntryDirectory {
		return nil, nil, errors.Join(errOutputV3Unsafe, err)
	}
	shard, err := filesDirectory.OpenDirectory(expected.name, true)
	if err != nil {
		return nil, nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, shard.Close()) }()
	if err := verifyPinnedDirectoryEntry(filesDirectory, expected.name, shard); err != nil {
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
		return nil, nil, fmt.Errorf("%w: file-state shard changed during preflight", errOutputV3Unsafe)
	}

	records := make([]outputV3FileNamespaceRecord, 0)
	attention := make([]ResumeAttention, 0)
	for _, entry := range expected.entries {
		stateName := expected.name + "/" + entry.name
		entryKind, entryExact, classifyErr := shard.ClassifyExactEntry(entry.name)
		if classifyErr != nil || !entryExact || entryKind != outputV3EntryRegularFile {
			attention = append(attention, fileNamespaceAttention(
				"unsafe-file-state-entry", stateName, classifyErr,
			))
			continue
		}
		switch entry.classification.Classification() {
		case resumestate.FileShardEntryRecord:
			encoded, readErr, recordCloseErr := readStateRecordWithCleanup(
				shard, entry.name, resumestate.MaxFileStateBytes,
			)
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
	if err := verifyPinnedDirectoryEntry(filesDirectory, expected.name, shard); err != nil {
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
func (session *filesystemOutputSession) adoptFileNamespaceSnapshot(
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
				return outputFault(transfer.OutputFaultSession, transfer.OutputFaultContract, err)
			}
			if !leaderSettled {
				next, err := resumestate.ApplyDuplicateOutputObjectDecision(leader.bound, decision)
				if err != nil {
					return outputFault(transfer.OutputFaultSession, transfer.OutputFaultContract, err)
				}
				if err := session.installScannedFileRecord(leader, next); err != nil {
					return err
				}
				leaderSettled = true
			}
			next, err := resumestate.ApplyDuplicateOutputObjectDecision(member.bound, decision)
			if err != nil {
				return outputFault(transfer.OutputFaultSession, transfer.OutputFaultContract, err)
			}
			if err := session.installScannedFileRecord(member, next); err != nil {
				return err
			}
		}
	}
	return nil
}

func (session *filesystemOutputSession) installScannedFileRecord(
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
	if err := verifyPinnedDirectoryEntry(session.filesDir, scanned.shardName, shard); err != nil {
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
	if errors.Is(err, errOutputV3Unsafe) || errors.Is(err, resumestate.ErrFileStateNamespaceLimit) {
		class = transfer.OutputFaultNamespaceUnsafe
	}
	return transfer.NewOutputSessionError(
		outputFault(transfer.OutputFaultSession, class, err),
		true,
	)
}
