package outputruntime

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"math"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
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
	accepted, acceptedDirectory := session.admittedDirs[directory.Path]
	incremental := session.incrementalAdmission
	session.mu.Unlock()
	if directory.Path == "" {
		admitted = directory.DirectoryID == session.selection.SyntheticRoot() &&
			directory.Generation == session.selection.RootGeneration()
	}
	parentMatches := directory.Path == ""
	if directory.Path != "" && acceptedDirectory {
		parentMatches = accepted.Parent().Equal(directory.ParentAdmission)
	}
	if !admitted || (directory.Path != "" && selected.ModifiedTime != directory.ModifiedTime) ||
		(incremental && (!acceptedDirectory || !parentMatches)) || (incremental && acceptedDirectory &&
		(accepted.DirectoryID() != directory.DirectoryID || accepted.Generation() != directory.Generation ||
			accepted.Path() != directory.Path)) {
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
	return session.beginFileCheckpointPhase(ctx, file, ancestryValidation, parentPath, digest)
}

func (session *Session) validateBeginFileSelection(file transfer.OutputFile) error {
	target, targetErr := outputTargetForDescriptor(session.SessionID(), file.Descriptor, file.Path)
	session.mu.Lock()
	selection := session.selection
	parentAdmission, admittedParent := session.admittedDirs[outputLocatorParentPath(file.Path)]
	incremental := session.incrementalAdmission
	session.mu.Unlock()
	var selected transfer.OutputSelectionFile
	var found bool
	var live resumestate.LiveFileSelection
	if incremental {
		live, found = session.incrementalFileSelection(file.Path)
		if found {
			selected = live.Selection
			if live.Revision != file.Descriptor.FileRevision() || !live.ParentAdmission.Equal(file.ParentAdmission) {
				return transfer.ErrInvalidOutputSelection
			}
		}
	} else {
		session.mu.Lock()
		selected, found = session.selectedFiles[file.Path]
		session.mu.Unlock()
	}
	if targetErr != nil {
		return transfer.ErrInvalidOutputSelection
	}
	if !found || file.ExpectedSize != selected.ExpectedSize {
		return transfer.ErrInvalidOutputSelection
	}
	if file.Descriptor.ShareInstance() != selection.ShareInstance() ||
		file.Descriptor.FileID() != selected.FileID || file.Descriptor.ExactSize() != selected.ExpectedSize ||
		file.Descriptor.ModifiedTime() != selected.ModifiedTime || file.Descriptor.FileRevision().IsZero() {
		return transfer.ErrInvalidOutputSelection
	}
	if file.Target != target {
		return transfer.ErrInvalidOutputSelection
	}
	if incremental && (!admittedParent || !file.ParentAdmission.Equal(parentAdmission)) {
		return transfer.ErrDirectoryAdmissionMismatch
	}
	return nil
}

func (session *Session) beginFileCheckpointPhase(
	ctx context.Context,
	file transfer.OutputFile,
	ancestryValidation *outputAncestryValidation,
	parentPath string,
	digest resumestate.LocatorDigest,
) (resultStart transfer.FileStart, resultErr error) {
	if err := session.claimFileStart(digest); err != nil {
		return transfer.FileStart{}, err
	}
	defer session.releaseFileStart(digest)
	if live, found := session.incrementalFileSelection(file.Path); found {
		if checkpoint, committed := session.incrementalCheckpointFor(live); committed {
			return session.beginFileFromIncrementalCheckpoint(ctx, file, checkpoint)
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
	resumable, err := session.prepareInitialCheckpointFile(file, digest)
	if err != nil {
		return transfer.FileStart{}, err
	}
	return session.reduceFile(ctx, file, resumable)
}

func (session *Session) prepareInitialCheckpointFile(
	file transfer.OutputFile,
	digest resumestate.LocatorDigest,
) (resumestate.CheckpointRuntimeFile, error) {
	objectID, err := session.allocateOutputObjectID(digest)
	if err != nil {
		return resumestate.CheckpointRuntimeFile{}, fileOutputFault("allocate output object", err)
	}
	resumable, err := resumestate.NewCheckpointRuntimeFile(
		session.checkpointRuntime, file.Descriptor, file.Path, objectID,
	)
	if err != nil {
		return resumestate.CheckpointRuntimeFile{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	return resumable, nil
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

func outputBindingForRuntimeState(
	sessionID transfer.OutputSessionID,
	descriptor content.FileRevisionDescriptor,
	record resumestate.CheckpointRuntimeState,
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
	resumable resumestate.CheckpointRuntimeFile,
) (transfer.FileStart, error) {
	record := resumable.BoundState().State()
	binding, err := outputBindingForRuntimeState(session.SessionID(), resumable.Descriptor(), record)
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
		transaction.resumable.BoundState().State().Phase() != resumestate.CheckpointRuntimeWitnessed {
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
	record := transaction.resumable.BoundState().State()
	if rangeSetIntersects(record.DurableRanges(), offset, end) || rangeSetIntersects(transaction.pending, offset, end) {
		// A verified checkpoint remains authoritative across process restart. Any
		// later overwrite would let that old record authenticate different bytes;
		// pending writes are equally single-owner because the protocol has no
		// idempotent rewrite proof.
		return outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, errNativeRangeOverlap)
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
