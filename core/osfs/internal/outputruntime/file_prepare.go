package outputruntime

import (
	"errors"
	"io/fs"
	"sync"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

type FileTransaction struct {
	session *Session

	mu         sync.Mutex
	descriptor content.FileRevisionDescriptor
	resumable  resumestate.ResumableFileAuthority
	binding    transfer.OutputFileBinding
	recordDir  outputcap.Directory
	recordName string
	anchorDir  outputcap.Directory
	anchorName string
	stageDir   outputcap.Directory
	stageName  string
	anchor     anchorWitness
	data       stagedData
	pending    content.RangeSet
	lifecycle  fileTransactionLifecycle
	reduceFile fileReducer
}

var errOutputV3InternalCleanupNeedsAttention = errors.New(
	"osfs: verified output cleanup needs attention",
)

type fileTransactionLifecycle uint8

const (
	FileTransactionOpen fileTransactionLifecycle = iota + 1
	FileTransactionSettling
	FileTransactionClosed
)

var errOutputV3RangeOverlap = errors.New("osfs: output write overlaps transaction-owned data")

func (session *Session) observeStage(
	record resumestate.FileRecord,
	anchor outputcap.File,
	anchorObservation resumestate.AnchorObservation,
) (outputcap.File, outputcap.Directory, resumestate.EntryObservation, error) {
	name := resumestate.StageName(record.OutputObject())
	directory, present, err := openOutputShard(session.stagesDir, name.Shard(), false)
	if err != nil {
		if classifyOutputV3RecoveryFailure(err, outputV3BeforeEntryEvidence) == outputV3RecoveryAmbiguous {
			return nil, nil, resumestate.EntryUnsafe, nil
		}
		return nil, nil, 0, err
	}
	if !present {
		return nil, nil, resumestate.EntryMissing, nil
	}
	kind, err := directory.ObserveEntry(name.Name())
	if err != nil {
		if classifyOutputV3RecoveryFailure(err, outputV3BeforeEntryEvidence) == outputV3RecoveryAmbiguous {
			return nil, directory, resumestate.EntryUnsafe, nil
		}
		return nil, directory, 0, err
	}
	if kind == outputcap.EntryAbsent {
		return nil, directory, resumestate.EntryMissing, nil
	}
	if kind != outputcap.EntryRegularFile {
		if anchorObservation == resumestate.AnchorVerified {
			return nil, directory, resumestate.EntryDifferentFromAnchor, nil
		}
		return nil, directory, resumestate.EntryPresentUnresolved, nil
	}
	stage, err := directory.OpenFile(name.Name(), true, true)
	if err != nil {
		return stage, directory, resumestate.EntryUnsafe, nil
	}
	if anchorObservation != resumestate.AnchorVerified {
		return stage, directory, resumestate.EntryPresentUnresolved, nil
	}
	same, err := stage.SameFile(anchor)
	if err != nil {
		return stage, directory, resumestate.EntryUnsafe, nil
	}
	if !same {
		return stage, directory, resumestate.EntryDifferentFromAnchor, nil
	}
	return stage, directory, resumestate.EntrySameAsAnchor, nil
}

func (session *Session) installFileRecord(
	directory outputcap.Directory,
	name string,
	previous resumestate.BoundFileRecord,
	next resumestate.BoundFileRecord,
) error {
	session.stateInstall.Lock()
	defer session.stateInstall.Unlock()
	if session.stateWritesDisabled() {
		return pauseRequiredFileOutputFault(outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultOwnership, outputfault.ErrSessionClosed,
		))
	}
	if previous.Record().LocatorDigest() != next.Record().LocatorDigest() ||
		next.Record().StateGeneration() <= previous.Record().StateGeneration() {
		return outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, resumestate.ErrInvalidTransition)
	}
	currentEncoded, err := resumestate.EncodeFileRecord(previous)
	if err != nil {
		return outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	nextEncoded, err := resumestate.EncodeFileRecord(next)
	if err != nil {
		return outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	outcome, replaceErr := session.store.ReplaceRecord(
		directory,
		name,
		outputnamespace.NewRecordImage(currentEncoded, previous.Record().StateGeneration()),
		outputnamespace.NewRecordImage(nextEncoded, next.Record().StateGeneration()),
		resumestate.MaxFileStateBytes,
	)
	var resultErr error
	switch outcome {
	case outputnamespace.ReplaceAdopted:
		if replaceErr != nil {
			// The durable record is already the next generation, but the current
			// owner cannot safely continue with handles whose close/cleanup status is
			// unknown. A fresh process can adopt the exact installed generation.
			session.poisonState()
			resultErr = pauseRequiredFileOutputFault(fileOutputFault("finish adopted file-state replacement", replaceErr))
		}
	case outputnamespace.ReplaceUnchanged:
		if replaceErr == nil {
			replaceErr = outputcap.ErrUnsafeNamespace
		}
		return pauseRequiredFileOutputFault(fileOutputFault("replace file state", replaceErr))
	case outputnamespace.ReplaceUncertain:
		session.poisonState()
		return pauseRequiredFileOutputFault(fileOutputFault(
			"replace file state with uncertain authority", errors.Join(outputcap.ErrUnsafeNamespace, replaceErr),
		))
	default:
		session.poisonState()
		return pauseRequiredFileOutputFault(outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultContract, resumestate.ErrInvalidState,
		))
	}
	session.owner.trace(FilesystemOutputTrace{
		Operation: TraceFilePhaseTransition, ResumeIntent: session.resumeIntent,
		SessionID: session.SessionID(), LocatorDigest: outputLocatorDigestFromState(next.Record().LocatorDigest()),
		OutputObjectID: outputObjectIdentityFromState(next.Record().OutputObject()),
		PreviousPhase:  filesystemOutputFilePhaseFromState(previous.Record().Phase()),
		NextPhase:      filesystemOutputFilePhaseFromState(next.Record().Phase()),
	})
	return resultErr
}

func (session *Session) ensureInitialFileRecord(
	directory outputcap.Directory,
	name string,
	encoded []byte,
) (outputnamespace.CreateOutcome, error) {
	session.stateInstall.Lock()
	defer session.stateInstall.Unlock()
	if session.stateWritesDisabled() {
		return outputnamespace.CreateNotInstalled, outputfault.ErrSessionClosed
	}
	return session.store.EnsureInitialRecord(directory, name, encoded, resumestate.MaxFileStateBytes)
}

func (session *Session) transactionStart(
	descriptor content.FileRevisionDescriptor,
	resumable resumestate.ResumableFileAuthority,
	recordDir outputcap.Directory,
	recordName string,
) (resultStart transfer.FileStart, resultErr error) {
	record := resumable.Bound().Record()
	stageName := resumestate.StageName(record.OutputObject())
	anchorName := resumestate.AnchorName(record.OutputObject())
	var stageDir, anchorDir outputcap.Directory
	var dataFile, anchorFile outputcap.File
	cleanupOwned := true
	defer func() {
		if !cleanupOwned {
			return
		}
		if closeErr := errors.Join(
			closeOutputV3File(dataFile), closeOutputV3File(anchorFile),
			closeOutputV3Directory(stageDir), closeOutputV3Directory(anchorDir),
		); closeErr != nil {
			resultStart = transfer.FileStart{}
			resultErr = pauseRequiredFileOutputFault(fileOutputFault(
				"close incomplete file transaction", errors.Join(resultErr, closeErr),
			))
		}
	}()
	quarantine := func(reason resumestate.QuarantineReason) (transfer.FileStart, error) {
		target, err := outputTargetForDescriptor(session.SessionID(), descriptor, record.CanonicalLocator())
		if err != nil {
			return transfer.FileStart{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
		}
		quarantined, err := session.installUnsafeNamespaceQuarantine(
			recordDir, recordName, resumable.Bound(), reason,
		)
		if err != nil {
			return transfer.FileStart{}, err
		}
		return session.quarantinedStart(
			target, quarantined.Record().LocatorDigest(), mapQuarantineReason(quarantined.Record().QuarantineReason()),
		)
	}

	stageDir, present, err := openOutputShard(session.stagesDir, stageName.Shard(), false)
	if err != nil || !present {
		return quarantine(resumestate.QuarantineStageUnsafe)
	}
	anchorDir, present, err = openOutputShard(session.anchorsDir, anchorName.Shard(), false)
	if err != nil || !present {
		return quarantine(resumestate.QuarantineAnchorUnsafe)
	}
	dataFile, err = stageDir.OpenFile(stageName.Name(), true, true)
	if err != nil {
		return quarantine(resumestate.QuarantineStageUnsafe)
	}
	anchorFile, err = anchorDir.OpenFile(anchorName.Name(), true, false)
	if err != nil {
		return quarantine(resumestate.QuarantineAnchorUnsafe)
	}
	data := stagedData{file: dataFile}
	anchor := anchorWitness{file: anchorFile}
	same, err := data.SameFile(anchor)
	if err != nil || !same {
		return quarantine(resumestate.QuarantineStageUnsafe)
	}
	binding, err := outputBindingForRecord(session.SessionID(), descriptor, record)
	if err != nil {
		return transfer.FileStart{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	durable, err := transfer.VerifyDurableRanges(
		binding, transfer.CheckpointGeneration(record.CheckpointGeneration()), record.DurableRanges(),
	)
	if err != nil {
		return transfer.FileStart{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	pending, _ := content.NewRangeSet(nil)
	transaction := &FileTransaction{
		session: session, descriptor: descriptor, resumable: resumable, binding: binding,
		recordDir: recordDir, recordName: recordName,
		anchorDir: anchorDir, anchorName: anchorName.Name(), stageDir: stageDir, stageName: stageName.Name(),
		anchor: anchor, data: data, pending: pending,
		lifecycle: FileTransactionOpen, reduceFile: session.reduceFile,
	}
	session.mu.Lock()
	session.active[record.LocatorDigest()] = transaction
	session.mu.Unlock()
	start, err := transfer.NewFileTransactionStart(transaction, durable)
	if err != nil {
		session.finishFile(record.LocatorDigest(), transaction)
		return transfer.FileStart{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	cleanupOwned = false
	return start, nil
}

func (session *Session) collisionStart(file transfer.OutputFile) (transfer.FileStart, error) {
	settlement, err := transfer.NewCollisionFileSettlement(file.Target)
	if err != nil {
		return transfer.FileStart{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	return transfer.NewFileSettlementStart(settlement)
}

func (session *Session) quarantinedStart(
	target transfer.OutputFileTarget,
	digest resumestate.LocatorDigest,
	reason transfer.QuarantineReason,
) (transfer.FileStart, error) {
	reference, err := transfer.NewOutputStateRef(session.SessionID(), digest.OutputLocatorDigest())
	if err != nil {
		return transfer.FileStart{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	settlement, err := transfer.NewImmediateQuarantinedFileSettlement(target, reference, reason)
	if err != nil {
		return transfer.FileStart{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	return transfer.NewFileSettlementStart(settlement)
}

func (session *Session) quarantineRecoveryStart(
	file transfer.OutputFile,
	recordDir outputcap.Directory,
	recordName string,
	bound resumestate.BoundFileRecord,
	reason resumestate.QuarantineReason,
) (transfer.FileStart, error) {
	quarantined, err := session.installUnsafeNamespaceQuarantine(recordDir, recordName, bound, reason)
	if err != nil {
		return transfer.FileStart{}, err
	}
	return session.quarantinedStart(
		file.Target, quarantined.Record().LocatorDigest(), mapQuarantineReason(quarantined.Record().QuarantineReason()),
	)
}

func (session *Session) quarantineRecoveryStartWithCleanup(
	file transfer.OutputFile,
	recordDir outputcap.Directory,
	recordName string,
	bound resumestate.BoundFileRecord,
	reason resumestate.QuarantineReason,
	cleanupOperation string,
	cleanupErr error,
) (transfer.FileStart, error) {
	start, quarantineErr := session.quarantineRecoveryStart(
		file, recordDir, recordName, bound, reason,
	)
	if quarantineErr != nil {
		if cleanupErr != nil {
			quarantineErr = errors.Join(
				quarantineErr,
				pauseRequiredFileOperationFault(cleanupOperation, nil, cleanupErr),
			)
		}
		return transfer.FileStart{}, quarantineErr
	}
	if cleanupErr != nil {
		return transfer.FileStart{}, pauseRequiredFileOperationFault(cleanupOperation, nil, cleanupErr)
	}
	return start, nil
}

func (session *Session) installUnsafeNamespaceQuarantine(
	recordDir outputcap.Directory,
	recordName string,
	bound resumestate.BoundFileRecord,
	reason resumestate.QuarantineReason,
) (resumestate.BoundFileRecord, error) {
	quarantined, err := resumestate.PrepareUnsafeNamespaceQuarantine(bound, reason)
	if err != nil {
		return resumestate.BoundFileRecord{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if err := session.installFileRecord(recordDir, recordName, bound, quarantined); err != nil {
		// The original ambiguity remains unresolved until a fresh owner reopens
		// this exact state record, so even a safely unchanged CAS must pause the job.
		return resumestate.BoundFileRecord{}, pauseRequiredFileOutputFault(err)
	}
	return quarantined, nil
}

func (session *Session) openPublicationWitness(
	record resumestate.FileRecord,
	expected anchorWitness,
) (*publicationWitness, error, error) {
	stageName := resumestate.StageName(record.OutputObject())
	stageDir, present, err := openOutputShard(session.stagesDir, stageName.Shard(), false)
	if err != nil {
		return nil, err, closeOutputV3Directory(stageDir)
	}
	if !present {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, fs.ErrNotExist), closeOutputV3Directory(stageDir)
	}
	anchorName := resumestate.AnchorName(record.OutputObject())
	anchorDir, present, err := openOutputShard(session.anchorsDir, anchorName.Shard(), false)
	if err != nil {
		return nil, err, errors.Join(stageDir.Close(), closeOutputV3Directory(anchorDir))
	}
	if !present {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, fs.ErrNotExist),
			errors.Join(stageDir.Close(), closeOutputV3Directory(anchorDir))
	}
	witness, operationErr, witnessCleanupErr := openPublicationWitnessInDirectoriesResult(
		record, stageDir, anchorDir, expected,
	)
	cleanupErr := errors.Join(witnessCleanupErr, stageDir.Close(), anchorDir.Close())
	if operationErr != nil || cleanupErr != nil {
		if witness != nil {
			cleanupErr = errors.Join(cleanupErr, witness.Close())
		}
		return nil, operationErr, cleanupErr
	}
	return witness, nil, nil
}

func openPublicationWitnessInDirectories(
	record resumestate.FileRecord,
	stageDir outputcap.Directory,
	anchorDir outputcap.Directory,
	expected anchorWitness,
) (*publicationWitness, error) {
	witness, operationErr, cleanupErr := openPublicationWitnessInDirectoriesResult(
		record, stageDir, anchorDir, expected,
	)
	return witness, errors.Join(operationErr, cleanupErr)
}

func openPublicationWitnessInDirectoriesResult(
	record resumestate.FileRecord,
	stageDir outputcap.Directory,
	anchorDir outputcap.Directory,
	expected anchorWitness,
) (*publicationWitness, error, error) {
	if stageDir == nil || anchorDir == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("publication witness directory is absent")), nil
	}
	stageName := resumestate.StageName(record.OutputObject())
	stage, err := stageDir.OpenFile(stageName.Name(), true, false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, outputcap.ErrUnsafeNamespace) {
			err = errors.Join(outputcap.ErrUnsafeNamespace, err)
		}
		return nil, err, closeOutputV3File(stage)
	}
	witness := &publicationWitness{stage: stagedData{file: stage}}
	fail := func(cause error, unsafe bool) (*publicationWitness, error, error) {
		if unsafe {
			cause = errors.Join(outputcap.ErrUnsafeNamespace, cause)
		}
		return nil, cause, witness.Close()
	}
	anchorName := resumestate.AnchorName(record.OutputObject())
	anchor, err := anchorDir.OpenFile(anchorName.Name(), true, false)
	if err != nil {
		return fail(err, errors.Is(err, fs.ErrNotExist) || errors.Is(err, outputcap.ErrUnsafeNamespace))
	}
	witness.anchor = anchorWitness{file: anchor}
	stageSize, stageErr := stage.Size()
	anchorSize, anchorErr := anchor.Size()
	if stageErr != nil || anchorErr != nil {
		return fail(
			errors.Join(stageErr, anchorErr),
			errors.Is(stageErr, outputcap.ErrUnsafeNamespace) || errors.Is(anchorErr, outputcap.ErrUnsafeNamespace),
		)
	}
	if stageSize != record.ExactSize() || anchorSize != record.ExactSize() {
		return fail(errors.New("publication stage or anchor size differs from its record"), true)
	}
	same, sameErr := stage.SameFile(anchor)
	if sameErr != nil {
		return fail(sameErr, errors.Is(sameErr, outputcap.ErrUnsafeNamespace))
	}
	if !same {
		return fail(errors.New("publication stage and anchor identify different objects"), true)
	}
	if expected.valid() {
		stageMatches, stageMatchErr := stage.SameFile(expected.file)
		anchorMatches, anchorMatchErr := anchor.SameFile(expected.file)
		if stageMatchErr != nil || anchorMatchErr != nil {
			return fail(
				errors.Join(stageMatchErr, anchorMatchErr),
				errors.Is(stageMatchErr, outputcap.ErrUnsafeNamespace) || errors.Is(anchorMatchErr, outputcap.ErrUnsafeNamespace),
			)
		}
		if !stageMatches || !anchorMatches {
			return fail(errors.New("publication names no longer identify the retained witness"), true)
		}
	}
	metadataMatches, metadataErr := anchor.MetadataMatches(
		record.ExactSize(), record.ExpectedMetadata().ModifiedTime,
	)
	if metadataErr != nil {
		return fail(metadataErr, errors.Is(metadataErr, outputcap.ErrUnsafeNamespace))
	}
	if !metadataMatches {
		return fail(errors.New("publication witness metadata differs from its record"), true)
	}
	return witness, nil, nil
}

func (transaction *FileTransaction) Binding() transfer.OutputFileBinding {
	if transaction == nil {
		return transfer.OutputFileBinding{}
	}
	return transaction.binding
}

func rangeSetIntersects(ranges content.RangeSet, offset, end uint64) bool {
	for _, current := range ranges.Ranges() {
		if current.Offset >= end {
			return false
		}
		if current.End > offset {
			return true
		}
	}
	return false
}

func (transaction *FileTransaction) preparePublication() (
	transfer.FileSettlement,
	bool,
	error,
) {
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	return transaction.preparePublicationLocked()
}

func (transaction *FileTransaction) preparePublicationLocked() (
	transfer.FileSettlement,
	bool,
	error,
) {
	if transaction.lifecycle != FileTransactionSettling || transaction.session.operationDisabled() {
		return transfer.FileSettlement{}, false, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultOwnership, outputfault.ErrSessionClosed,
		)
	}
	checkpoint, err := transaction.checkpointLocked()
	if err != nil {
		return transfer.FileSettlement{}, false, err
	}
	if !transfer.RangesCoverFile(transaction.binding.ExactSize(), checkpoint.Ranges()) {
		return transfer.FileSettlement{}, false, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultContract, transfer.ErrIncompleteOutputFile,
		)
	}
	metadataErr := transaction.data.SetModifiedTime(transaction.descriptor.ModifiedTime())
	if metadataErr == nil {
		metadataErr = transaction.data.Sync()
	}
	if metadataErr != nil {
		return transfer.FileSettlement{}, false, fileOutputFault("install output metadata", metadataErr)
	}
	same, sameErr := transaction.data.SameFile(transaction.anchor)
	if sameErr != nil {
		if !errors.Is(sameErr, outputcap.ErrUnsafeNamespace) {
			return transfer.FileSettlement{}, false, pauseRequiredFileOutputFault(fileOutputFault(
				"compare publication witness", sameErr,
			))
		}
		settlement, quarantineErr := transaction.installWitnessQuarantineLocked(resumestate.QuarantineStageUnsafe)
		return settlement, quarantineErr == nil, quarantineErr
	}
	if !same {
		settlement, quarantineErr := transaction.installWitnessQuarantineLocked(resumestate.QuarantineStageUnsafe)
		return settlement, quarantineErr == nil, quarantineErr
	}
	witness, witnessErr, witnessCleanupErr := openPublicationWitnessInDirectoriesResult(
		transaction.resumable.Bound().Record(), transaction.stageDir, transaction.anchorDir, transaction.anchor,
	)
	if witnessErr == nil && witness != nil {
		witnessCleanupErr = errors.Join(witnessCleanupErr, witness.Close())
	}
	if witnessErr != nil {
		if errors.Is(witnessErr, outputcap.ErrUnsafeNamespace) {
			return transaction.installWitnessQuarantineWithCleanupLocked(
				resumestate.QuarantinePublicationHistory,
				"close quarantined publication witness recheck",
				witnessCleanupErr,
			)
		}
		return transfer.FileSettlement{}, false, pauseRequiredFileOperationFault(
			"reverify publication witness names", witnessErr, witnessCleanupErr,
		)
	}
	if witnessCleanupErr != nil {
		return transfer.FileSettlement{}, false, pauseRequiredFileOperationFault(
			"close reverified publication witness names", nil, witnessCleanupErr,
		)
	}
	publishing, err := resumestate.PreparePublication(transaction.resumable)
	if err != nil {
		return transfer.FileSettlement{}, false, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if err := transaction.session.installFileRecord(
		transaction.recordDir, transaction.recordName, transaction.resumable.Bound(), publishing,
	); err != nil {
		return transfer.FileSettlement{}, false, err
	}
	transaction.resumable, err = resumestate.BindResumableFile(publishing, transaction.descriptor)
	if err != nil {
		return transfer.FileSettlement{}, false, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	return transfer.FileSettlement{}, false, nil
}
