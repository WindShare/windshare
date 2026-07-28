package outputruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func (session *Session) FinalizeDirectory(
	ctx context.Context,
	directory transfer.OutputDirectory,
) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if session == nil {
		return transfer.ErrInvalidOutputBinding
	}
	if err := session.beginOperation(); err != nil {
		return err
	}
	defer session.endOperation()
	session.mu.Lock()
	selected, admitted := session.selectedDirs[directory.Path]
	session.mu.Unlock()
	if !admitted || selected.ModifiedTime != directory.ModifiedTime {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultContract, transfer.ErrInvalidOutputSelection)
	}
	requirement := outputAncestryRequirement{
		path: directory.Path, authority: outputAncestryMetadataAuthority,
	}
	locator := resumestate.DigestCanonicalLocator(directory.Path)
	validation, err := session.validateOutputAncestry(requirement)
	if err != nil {
		session.traceOutputAncestry(FilesystemOutputAncestryDirectoryFinalize, locator, err)
		return outputAncestryOperationFault("validate output ancestry before finalizing directory", err)
	}
	session.traceOutputAncestry(FilesystemOutputAncestryDirectoryFinalize, locator, nil)
	defer func() {
		resultErr = finishOutputAncestryOperation(
			session, validation, requirement, FilesystemOutputAncestryDirectoryFinalize, locator,
			"finish finalizing output directory", resultErr,
		)
	}()
	opened, err := validation.directory(directory.Path)
	if err != nil {
		return outputAncestryOperationFault("retain output directory for finalization", err)
	}
	if err := validation.revalidateRetainedDirectory(directory.Path, outputAncestryMetadataAuthority); err != nil {
		session.traceOutputAncestry(FilesystemOutputAncestryDirectoryFinalize, locator, err)
		return outputAncestryOperationFault("revalidate retained output directory for finalization", err)
	}
	result := opened.SetModifiedTime(directory.ModifiedTime)
	if result == nil {
		result = opened.Sync()
	}
	if err := result; err != nil {
		return outputDirectoryOperationFault("finalize output directory", err)
	}
	return nil
}

func (session *Session) BeginFile(
	ctx context.Context,
	file transfer.OutputFile,
) (resultStart transfer.FileStart, resultErr error) {
	if err := ctx.Err(); err != nil {
		return transfer.FileStart{}, err
	}
	if session == nil {
		return transfer.FileStart{}, transfer.ErrInvalidOutputBinding
	}
	if err := session.beginOperation(); err != nil {
		return transfer.FileStart{}, err
	}
	defer session.endOperation()
	state := session.stateSnapshot()
	if err := session.validateBeginFileSelection(file); err != nil {
		return transfer.FileStart{}, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultContract, err,
		)
	}
	defer func() {
		session.traceReturnedFileStart(filesystemOutputFileSettlementTraceContext{
			boundary: FilesystemOutputSettlementBeginFile,
		}, resultStart, resultErr)
	}()
	parentPath := outputLocatorParentPath(file.Path)
	digest := resumestate.DigestCanonicalLocator(file.Path)
	requirement := outputAncestryRequirement{path: parentPath, authority: outputAncestryCreateAuthority}
	ancestryValidation, err := session.validateOutputAncestry(requirement)
	if err != nil {
		session.traceOutputAncestry(FilesystemOutputAncestryBeginFile, digest, err)
		return transfer.FileStart{}, outputAncestryOperationFault("validate output ancestry before BeginFile", err)
	}
	session.traceOutputAncestry(FilesystemOutputAncestryBeginFile, digest, nil)
	defer finishBeginFileAncestry(
		session, ancestryValidation, requirement, digest, &resultStart, &resultErr,
	)
	return session.beginFileRecordPhase(ctx, file, state, ancestryValidation, parentPath, digest)
}

func (session *Session) validateBeginFileSelection(file transfer.OutputFile) error {
	target, targetErr := outputTargetForDescriptor(session.SessionID(), file.Descriptor, file.Path)
	selected, found := session.selectedFiles[file.Path]
	if targetErr != nil {
		return transfer.ErrInvalidOutputSelection
	}
	if !found || file.ExpectedSize != selected.ExpectedSize {
		return transfer.ErrInvalidOutputSelection
	}
	if file.Descriptor.ShareInstance() != session.selection.ShareInstance() ||
		file.Descriptor.FileID() != selected.FileID || file.Descriptor.ExactSize() != selected.ExpectedSize ||
		file.Descriptor.ModifiedTime() != selected.ModifiedTime || file.Descriptor.FileRevision().IsZero() {
		return transfer.ErrInvalidOutputSelection
	}
	if file.Target != target {
		return transfer.ErrInvalidOutputSelection
	}
	return nil
}

func (session *Session) beginFileRecordPhase(
	ctx context.Context,
	file transfer.OutputFile,
	state resumestate.SessionAuthority,
	ancestryValidation *outputAncestryValidation,
	parentPath string,
	digest resumestate.LocatorDigest,
) (resultStart transfer.FileStart, resultErr error) {
	if err := session.claimFileStart(digest); err != nil {
		return transfer.FileStart{}, err
	}
	defer session.releaseFileStart(digest)
	if session.fileStartIsDuplicate(digest) {
		return session.quarantinedStart(file.Target, digest, transfer.QuarantineStateCorrupt)
	}

	recordName := resumestate.FileRecordName(digest)
	recordDir, recordShardPresent, err := openOutputShard(session.filesDir, recordName.Shard(), false)
	if err != nil {
		if classifyOutputV3RecoveryFailure(err, outputV3BeforeEntryEvidence) == outputV3RecoveryAmbiguous {
			return session.quarantinedStart(file.Target, digest, transfer.QuarantineStateCorrupt)
		}
		return transfer.FileStart{}, recoveryFileOutputFault("open file-state shard", err, outputV3BeforeEntryEvidence)
	}
	recordOwned := recordDir != nil
	defer func() {
		resultStart, resultErr = finishBeginFileShard(recordDir, recordOwned, resultStart, resultErr)
	}()
	if recordShardPresent {
		start, handled, transactionStarted, beginErr := session.gateBeginFileState(ctx, file, recordDir, recordName)
		if beginErr != nil || handled {
			if beginErr == nil && transactionStarted {
				recordOwned = false
			}
			return start, beginErr
		}
	}

	_, collision, err := session.inspectBeginFileDestination(
		ancestryValidation, parentPath, file.Path, digest,
	)
	if err != nil {
		return transfer.FileStart{}, err
	}
	if collision {
		return session.collisionStart(file)
	}
	recordDir, created, err := session.ensureBeginFileShard(recordDir, recordShardPresent, recordName)
	if err != nil {
		return transfer.FileStart{}, err
	}
	recordOwned = recordOwned || created

	resumable, err := session.prepareInitialFileRecord(state, file, recordDir, recordName, digest)
	if err != nil {
		return transfer.FileStart{}, err
	}
	start, err := session.reduceFile(ctx, file, resumable, recordDir, recordName.Name())
	if err == nil {
		_, _, transactionStarted := start.Transaction()
		recordOwned = !transactionStarted
	}
	return start, err
}

func (session *Session) fileStartIsDuplicate(digest resumestate.LocatorDigest) bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	_, duplicate := session.duplicateObjects[digest]
	return duplicate
}

func finishBeginFileShard(
	recordDir outputcap.Directory,
	recordOwned bool,
	resultStart transfer.FileStart,
	resultErr error,
) (transfer.FileStart, error) {
	if !recordOwned {
		return resultStart, resultErr
	}
	if closeErr := closeOutputV3Directory(recordDir); closeErr != nil {
		return transfer.FileStart{}, pauseRequiredFileOutputFault(fileOutputFault(
			"close file-state shard after begin", errors.Join(resultErr, closeErr),
		))
	}
	return resultStart, resultErr
}

func (session *Session) gateBeginFileState(
	ctx context.Context,
	file transfer.OutputFile,
	recordDir outputcap.Directory,
	recordName resumestate.ShardedName,
) (transfer.FileStart, bool, bool, error) {
	start, handled, err := session.gateFileStateShard(ctx, file, recordDir, recordName)
	if err != nil || !handled {
		return start, handled, false, err
	}
	_, _, transactionStarted := start.Transaction()
	return start, true, transactionStarted, nil
}

func (session *Session) inspectBeginFileDestination(
	validation *outputAncestryValidation,
	parentPath string,
	filePath string,
	digest resumestate.LocatorDigest,
) (outputcap.Directory, bool, error) {
	parent, err := validation.directory(parentPath)
	if err != nil {
		return nil, false, outputAncestryOperationFault("retain final parent before begin", err)
	}
	_, leaf, err := outputLocatorParentAndLeaf(filePath)
	if err != nil {
		return nil, false, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if err := validation.revalidateRetainedDirectory(parentPath, outputAncestryCreateAuthority); err != nil {
		session.traceOutputAncestry(FilesystemOutputAncestryBeginFile, digest, err)
		return nil, false, outputAncestryOperationFault("revalidate final parent before begin", err)
	}
	finalKind, observeErr := parent.ObserveEntry(leaf)
	if observeErr != nil {
		fault := fileOutputFault("observe final before begin", observeErr)
		if errors.Is(observeErr, fs.ErrPermission) {
			fault = pauseRequiredFileOutputFault(fault)
		}
		return nil, false, fault
	}
	return parent, finalKind != outputcap.EntryAbsent, nil
}

func (session *Session) ensureBeginFileShard(
	recordDir outputcap.Directory,
	recordShardPresent bool,
	recordName resumestate.ShardedName,
) (outputcap.Directory, bool, error) {
	if recordShardPresent {
		return recordDir, false, nil
	}
	recordDir, _, err := openOutputShard(session.filesDir, recordName.Shard(), true)
	if err != nil {
		fault := fileOutputFault("create file-state shard", err)
		if errors.Is(err, fs.ErrPermission) {
			fault = pauseRequiredFileOutputFault(fault)
		}
		return nil, false, fault
	}
	return recordDir, true, nil
}

func (session *Session) prepareInitialFileRecord(
	state resumestate.SessionAuthority,
	file transfer.OutputFile,
	recordDir outputcap.Directory,
	recordName resumestate.ShardedName,
	digest resumestate.LocatorDigest,
) (resumestate.ResumableFileAuthority, error) {
	objectID, err := session.allocateOutputObjectID(digest)
	if err != nil {
		return resumestate.ResumableFileAuthority{}, fileOutputFault("allocate output object", err)
	}
	resumable, err := resumestate.NewFileRecord(resumestate.FileRecordSpec{
		Session: state, Descriptor: file.Descriptor, CanonicalLocator: file.Path, OutputObject: objectID,
	})
	if err != nil {
		return resumestate.ResumableFileAuthority{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	encoded, err := resumestate.EncodeFileRecord(resumable.Bound())
	if err != nil {
		return resumestate.ResumableFileAuthority{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if err := session.installInitialFileRecord(recordDir, recordName.Name(), encoded); err != nil {
		return resumestate.ResumableFileAuthority{}, err
	}
	return resumable, nil
}

func (session *Session) installInitialFileRecord(
	recordDir outputcap.Directory,
	recordName string,
	encoded []byte,
) error {
	installOutcome, installErr := session.ensureInitialFileRecord(recordDir, recordName, encoded)
	switch installOutcome {
	case outputnamespace.CreateAdopted:
		if installErr == nil {
			return nil
		}
		session.poisonState()
		return pauseRequiredFileOutputFault(fileOutputFault(
			"finish adopted reserved file state", installErr,
		))
	case outputnamespace.CreateNotInstalled:
		if installErr == nil {
			installErr = outputcap.ErrUnsafeNamespace
		}
		return pauseRequiredFileOutputFault(fileOutputFault("install reserved file state", installErr))
	case outputnamespace.CreateUncertain:
		session.poisonState()
		// State-install ambiguity invalidates this owner, but it is still an I/O
		// settlement failure rather than evidence that a different owner exists.
		// Keeping that distinction lets the job pause and hand recovery to a fresh
		// opener without reporting a false ownership conflict.
		return pauseRequiredFileOutputFault(outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultStateIO,
			fmt.Errorf(
				"install reserved file state with uncertain authority: %w",
				errors.Join(outputcap.ErrUnsafeNamespace, installErr),
			),
		))
	default:
		session.poisonState()
		return pauseRequiredFileOutputFault(outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultContract, resumestate.ErrInvalidState,
		))
	}
}

func finishOutputAncestryOperation(
	session *Session,
	validation *outputAncestryValidation,
	requirement outputAncestryRequirement,
	boundary FilesystemOutputAncestryBoundary,
	locator resumestate.LocatorDigest,
	operation string,
	operationErr error,
) error {
	revalidateErr := validation.Revalidate(requirement)
	closeErr := closeOutputAncestryValidation(validation)
	ancestryErr := errors.Join(revalidateErr, closeErr)
	if boundary != 0 {
		session.traceOutputAncestry(boundary, locator, ancestryErr)
	}
	if ancestryErr == nil {
		return operationErr
	}
	return errors.Join(
		operationErr,
		outputAncestryOperationFault(operation, revalidateErr),
		outputAncestryCleanupFault("close output ancestry validation", closeErr),
	)
}

func finishBeginFileAncestry(
	session *Session,
	validation *outputAncestryValidation,
	requirement outputAncestryRequirement,
	locator resumestate.LocatorDigest,
	resultStart *transfer.FileStart,
	resultErr *error,
) {
	revalidateErr := validation.Revalidate(requirement)
	closeErr := closeOutputAncestryValidation(validation)
	ancestryErr := errors.Join(revalidateErr, closeErr)
	session.traceOutputAncestry(FilesystemOutputAncestryBeginFile, locator, ancestryErr)
	if ancestryErr == nil {
		return
	}
	var transactionCloseErr error
	if transaction, _, started := resultStart.Transaction(); started {
		owned, ok := transaction.(*FileTransaction)
		if !ok {
			transactionCloseErr = errors.New("BeginFile returned a foreign transaction")
			session.poisonState()
		} else {
			_, transactionCloseErr = owned.pauseForBeginFileCleanup(
				context.Background(), transfer.FilePauseOutputFailure,
			)
		}
	}
	*resultStart = transfer.FileStart{}
	*resultErr = errors.Join(
		*resultErr,
		outputAncestryOperationFault("finish BeginFile ancestry validation", revalidateErr),
		outputAncestryCleanupFault("close BeginFile ancestry validation", closeErr),
		outputAncestryCleanupFault("pause BeginFile transaction after ancestry failure", transactionCloseErr),
	)
}

func (session *Session) gateFileStateShard(
	ctx context.Context,
	file transfer.OutputFile,
	shard outputcap.Directory,
	recordName resumestate.ShardedName,
) (transfer.FileStart, bool, error) {
	names, err := shard.Names(outputnamespace.FileShardInspectionLimit)
	if err != nil {
		return transfer.FileStart{}, false, recoveryFileOutputFault(
			"inspect file-state shard", err, outputV3BeforeEntryEvidence,
		)
	}
	digest := resumestate.DigestCanonicalLocator(file.Path)
	inspection, ambiguous := inspectFileStateShard(shard, names, recordName, digest)
	if ambiguous || invalidFileStateTarget(inspection) {
		return session.quarantineFileStateStart(file, digest)
	}
	if inspection.targetKind == outputcap.EntryAbsent {
		return transfer.FileStart{}, false, nil
	}

	bound, recordCloseErr, err := session.openBoundFileRecord(shard, recordName)
	if err != nil {
		return session.quarantineFileStateStart(file, digest)
	}
	if recordCloseErr != nil {
		return transfer.FileStart{}, false, pauseRequiredFileOutputFault(fileOutputFault(
			"close bound file-state record", recordCloseErr,
		))
	}
	for _, name := range inspection.temporaries {
		start, handled, err := session.gateFileStateTemporary(file, shard, recordName, name, bound, digest)
		if handled || err != nil {
			return start, handled, err
		}
	}
	return session.resumeFile(ctx, file, shard, recordName.Name(), bound)
}

type fileStateShardInspection struct {
	targetKind  outputcap.EntryKind
	temporaries []string
}

func inspectFileStateShard(
	shard outputcap.Directory,
	names []string,
	recordName resumestate.ShardedName,
	digest resumestate.LocatorDigest,
) (fileStateShardInspection, bool) {
	inspection := fileStateShardInspection{targetKind: outputcap.EntryAbsent}
	for _, name := range names {
		classified := resumestate.ClassifyFileShardEntry(recordName.Shard(), name)
		switch classified.Classification() {
		case resumestate.FileShardEntryRecord:
			if classified.Locator() != digest {
				continue
			}
			if name != recordName.Name() || inspection.targetKind != outputcap.EntryAbsent {
				return inspection, true
			}
			kind, err := shard.ObserveEntry(name)
			if err != nil {
				return inspection, true
			}
			inspection.targetKind = kind
		case resumestate.FileShardEntryUpdateTemporary:
			if classified.Locator() == digest {
				inspection.temporaries = append(inspection.temporaries, name)
			}
		case resumestate.FileShardEntryMalformedForLocator:
			if classified.Locator() == digest {
				return inspection, true
			}
		}
	}
	return inspection, false
}

func invalidFileStateTarget(inspection fileStateShardInspection) bool {
	if inspection.targetKind != outputcap.EntryAbsent && inspection.targetKind != outputcap.EntryRegularFile {
		return true
	}
	return inspection.targetKind == outputcap.EntryAbsent && len(inspection.temporaries) != 0
}

func (session *Session) quarantineFileStateStart(
	file transfer.OutputFile,
	digest resumestate.LocatorDigest,
) (transfer.FileStart, bool, error) {
	start, err := session.quarantinedStart(file.Target, digest, transfer.QuarantineStateCorrupt)
	return start, true, err
}

func outputBindingForRecord(
	sessionID transfer.OutputSessionID,
	descriptor content.FileRevisionDescriptor,
	record resumestate.FileRecord,
) (transfer.OutputFileBinding, error) {
	locator, err := transfer.NewPathOutputLocator(record.CanonicalLocator())
	if err != nil {
		return transfer.OutputFileBinding{}, err
	}
	identity, err := transfer.OutputObjectIdentityFromBytes(record.OutputObject().Bytes())
	if err != nil {
		return transfer.OutputFileBinding{}, err
	}
	return transfer.NewOutputFileBinding(filesystemOutputBackendID, sessionID, descriptor, locator, identity)
}

func outputTargetForDescriptor(
	sessionID transfer.OutputSessionID,
	descriptor content.FileRevisionDescriptor,
	canonicalLocator string,
) (transfer.OutputFileTarget, error) {
	locator, err := transfer.NewPathOutputLocator(canonicalLocator)
	if err != nil {
		return transfer.OutputFileTarget{}, err
	}
	return transfer.NewOutputFileTarget(filesystemOutputBackendID, sessionID, descriptor, locator)
}

func (session *Session) verifiedStart(
	kind transfer.FileSettlementKind,
	resumable resumestate.ResumableFileAuthority,
) (transfer.FileStart, error) {
	record := resumable.Bound().Record()
	binding, err := outputBindingForRecord(session.SessionID(), resumable.Descriptor(), record)
	if err != nil {
		return transfer.FileStart{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	checkpoint, err := transfer.VerifyDurableRanges(
		binding, transfer.CheckpointGeneration(record.CheckpointGeneration()), record.DurableRanges(),
	)
	if err != nil {
		return transfer.FileStart{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	settlement, err := transfer.NewVerifiedFileSettlement(kind, checkpoint)
	if err != nil {
		return transfer.FileStart{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	return transfer.NewFileSettlementStart(settlement)
}

func (transaction *FileTransaction) WriteRange(
	ctx context.Context,
	offset uint64,
	data []byte,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if transaction == nil {
		return transfer.ErrInvalidOutputBinding
	}
	if err := transaction.session.beginOperation(); err != nil {
		return err
	}
	defer transaction.session.endOperation()
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	if transaction.lifecycle != FileTransactionOpen || transaction.session.operationDisabled() || !transaction.data.valid() ||
		transaction.resumable.Bound().Record().Phase() != resumestate.FileWitnessed {
		return outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultOwnership, outputfault.ErrSessionClosed)
	}
	if len(data) == 0 {
		return nil
	}
	if offset > transaction.binding.ExactSize() || uint64(len(data)) > transaction.binding.ExactSize()-offset ||
		offset > math.MaxInt64 || uint64(len(data)) > math.MaxInt64-offset {
		return outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, outputfault.ErrOutOfRange)
	}
	end := offset + uint64(len(data))
	record := transaction.resumable.Bound().Record()
	if rangeSetIntersects(record.DurableRanges(), offset, end) || rangeSetIntersects(transaction.pending, offset, end) {
		// A verified checkpoint remains authoritative across process restart. Any
		// later overwrite would let that old record authenticate different bytes;
		// pending writes are equally single-owner because the protocol has no
		// idempotent rewrite proof.
		return outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, errOutputV3RangeOverlap)
	}
	writtenRange, _ := content.NewRangeSet([]content.Range{{Offset: offset, End: end}})
	pending, err := transfer.MergeRanges(transaction.pending, writtenRange)
	if err != nil || pending.Len() > resumestate.MaxDurableRangesPerFile {
		return outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, errors.Join(err, resumestate.ErrInvalidState))
	}
	written, err := transaction.data.WriteAt(data, int64(offset))
	if err == nil && written != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return fileOutputFault("write output range", err)
	}
	transaction.pending = pending
	return nil
}
