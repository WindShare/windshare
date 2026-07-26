package osfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"sync"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

type filesystemFileTransaction struct {
	session *filesystemOutputSession

	mu         sync.Mutex
	descriptor content.FileRevisionDescriptor
	resumable  resumestate.ResumableFileAuthority
	binding    transfer.OutputFileBinding
	recordDir  outputV3Directory
	recordName string
	anchorDir  outputV3Directory
	anchorName string
	stageDir   outputV3Directory
	stageName  string
	anchor     outputV3File
	data       outputV3File
	pending    content.RangeSet
	lifecycle  filesystemFileTransactionLifecycle
	reduceFile filesystemFileReducer
}

var errOutputV3InternalCleanupNeedsAttention = errors.New(
	"osfs: verified output cleanup needs attention",
)

type filesystemFileTransactionLifecycle uint8

const (
	filesystemFileTransactionOpen filesystemFileTransactionLifecycle = iota + 1
	filesystemFileTransactionSettling
	filesystemFileTransactionClosed
)

type filesystemFileReducer func(
	context.Context,
	transfer.OutputFile,
	resumestate.ResumableFileAuthority,
	outputV3Directory,
	string,
) (transfer.FileStart, error)

type outputPublicationWitness struct {
	stage  outputV3File
	anchor outputV3File
}

func (witness *outputPublicationWitness) Close() error {
	if witness == nil {
		return nil
	}
	var result error
	if witness.stage != nil {
		result = errors.Join(result, witness.stage.Close())
		witness.stage = nil
	}
	if witness.anchor != nil {
		result = errors.Join(result, witness.anchor.Close())
		witness.anchor = nil
	}
	return result
}

var errOutputV3RangeOverlap = errors.New("osfs: output write overlaps transaction-owned data")

func (session *filesystemOutputSession) FinalizeDirectory(
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
		return outputFault(transfer.OutputFaultSession, transfer.OutputFaultContract, transfer.ErrInvalidOutputSelection)
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

func (session *filesystemOutputSession) BeginFile(
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
	target, targetErr := outputTargetForDescriptor(session.SessionID(), file.Descriptor, file.Path)
	selected, found := session.selectedFiles[file.Path]
	if !found || file.ExpectedSize != selected.ExpectedSize ||
		file.Descriptor.ShareInstance() != session.selection.ShareInstance() ||
		file.Descriptor.FileID() != selected.FileID || file.Descriptor.ExactSize() != selected.ExpectedSize ||
		file.Descriptor.ModifiedTime() != selected.ModifiedTime || file.Descriptor.FileRevision().IsZero() ||
		targetErr != nil || file.Target != target {
		return transfer.FileStart{}, outputFault(
			transfer.OutputFaultFile, transfer.OutputFaultContract, transfer.ErrInvalidOutputSelection,
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
	if err := session.claimFileStart(digest); err != nil {
		return transfer.FileStart{}, err
	}
	defer session.releaseFileStart(digest)
	session.mu.Lock()
	_, duplicateObject := session.duplicateObjects[digest]
	session.mu.Unlock()
	if duplicateObject {
		return session.quarantinedStart(file.Target, digest, transfer.QuarantineStateCorrupt)
	}

	recordName := resumestate.FileRecordName(digest)
	recordDir, recordShardPresent, err := openOutputShard(session.filesDir, recordName.Shard(), false)
	if err != nil {
		if classifyOutputV3RecoveryFailure(err, outputV3BeforeEntryEvidence) == outputV3RecoveryAmbiguous {
			return session.quarantinedStart(file.Target, digest, transfer.QuarantineStateCorrupt)
		}
		return transfer.FileStart{}, recoveryFileOutputFault(
			"open file-state shard", err, outputV3BeforeEntryEvidence,
		)
	}
	recordOwned := recordDir != nil
	defer func() {
		if !recordOwned {
			return
		}
		if closeErr := closeOutputV3Directory(recordDir); closeErr != nil {
			resultStart = transfer.FileStart{}
			resultErr = pauseRequiredFileOutputFault(fileOutputFault(
				"close file-state shard after begin", errors.Join(resultErr, closeErr),
			))
		}
	}()
	if recordShardPresent {
		start, handled, beginErr := session.gateFileStateShard(ctx, file, recordDir, recordName)
		if beginErr != nil || handled {
			_, _, transactionStarted := start.Transaction()
			if beginErr == nil && transactionStarted {
				recordOwned = false
			}
			return start, beginErr
		}
	}

	parent, err := ancestryValidation.directory(parentPath)
	if err != nil {
		return transfer.FileStart{}, outputAncestryOperationFault("retain final parent before begin", err)
	}
	_, leaf, err := outputLocatorParentAndLeaf(file.Path)
	if err != nil {
		return transfer.FileStart{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if err := ancestryValidation.revalidateRetainedDirectory(parentPath, outputAncestryCreateAuthority); err != nil {
		session.traceOutputAncestry(FilesystemOutputAncestryBeginFile, digest, err)
		return transfer.FileStart{}, outputAncestryOperationFault("revalidate final parent before begin", err)
	}
	finalKind, observeErr := parent.ObserveEntry(leaf)
	if observeErr != nil {
		fault := fileOutputFault("observe final before begin", observeErr)
		if errors.Is(observeErr, fs.ErrPermission) {
			fault = pauseRequiredFileOutputFault(fault)
		}
		return transfer.FileStart{}, fault
	}
	if finalKind != outputV3EntryAbsent {
		return session.collisionStart(file)
	}

	if !recordShardPresent {
		recordDir, _, err = openOutputShard(session.filesDir, recordName.Shard(), true)
		if err != nil {
			fault := fileOutputFault("create file-state shard", err)
			if errors.Is(err, fs.ErrPermission) {
				fault = pauseRequiredFileOutputFault(fault)
			}
			return transfer.FileStart{}, fault
		}
		recordOwned = true
	}

	objectID, err := session.allocateOutputObjectID(digest)
	if err != nil {
		return transfer.FileStart{}, fileOutputFault("allocate output object", err)
	}
	resumable, err := resumestate.NewFileRecord(resumestate.FileRecordSpec{
		Session: state, Descriptor: file.Descriptor, CanonicalLocator: file.Path, OutputObject: objectID,
	})
	if err != nil {
		return transfer.FileStart{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	encoded, err := resumestate.EncodeFileRecord(resumable.Bound())
	if err != nil {
		return transfer.FileStart{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	installOutcome, installErr := session.ensureInitialFileRecord(recordDir, recordName.Name(), encoded)
	switch installOutcome {
	case outputStateCreateAdopted:
		if installErr != nil {
			session.poisonState()
			return transfer.FileStart{}, pauseRequiredFileOutputFault(fileOutputFault(
				"finish adopted reserved file state", installErr,
			))
		}
	case outputStateCreateNotInstalled:
		if installErr == nil {
			installErr = errOutputV3Unsafe
		}
		return transfer.FileStart{}, pauseRequiredFileOutputFault(fileOutputFault(
			"install reserved file state", installErr,
		))
	case outputStateCreateUncertain:
		session.poisonState()
		// State-install ambiguity invalidates this owner, but it is still an I/O
		// settlement failure rather than evidence that a different owner exists.
		// Keeping that distinction lets the job pause and hand recovery to a fresh
		// opener without reporting a false ownership conflict.
		return transfer.FileStart{}, pauseRequiredFileOutputFault(outputFault(
			transfer.OutputFaultFile,
			transfer.OutputFaultStateIO,
			fmt.Errorf(
				"install reserved file state with uncertain authority: %w",
				errors.Join(errOutputV3Unsafe, installErr),
			),
		))
	default:
		session.poisonState()
		return transfer.FileStart{}, pauseRequiredFileOutputFault(outputFault(
			transfer.OutputFaultFile, transfer.OutputFaultContract, resumestate.ErrInvalidState,
		))
	}
	start, err := session.reduceFile(ctx, file, resumable, recordDir, recordName.Name())
	if err == nil {
		_, _, transactionStarted := start.Transaction()
		recordOwned = !transactionStarted
	}
	return start, err
}

func finishOutputAncestryOperation(
	session *filesystemOutputSession,
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
	session *filesystemOutputSession,
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
		owned, ok := transaction.(*filesystemFileTransaction)
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

func (session *filesystemOutputSession) gateFileStateShard(
	ctx context.Context,
	file transfer.OutputFile,
	shard outputV3Directory,
	recordName resumestate.ShardedName,
) (transfer.FileStart, bool, error) {
	names, err := shard.Names(outputFileShardInspectionLimit)
	if err != nil {
		return transfer.FileStart{}, false, recoveryFileOutputFault(
			"inspect file-state shard", err, outputV3BeforeEntryEvidence,
		)
	}
	digest := resumestate.DigestCanonicalLocator(file.Path)
	quarantine := func() (transfer.FileStart, bool, error) {
		start, err := session.quarantinedStart(file.Target, digest, transfer.QuarantineStateCorrupt)
		return start, true, err
	}
	targetKind := outputV3EntryAbsent
	var temporaries []string
	for _, name := range names {
		classified := resumestate.ClassifyFileShardEntry(recordName.Shard(), name)
		switch classified.Classification() {
		case resumestate.FileShardEntryRecord:
			if classified.Locator() == digest {
				if name != recordName.Name() || targetKind != outputV3EntryAbsent {
					return quarantine()
				}
				targetKind, err = shard.ObserveEntry(name)
				if err != nil {
					return quarantine()
				}
			}
		case resumestate.FileShardEntryUpdateTemporary:
			if classified.Locator() == digest {
				temporaries = append(temporaries, name)
			}
		case resumestate.FileShardEntryMalformedForLocator:
			if classified.Locator() == digest {
				return quarantine()
			}
		}
	}
	if targetKind != outputV3EntryAbsent && targetKind != outputV3EntryRegularFile {
		return quarantine()
	}
	if targetKind == outputV3EntryAbsent {
		if len(temporaries) != 0 {
			return quarantine()
		}
		return transfer.FileStart{}, false, nil
	}

	bound, recordCloseErr, err := session.openBoundFileRecord(shard, recordName)
	if err != nil {
		return quarantine()
	}
	if recordCloseErr != nil {
		return transfer.FileStart{}, false, pauseRequiredFileOutputFault(fileOutputFault(
			"close bound file-state record", recordCloseErr,
		))
	}
	for _, name := range temporaries {
		classified := resumestate.ClassifyFileShardEntry(recordName.Shard(), name)
		kind, observeErr := shard.ObserveEntry(name)
		if observeErr != nil {
			start, quarantineErr := session.quarantineRecoveryStart(
				file, shard, recordName.Name(), bound, resumestate.QuarantineUpdateTemporary,
			)
			return start, true, quarantineErr
		}
		entry := resumestate.UpdateTemporaryEntryUnsafe
		if kind == outputV3EntryAbsent {
			entry = resumestate.UpdateTemporaryEntryMissing
		} else if kind == outputV3EntryRegularFile {
			entry = resumestate.UpdateTemporaryEntryRegular
		}
		decision, err := resumestate.ReduceUpdateTemporary(
			session.stateSnapshot().NamespaceAuthority(), classified, entry, resumestate.UpdateTargetValid,
		)
		if err != nil {
			return transfer.FileStart{}, false, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
		}
		switch decision.Action() {
		case resumestate.UpdateTemporaryAcceptInstalledTarget:
			continue
		case resumestate.UpdateTemporaryRemoveAndSyncShard:
			temporary, err := shard.OpenFile(name, true, false)
			if err != nil {
				// Names and ObserveEntry already established that this exact temporary
				// exists. Losing the ability to classify it through a handle is therefore
				// namespace ambiguity, not a retryable lack of mutation authority.
				start, quarantineErr := session.quarantineRecoveryStart(
					file, shard, recordName.Name(), bound, resumestate.QuarantineUpdateTemporary,
				)
				closeErr := closeOutputV3File(temporary)
				if quarantineErr != nil {
					if closeErr != nil {
						quarantineErr = pauseRequiredFileOutputFault(fileOutputFault(
							"close ambiguous state update temporary", errors.Join(quarantineErr, err, closeErr),
						))
					}
					return transfer.FileStart{}, true, quarantineErr
				}
				if closeErr != nil {
					return transfer.FileStart{}, true, pauseRequiredFileOutputFault(fileOutputFault(
						"close quarantined state update temporary", errors.Join(err, closeErr),
					))
				}
				return start, true, quarantineErr
			}
			if err := decision.AuthorizeRemoval(
				bound, recordName.Shard(), name, resumestate.UpdateTemporaryEntryRegular,
			); err != nil {
				closeErr := temporary.Close()
				if closeErr != nil {
					return transfer.FileStart{}, false, pauseRequiredFileOutputFault(fileOutputFault(
						"close unauthorized state update temporary", errors.Join(err, closeErr),
					))
				}
				return transfer.FileStart{}, false, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
			}
			if removeErr := shard.RemoveFile(name, temporary); removeErr != nil {
				closeErr := temporary.Close()
				if classifyOutputV3RecoveryFailure(
					removeErr, outputV3AuthorizedMutation,
				) == outputV3RecoveryAmbiguous {
					start, quarantineErr := session.quarantineRecoveryStartWithCleanup(
						file, shard, recordName.Name(), bound, resumestate.QuarantineUpdateTemporary,
						"close quarantined state update removal", closeErr,
					)
					return start, true, quarantineErr
				}
				return transfer.FileStart{}, false, pauseRequiredFileOperationFault(
					"remove state update temporary", removeErr, closeErr,
				)
			}
			syncErr := shard.Sync()
			closeErr := temporary.Close()
			if syncErr != nil {
				if classifyOutputV3RecoveryFailure(
					syncErr, outputV3AuthorizedMutation,
				) == outputV3RecoveryAmbiguous {
					start, quarantineErr := session.quarantineRecoveryStartWithCleanup(
						file, shard, recordName.Name(), bound, resumestate.QuarantineUpdateTemporary,
						"close quarantined state update sync", closeErr,
					)
					return start, true, quarantineErr
				}
				return transfer.FileStart{}, false, pauseRequiredFileOperationFault(
					"sync state update temporary", syncErr, closeErr,
				)
			}
			if closeErr != nil {
				return transfer.FileStart{}, false, pauseRequiredFileOperationFault(
					"close synced state update temporary", nil, closeErr,
				)
			}
		case resumestate.UpdateTemporaryInstallFileQuarantine:
			quarantined, err := resumestate.ApplyUpdateTemporaryQuarantine(bound, decision)
			if err != nil {
				return transfer.FileStart{}, false, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
			}
			if quarantined.Record().StateGeneration() != bound.Record().StateGeneration() {
				if err := session.installFileRecord(shard, recordName.Name(), bound, quarantined); err != nil {
					return transfer.FileStart{}, false, err
				}
			}
			return quarantine()
		default:
			return quarantine()
		}
	}
	return session.resumeFile(ctx, file, shard, recordName.Name(), bound)
}

func (session *filesystemOutputSession) openBoundFileRecord(
	shard outputV3Directory,
	name resumestate.ShardedName,
) (resumestate.BoundFileRecord, error, error) {
	encoded, readErr, closeErr := readStateRecordWithCleanup(shard, name.Name(), resumestate.MaxFileStateBytes)
	if readErr != nil {
		return resumestate.BoundFileRecord{}, closeErr, readErr
	}
	record, err := resumestate.DecodeFileRecord(encoded)
	if err != nil {
		return resumestate.BoundFileRecord{}, closeErr, errors.Join(errOutputIntentUnsafe, err)
	}
	if record.LocatorDigest() != resumestate.DigestCanonicalLocator(record.CanonicalLocator()) {
		return resumestate.BoundFileRecord{}, closeErr, errOutputIntentUnsafe
	}
	bound, bindErr := resumestate.BindFileRecord(session.stateSnapshot(), name.Shard(), name.Name(), record)
	return bound, closeErr, bindErr
}

func (session *filesystemOutputSession) reconcileFileShardUpdates(
	shardName string,
	shard outputV3Directory,
	names []string,
) (resultAttention bool, resultErr error) {
	requirement := outputAncestryRequirement{}
	validation, err := session.validateOutputAncestry(requirement)
	if err != nil {
		session.traceOutputAncestry(FilesystemOutputAncestryRecovery, resumestate.LocatorDigest{}, err)
		return false, outputAncestryOperationFault("validate ancestry before file-state recovery", err)
	}
	defer func() {
		ancestryErr := finishOutputAncestryOperation(
			session, validation, requirement, FilesystemOutputAncestryRecovery,
			resumestate.LocatorDigest{}, "finish file-state recovery ancestry", nil,
		)
		if ancestryErr != nil {
			resultAttention = false
			resultErr = errors.Join(resultErr, ancestryErr)
		}
	}()
	attention := false
	for _, name := range names {
		classified := resumestate.ClassifyFileShardEntry(shardName, name)
		if classified.Classification() == resumestate.FileShardEntryRecord {
			continue
		}
		entryKind, err := shard.ObserveEntry(name)
		if err != nil {
			if classified.Classification() == resumestate.FileShardEntryUpdateTemporary &&
				!classified.Locator().IsZero() {
				targetName := resumestate.FileRecordName(classified.Locator())
				targetKind, targetErr := shard.ObserveEntry(targetName.Name())
				if targetErr == nil && targetKind == outputV3EntryRegularFile {
					bound, recordCloseErr, bindErr := session.openBoundFileRecord(shard, targetName)
					if bindErr == nil {
						if _, quarantineErr := session.installUnsafeNamespaceQuarantine(
							shard, targetName.Name(), bound, resumestate.QuarantineUpdateTemporary,
						); quarantineErr != nil {
							if recordCloseErr != nil {
								return false, pauseRequiredFileOutputFault(fileOutputFault(
									"close update target after failed entry-race quarantine",
									errors.Join(quarantineErr, recordCloseErr),
								))
							}
							return false, quarantineErr
						}
						if recordCloseErr != nil {
							return false, pauseRequiredFileOutputFault(fileOutputFault(
								"close quarantined update target after entry race", recordCloseErr,
							))
						}
					}
				}
				attention = true
				continue
			}
			return false, fileOutputFault("observe file-shard recovery entry", err)
		}
		entry := resumestate.UpdateTemporaryEntryUnsafe
		if entryKind == outputV3EntryAbsent {
			entry = resumestate.UpdateTemporaryEntryMissing
		} else if entryKind == outputV3EntryRegularFile {
			entry = resumestate.UpdateTemporaryEntryRegular
		}
		targetObservation := resumestate.UpdateTargetMissing
		var bound resumestate.BoundFileRecord
		if !classified.Locator().IsZero() {
			targetName := resumestate.FileRecordName(classified.Locator())
			targetKind, observeErr := shard.ObserveEntry(targetName.Name())
			if observeErr != nil {
				return false, fileOutputFault("observe update target", observeErr)
			}
			if targetKind == outputV3EntryRegularFile {
				var recordCloseErr error
				bound, recordCloseErr, err = session.openBoundFileRecord(shard, targetName)
				if err == nil && recordCloseErr != nil {
					return false, pauseRequiredFileOutputFault(fileOutputFault(
						"close recovered file-state record", recordCloseErr,
					))
				}
				if err == nil {
					targetObservation = resumestate.UpdateTargetValid
				} else {
					targetObservation = resumestate.UpdateTargetInvalid
				}
			} else if targetKind != outputV3EntryAbsent {
				targetObservation = resumestate.UpdateTargetInvalid
			}
		}
		decision, err := resumestate.ReduceUpdateTemporary(
			session.stateSnapshot().NamespaceAuthority(), classified, entry, targetObservation,
		)
		if err != nil {
			return false, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
		}
		switch decision.Action() {
		case resumestate.UpdateTemporaryAcceptInstalledTarget:
			continue
		case resumestate.UpdateTemporaryRemoveAndSyncShard:
			temporary, err := shard.OpenFile(name, true, false)
			if err != nil {
				closeErr := closeOutputV3File(temporary)
				if targetObservation == resumestate.UpdateTargetValid {
					targetName := resumestate.FileRecordName(classified.Locator())
					if _, quarantineErr := session.installUnsafeNamespaceQuarantine(
						shard, targetName.Name(), bound, resumestate.QuarantineUpdateTemporary,
					); quarantineErr != nil {
						if closeErr != nil {
							quarantineErr = pauseRequiredFileOutputFault(fileOutputFault(
								"close ambiguous recovered update temporary", errors.Join(quarantineErr, err, closeErr),
							))
						}
						return false, quarantineErr
					}
					if closeErr != nil {
						return false, pauseRequiredFileOutputFault(fileOutputFault(
							"close quarantined recovered update temporary", errors.Join(err, closeErr),
						))
					}
					attention = true
					continue
				}
				fault := fileOutputFault("open recoverable update temporary", errors.Join(err, closeErr))
				if closeErr != nil {
					fault = pauseRequiredFileOutputFault(fault)
				}
				return false, fault
			}
			if err := decision.AuthorizeRemoval(
				bound, shardName, name, resumestate.UpdateTemporaryEntryRegular,
			); err != nil {
				closeErr := temporary.Close()
				if closeErr != nil {
					return false, pauseRequiredFileOutputFault(fileOutputFault(
						"close unauthorized recoverable update temporary", errors.Join(err, closeErr),
					))
				}
				return false, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
			}
			if removeErr := shard.RemoveFile(name, temporary); removeErr != nil {
				closeErr := temporary.Close()
				if targetObservation == resumestate.UpdateTargetValid && classifyOutputV3RecoveryFailure(
					removeErr, outputV3AuthorizedMutation,
				) == outputV3RecoveryAmbiguous {
					targetName := resumestate.FileRecordName(classified.Locator())
					if _, quarantineErr := session.installUnsafeNamespaceQuarantine(
						shard, targetName.Name(), bound, resumestate.QuarantineUpdateTemporary,
					); quarantineErr != nil {
						if closeErr != nil {
							quarantineErr = errors.Join(
								quarantineErr,
								pauseRequiredFileOperationFault(
									"close quarantined recovered update removal", nil, closeErr,
								),
							)
						}
						return false, quarantineErr
					}
					if closeErr != nil {
						return false, pauseRequiredFileOperationFault(
							"close quarantined recovered update removal", nil, closeErr,
						)
					}
					attention = true
					continue
				}
				return false, pauseRequiredFileOperationFault(
					"remove recoverable update temporary", removeErr, closeErr,
				)
			}
			syncErr := shard.Sync()
			closeErr := temporary.Close()
			if syncErr != nil {
				if targetObservation == resumestate.UpdateTargetValid &&
					classifyOutputV3RecoveryFailure(syncErr, outputV3AuthorizedMutation) == outputV3RecoveryAmbiguous {
					targetName := resumestate.FileRecordName(classified.Locator())
					if _, quarantineErr := session.installUnsafeNamespaceQuarantine(
						shard, targetName.Name(), bound, resumestate.QuarantineUpdateTemporary,
					); quarantineErr != nil {
						if closeErr != nil {
							quarantineErr = errors.Join(
								quarantineErr,
								pauseRequiredFileOperationFault(
									"close quarantined recovered update sync", nil, closeErr,
								),
							)
						}
						return false, quarantineErr
					}
					if closeErr != nil {
						return false, pauseRequiredFileOperationFault(
							"close quarantined recovered update sync", nil, closeErr,
						)
					}
					attention = true
					continue
				}
				return false, pauseRequiredFileOperationFault(
					"sync recoverable update temporary", syncErr, closeErr,
				)
			}
			if closeErr != nil {
				return false, pauseRequiredFileOperationFault(
					"close synced recoverable update temporary", nil, closeErr,
				)
			}
		case resumestate.UpdateTemporaryInstallFileQuarantine:
			quarantined, err := resumestate.ApplyUpdateTemporaryQuarantine(bound, decision)
			if err != nil {
				return false, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
			}
			if quarantined.Record().StateGeneration() != bound.Record().StateGeneration() {
				targetName := resumestate.FileRecordName(classified.Locator())
				if err := session.installFileRecord(shard, targetName.Name(), bound, quarantined); err != nil {
					return false, err
				}
			}
			attention = true
		default:
			attention = true
		}
	}
	return attention, nil
}

func (session *filesystemOutputSession) claimFileStart(digest resumestate.LocatorDigest) error {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed || session.settling || session.poisoned {
		return outputFault(transfer.OutputFaultSession, transfer.OutputFaultOwnership, errOutputSessionClosed)
	}
	if _, exists := session.active[digest]; exists {
		return outputFault(transfer.OutputFaultFile, transfer.OutputFaultOwnership, errOutputFileActive)
	}
	if _, exists := session.beginning[digest]; exists {
		return outputFault(transfer.OutputFaultFile, transfer.OutputFaultOwnership, errOutputFileActive)
	}
	if len(session.active)+len(session.beginning) >= maxFilesystemOutputTransactions {
		return outputFault(transfer.OutputFaultSession, transfer.OutputFaultOwnership, errOutputTransactionLimit)
	}
	session.beginning[digest] = struct{}{}
	session.beginWG.Add(1)
	return nil
}

func (session *filesystemOutputSession) releaseFileStart(digest resumestate.LocatorDigest) {
	session.mu.Lock()
	delete(session.beginning, digest)
	session.mu.Unlock()
	session.beginWG.Done()
}

func (session *filesystemOutputSession) allocateOutputObjectID(
	digest resumestate.LocatorDigest,
) (resumestate.OutputObjectID, error) {
	for range outputStateAllocationAttempts {
		objectID, err := session.owner.objectIDs.NewOutputObjectID()
		if err != nil {
			return resumestate.OutputObjectID{}, err
		}
		if objectID.IsZero() {
			continue
		}
		session.mu.Lock()
		_, claimed := session.objectClaims[objectID]
		if !claimed {
			session.objectClaims[objectID] = digest
		}
		session.mu.Unlock()
		if claimed {
			continue
		}
		occupied, err := session.outputObjectNameOccupied(objectID)
		if err != nil {
			session.releaseOutputObjectClaim(objectID, digest)
			return resumestate.OutputObjectID{}, err
		}
		if !occupied {
			return objectID, nil
		}
		session.releaseOutputObjectClaim(objectID, digest)
	}
	return resumestate.OutputObjectID{}, fmt.Errorf("%w: allocate unique output object", errOutputV3Unsafe)
}

func (session *filesystemOutputSession) releaseOutputObjectClaim(
	objectID resumestate.OutputObjectID,
	digest resumestate.LocatorDigest,
) {
	session.mu.Lock()
	if session.objectClaims[objectID] == digest {
		delete(session.objectClaims, objectID)
	}
	session.mu.Unlock()
}

func (session *filesystemOutputSession) outputObjectNameOccupied(id resumestate.OutputObjectID) (bool, error) {
	for _, candidate := range []struct {
		parent outputV3Directory
		name   resumestate.ShardedName
	}{
		{session.anchorsDir, resumestate.AnchorName(id)},
		{session.stagesDir, resumestate.StageName(id)},
	} {
		shard, present, err := openOutputShard(candidate.parent, candidate.name.Shard(), false)
		if err != nil {
			return false, err
		}
		if !present {
			continue
		}
		kind, observeErr := shard.ObserveEntry(candidate.name.Name())
		closeErr := shard.Close()
		if observeErr != nil || closeErr != nil {
			return false, errors.Join(observeErr, closeErr)
		}
		if kind != outputV3EntryAbsent {
			return true, nil
		}
	}
	return false, nil
}

func (session *filesystemOutputSession) resumeFile(
	ctx context.Context,
	file transfer.OutputFile,
	recordDir outputV3Directory,
	recordName string,
	bound resumestate.BoundFileRecord,
) (transfer.FileStart, bool, error) {
	record := bound.Record()
	name := resumestate.FileRecordName(record.LocatorDigest())
	if name.Name() != recordName || record.CanonicalLocator() != file.Path {
		start, quarantineErr := session.quarantinedStart(
			file.Target, resumestate.DigestCanonicalLocator(file.Path), transfer.QuarantineOwnershipMismatch,
		)
		return start, true, quarantineErr
	}
	resumable, err := resumestate.BindResumableFile(bound, file.Descriptor)
	if err != nil {
		if !outputV3RevisionOnlyMismatch(record, file.Descriptor) {
			return transfer.FileStart{}, true, outputFault(transfer.OutputFaultFile, transfer.OutputFaultOwnership, err)
		}
		return session.replaceInvalidatedRevision(ctx, file, recordDir, recordName, bound)
	}
	start, reduceErr := session.reduceFile(ctx, file, resumable, recordDir, recordName)
	return start, true, reduceErr
}

func outputV3RevisionOnlyMismatch(
	record resumestate.FileRecord,
	descriptor content.FileRevisionDescriptor,
) bool {
	return descriptor.FileRevision() != record.Revision() &&
		descriptor.ShareInstance() == record.ShareInstance() && descriptor.FileID() == record.FileID() &&
		descriptor.ExactSize() == record.ExactSize() &&
		descriptor.Geometry().ChunkSize() == record.ChunkSize() &&
		descriptor.ModifiedTime() == record.ExpectedMetadata().ModifiedTime
}

func (session *filesystemOutputSession) replaceInvalidatedRevision(
	ctx context.Context,
	file transfer.OutputFile,
	recordDir outputV3Directory,
	recordName string,
	bound resumestate.BoundFileRecord,
) (resultStart transfer.FileStart, resultHandled bool, resultErr error) {
	requirement := outputAncestryRequirement{}
	validation, err := session.validateOutputAncestry(requirement)
	if err != nil {
		session.traceOutputAncestry(FilesystemOutputAncestryRecovery, bound.Record().LocatorDigest(), err)
		return transfer.FileStart{}, true,
			outputAncestryOperationFault("validate ancestry before invalidated-revision recovery", err)
	}
	defer func() {
		ancestryErr := finishOutputAncestryOperation(
			session, validation, requirement, FilesystemOutputAncestryRecovery,
			bound.Record().LocatorDigest(), "finish invalidated-revision recovery ancestry", nil,
		)
		if ancestryErr != nil {
			resultStart = transfer.FileStart{}
			resultHandled = true
			resultErr = errors.Join(resultErr, ancestryErr)
		}
	}()
	parentSynced := false
	for {
		if err := ctx.Err(); err != nil {
			return transfer.FileStart{}, true, outputFault(transfer.OutputFaultFile, transfer.OutputFaultStateIO, err)
		}
		if bound.Record().Phase() == resumestate.FileRetiring {
			_, quarantined, err := session.retireBoundFile(
				recordDir, recordName, bound, transfer.OutputFileBinding{},
			)
			if err != nil {
				return transfer.FileStart{}, true, err
			}
			if quarantined {
				start, quarantineErr := session.quarantinedStart(
					file.Target, bound.Record().LocatorDigest(), transfer.QuarantineRetirementMismatch,
				)
				return start, true, quarantineErr
			}
			return transfer.FileStart{}, false, nil
		}

		observation, observationCleanupErr, observationErr := session.observeFile(
			validation, bound.Record(), parentSynced,
		)
		if observationErr != nil {
			return transfer.FileStart{}, true, pauseRequiredFileOutputFault(fileOutputFault(
				"observe invalidated revision", errors.Join(observationErr, observationCleanupErr),
			))
		}
		decision, err := resumestate.ReduceFileRecovery(bound, observation)
		if err != nil {
			return transfer.FileStart{}, true, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
		}
		quarantineDecision := decision.Action() == resumestate.RecoveryInstallQuarantine ||
			decision.Action() == resumestate.RecoveryHoldQuarantine
		if observationCleanupErr != nil && !quarantineDecision {
			return transfer.FileStart{}, true, pauseRequiredFileOutputFault(fileOutputFault(
				"close invalidated-revision observation", observationCleanupErr,
			))
		}
		session.owner.trace(FilesystemOutputTrace{
			Operation: TraceFileRecoveryDecision, ResumeIntent: session.resumeIntent,
			SessionID: session.SessionID(), LocatorDigest: outputLocatorDigestFromState(bound.Record().LocatorDigest()),
			OutputObjectID:   outputObjectIdentityFromState(bound.Record().OutputObject()),
			PreviousPhase:    filesystemOutputFilePhaseFromState(bound.Record().Phase()),
			RecoveryAction:   filesystemOutputRecoveryActionFromState(decision.Action()),
			QuarantineReason: recoveryDecisionQuarantineReason(decision),
		})

		switch decision.Action() {
		case resumestate.RecoveryInstallQuarantine:
			quarantined, err := resumestate.ApplyRecoveryDecision(bound, decision)
			if err != nil {
				return transfer.FileStart{}, true, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
			}
			if err := session.installFileRecord(recordDir, recordName, bound, quarantined); err != nil {
				return transfer.FileStart{}, true, err
			}
			if observationCleanupErr != nil {
				return transfer.FileStart{}, true, pauseRequiredFileOutputFault(fileOutputFault(
					"close quarantined invalidated-revision observation", observationCleanupErr,
				))
			}
			start, err := session.quarantinedStart(
				file.Target, quarantined.Record().LocatorDigest(),
				mapQuarantineReason(quarantined.Record().QuarantineReason()),
			)
			return start, true, err
		case resumestate.RecoveryHoldQuarantine:
			if observationCleanupErr != nil {
				return transfer.FileStart{}, true, pauseRequiredFileOutputFault(fileOutputFault(
					"close held invalidated-revision observation", observationCleanupErr,
				))
			}
			start, err := session.quarantinedStart(
				file.Target, bound.Record().LocatorDigest(), mapQuarantineReason(bound.Record().QuarantineReason()),
			)
			return start, true, err
		case resumestate.RecoverySyncFinalParent:
			start, terminal, err := session.recoverFinalParentSync(
				file, recordDir, recordName, bound, "sync invalidated revision final parent",
			)
			if terminal {
				return start, true, err
			}
			parentSynced = true
		case resumestate.RecoveryInstallPublished:
			published, err := resumestate.ApplyRecoveryDecision(bound, decision)
			if err != nil {
				return transfer.FileStart{}, true, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
			}
			if err := session.installFileRecord(recordDir, recordName, bound, published); err != nil {
				return transfer.FileStart{}, true, err
			}
			bound = published
			parentSynced = false
		case resumestate.RecoveryHoldPublishedCleanup:
			return transfer.FileStart{}, true, internalCleanupNeedsAttentionFault(
				"hold invalidated published file with ambiguous internal cleanup evidence",
			)
		case resumestate.RecoveryRemovePublishedStageAndSync, resumestate.RecoverySyncPublishedStageParent:
			retirement, err := session.authorizePublishedRetirement(recordDir, recordName, bound)
			if err != nil {
				return transfer.FileStart{}, true, err
			}
			switch retirement.disposition {
			case publishedRetirementAuthorized:
				return session.installInvalidatedRevisionRetirement(file, recordDir, recordName, bound)
			case publishedRetirementHoldPreserve:
				return transfer.FileStart{}, true, internalCleanupNeedsAttentionFault(
					"hold invalidated published file after retirement revalidation",
				)
			case publishedRetirementQuarantineInstalled:
				start, err := session.quarantinedStart(
					file.Target, bound.Record().LocatorDigest(), mapQuarantineReason(retirement.quarantineReason),
				)
				return start, true, err
			default:
				return transfer.FileStart{}, true, outputFault(
					transfer.OutputFaultFile, transfer.OutputFaultContract, resumestate.ErrInvalidState,
				)
			}
		case resumestate.RecoveryRetryObjectCreation, resumestate.RecoveryInstallWitness,
			resumestate.RecoveryRequireRevisionBinding, resumestate.RecoveryInstallPublishing,
			resumestate.RecoveryHoldPublishBlocked, resumestate.RecoveryLinkFinalNoReplace,
			resumestate.RecoveryInstallRetiring:
			return session.installInvalidatedRevisionRetirement(file, recordDir, recordName, bound)
		default:
			return transfer.FileStart{}, true, outputFault(
				transfer.OutputFaultFile, transfer.OutputFaultContract,
				fmt.Errorf("unexpected invalidated-revision recovery action %d", decision.Action()),
			)
		}
	}
}

func (session *filesystemOutputSession) installInvalidatedRevisionRetirement(
	file transfer.OutputFile,
	recordDir outputV3Directory,
	recordName string,
	bound resumestate.BoundFileRecord,
) (transfer.FileStart, bool, error) {
	retiring, err := resumestate.PrepareInvalidatedRevisionRetirement(bound)
	if err != nil {
		return transfer.FileStart{}, true, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if err := session.installFileRecord(recordDir, recordName, bound, retiring); err != nil {
		return transfer.FileStart{}, true, err
	}
	_, quarantined, err := session.retireBoundFile(
		recordDir, recordName, retiring, transfer.OutputFileBinding{},
	)
	if err != nil {
		return transfer.FileStart{}, true, err
	}
	if quarantined {
		start, quarantineErr := session.quarantinedStart(
			file.Target, retiring.Record().LocatorDigest(), transfer.QuarantineRetirementMismatch,
		)
		return start, true, quarantineErr
	}
	return transfer.FileStart{}, false, nil
}

func (session *filesystemOutputSession) reduceFile(
	ctx context.Context,
	file transfer.OutputFile,
	resumable resumestate.ResumableFileAuthority,
	recordDir outputV3Directory,
	recordName string,
) (resultStart transfer.FileStart, resultErr error) {
	requirement := outputAncestryRequirement{}
	validation, err := session.validateOutputAncestry(requirement)
	if err != nil {
		session.traceOutputAncestry(FilesystemOutputAncestryRecovery, resumable.Bound().Record().LocatorDigest(), err)
		return transfer.FileStart{}, outputAncestryOperationFault("validate ancestry before file recovery", err)
	}
	defer func() {
		ancestryErr := finishOutputAncestryOperation(
			session, validation, requirement, FilesystemOutputAncestryRecovery,
			resumable.Bound().Record().LocatorDigest(), "finish file recovery ancestry", nil,
		)
		if ancestryErr != nil {
			resultStart = transfer.FileStart{}
			resultErr = errors.Join(resultErr, ancestryErr)
		}
	}()
	parentSynced := false
	for {
		if err := ctx.Err(); err != nil {
			return transfer.FileStart{}, err
		}
		observation, observationCleanupErr, observationErr := session.observeFile(
			validation, resumable.Bound().Record(), parentSynced,
		)
		if observationErr != nil {
			return transfer.FileStart{}, pauseRequiredFileOutputFault(fileOutputFault(
				"observe file recovery", errors.Join(observationErr, observationCleanupErr),
			))
		}
		decision, err := resumestate.ReduceResumableFileRecovery(resumable, observation)
		if err != nil {
			return transfer.FileStart{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
		}
		quarantineDecision := decision.Action() == resumestate.RecoveryInstallQuarantine ||
			decision.Action() == resumestate.RecoveryHoldQuarantine
		if observationCleanupErr != nil && !quarantineDecision {
			return transfer.FileStart{}, pauseRequiredFileOutputFault(fileOutputFault(
				"close file recovery observation", observationCleanupErr,
			))
		}
		session.owner.trace(FilesystemOutputTrace{
			Operation: TraceFileRecoveryDecision, ResumeIntent: session.resumeIntent,
			SessionID: session.SessionID(), LocatorDigest: outputLocatorDigestFromState(resumable.Bound().Record().LocatorDigest()),
			OutputObjectID:   outputObjectIdentityFromState(resumable.Bound().Record().OutputObject()),
			PreviousPhase:    filesystemOutputFilePhaseFromState(resumable.Bound().Record().Phase()),
			RecoveryAction:   filesystemOutputRecoveryActionFromState(decision.Action()),
			QuarantineReason: recoveryDecisionQuarantineReason(decision),
		})

		switch decision.Action() {
		case resumestate.RecoveryRetryObjectCreation:
			operationErr, cleanupErr := session.createWitnessObject(resumable.Bound().Record())
			if operationErr != nil {
				if classifyOutputV3RecoveryFailure(
					operationErr, outputV3AuthorizedMutation,
				) == outputV3RecoveryAmbiguous {
					return session.quarantineRecoveryStartWithCleanup(
						file, recordDir, recordName, resumable.Bound(), resumestate.QuarantinePartialObjectCreation,
						"close quarantined witnessed output object", cleanupErr,
					)
				}
				return transfer.FileStart{}, pauseRequiredFileOperationFault(
					"create witnessed output object", operationErr, cleanupErr,
				)
			}
			if cleanupErr != nil {
				return transfer.FileStart{}, pauseRequiredFileOperationFault(
					"close created witnessed output object", nil, cleanupErr,
				)
			}
		case resumestate.RecoveryInstallWitness, resumestate.RecoveryInstallPublishing,
			resumestate.RecoveryInstallPublished, resumestate.RecoveryInstallPublishBlocked,
			resumestate.RecoveryInstallRetiring, resumestate.RecoveryInstallQuarantine:
			next, err := resumestate.ApplyRecoveryDecision(resumable.Bound(), decision)
			if err != nil {
				return transfer.FileStart{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
			}
			if err := session.installFileRecord(recordDir, recordName, resumable.Bound(), next); err != nil {
				return transfer.FileStart{}, err
			}
			resumable, err = resumestate.BindResumableFile(next, file.Descriptor)
			if err != nil {
				return transfer.FileStart{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
			}
			if decision.Action() == resumestate.RecoveryInstallPublishBlocked {
				return session.verifiedStart(transfer.FilePublishBlocked, resumable)
			}
			if decision.Action() == resumestate.RecoveryInstallQuarantine {
				if observationCleanupErr != nil {
					return transfer.FileStart{}, pauseRequiredFileOutputFault(fileOutputFault(
						"close quarantined file recovery observation", observationCleanupErr,
					))
				}
				return session.quarantinedStart(
					file.Target, resumable.Bound().Record().LocatorDigest(), mapQuarantineReason(decision.QuarantineReason()),
				)
			}
			if decision.Action() == resumestate.RecoveryInstallRetiring {
				binding, bindErr := outputBindingForRecord(session.SessionID(), file.Descriptor, next.Record())
				if bindErr != nil {
					return transfer.FileStart{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, bindErr)
				}
				settlement, _, cleanupErr := session.retireBoundFile(recordDir, recordName, next, binding)
				if cleanupErr != nil {
					return transfer.FileStart{}, cleanupErr
				}
				if decision.Settlement() == resumestate.RecoveryCollision {
					return session.collisionStart(file)
				}
				return transfer.NewFileSettlementStart(settlement)
			}
		case resumestate.RecoveryResumeContent:
			return session.transactionStart(file.Descriptor, resumable, recordDir, recordName)
		case resumestate.RecoveryLinkFinalNoReplace:
			// Retain the witness selected by this recovery attempt, then require the
			// fixed names to identify it again immediately before publication. This
			// prevents the reducer's observation from degrading into pathname
			// authority while the no-replace operation is prepared.
			witness, witnessErr, witnessCleanupErr := session.openPublicationWitness(
				resumable.Bound().Record(), nil,
			)
			if witnessErr != nil {
				if classifyOutputV3RecoveryFailure(
					witnessErr, outputV3ExistingEntryUnclassified,
				) == outputV3RecoveryAmbiguous {
					return session.quarantineRecoveryStartWithCleanup(
						file, recordDir, recordName, resumable.Bound(), resumestate.QuarantinePublicationHistory,
						"close quarantined recovery publication witness", witnessCleanupErr,
					)
				}
				return transfer.FileStart{}, pauseRequiredFileOperationFault(
					"retain recovery publication witness", witnessErr, witnessCleanupErr,
				)
			}
			if witnessCleanupErr != nil {
				return transfer.FileStart{}, pauseRequiredFileOperationFault(
					"close retained recovery publication witness", nil, witnessCleanupErr,
				)
			}
			publishResult, linkErr, linkCleanupErr := session.linkFinalNoReplaceResult(
				resumable.Bound(), witness.anchor,
			)
			cleanupErr := errors.Join(linkCleanupErr, witness.Close())
			if errors.Is(linkErr, errOutputAncestryUnsafe) {
				return transfer.FileStart{}, outputAncestryPauseFault("revalidate recovery final publication", linkErr)
			}
			if publishResult == 0 && linkErr != nil {
				if classifyOutputV3RecoveryFailure(
					linkErr, outputV3AuthorizedMutation,
				) == outputV3RecoveryAmbiguous {
					return session.quarantineRecoveryStartWithCleanup(
						file, recordDir, recordName, resumable.Bound(), resumestate.QuarantinePublicationHistory,
						"close quarantined final-link evidence", cleanupErr,
					)
				}
				return transfer.FileStart{}, pauseRequiredFileOperationFault(
					"publish final link", linkErr, cleanupErr,
				)
			}
			if publishResult == 0 {
				return transfer.FileStart{}, pauseRequiredFileOperationFault(
					"close unclassified final-link evidence", nil, cleanupErr,
				)
			}
			if publishResult != resumestate.PublishLinkCreated {
				publishDecision, reduceErr := resumestate.ReducePublishResult(
					resumable.Bound(), publishResult,
				)
				if reduceErr != nil {
					return transfer.FileStart{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, reduceErr)
				}
				next, applyErr := resumestate.ApplyRecoveryDecision(resumable.Bound(), publishDecision)
				if applyErr != nil {
					return transfer.FileStart{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, applyErr)
				}
				if err := session.installFileRecord(recordDir, recordName, resumable.Bound(), next); err != nil {
					return transfer.FileStart{}, err
				}
				resumable, err = resumestate.BindResumableFile(next, file.Descriptor)
				if err != nil {
					return transfer.FileStart{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
				}
				if linkErr != nil {
					if publishResult != resumestate.PublishExistingAmbiguous && classifyOutputV3RecoveryFailure(
						linkErr, outputV3AuthorizedMutation,
					) == outputV3RecoveryAmbiguous {
						return session.quarantineRecoveryStartWithCleanup(
							file, recordDir, recordName, next, resumestate.QuarantinePublicationHistory,
							"close quarantined classified publication evidence", cleanupErr,
						)
					}
					return transfer.FileStart{}, pauseRequiredFileOperationFault(
						"settle classified publication evidence", linkErr, cleanupErr,
					)
				}
				if cleanupErr != nil {
					return transfer.FileStart{}, pauseRequiredFileOperationFault(
						"close classified publication evidence", nil, cleanupErr,
					)
				}
				if publishResult == resumestate.PublishExistingAmbiguous {
					return session.quarantinedStart(
						file.Target, next.Record().LocatorDigest(), mapQuarantineReason(next.Record().QuarantineReason()),
					)
				}
				return session.verifiedStart(transfer.FilePublishBlocked, resumable)
			}
			if linkErr != nil || cleanupErr != nil {
				return transfer.FileStart{}, pauseRequiredFileOperationFault(
					"finish final publication", linkErr, cleanupErr,
				)
			}
			parentSynced = true
		case resumestate.RecoverySyncFinalParent:
			start, terminal, err := session.recoverFinalParentSync(
				file, recordDir, recordName, resumable.Bound(), "sync final parent",
			)
			if terminal {
				return start, err
			}
			parentSynced = true
		case resumestate.RecoveryHoldPublishBlocked:
			return session.verifiedStart(transfer.FilePublishBlocked, resumable)
		case resumestate.RecoveryRemovePublishedStageAndSync:
			operationErr, cleanupErr := session.removeStage(resumable.Bound().Record())
			if operationErr != nil {
				return transfer.FileStart{}, pauseRequiredFileOperationFault(
					"remove published stage", operationErr, cleanupErr,
				)
			}
			if cleanupErr != nil {
				return transfer.FileStart{}, pauseRequiredFileOperationFault(
					"close removed published stage", nil, cleanupErr,
				)
			}
		case resumestate.RecoverySyncPublishedStageParent:
			operationErr, cleanupErr := session.syncObjectShard(
				session.stagesDir, resumestate.StageName(resumable.Bound().Record().OutputObject()),
			)
			if operationErr != nil {
				return transfer.FileStart{}, pauseRequiredFileOperationFault(
					"sync published stage shard", operationErr, cleanupErr,
				)
			}
			if cleanupErr != nil {
				return transfer.FileStart{}, pauseRequiredFileOperationFault(
					"close synced published-stage shard", nil, cleanupErr,
				)
			}
			return session.verifiedStart(transfer.FilePublished, resumable)
		case resumestate.RecoveryHoldPublishedCleanup:
			return transfer.FileStart{}, internalCleanupNeedsAttentionFault(
				"hold published file with ambiguous internal cleanup evidence",
			)
		case resumestate.RecoveryHoldQuarantine:
			if observationCleanupErr != nil {
				return transfer.FileStart{}, pauseRequiredFileOutputFault(fileOutputFault(
					"close held file recovery observation", observationCleanupErr,
				))
			}
			return session.quarantinedStart(
				file.Target, resumable.Bound().Record().LocatorDigest(), mapQuarantineReason(resumable.Bound().Record().QuarantineReason()),
			)
		case resumestate.RecoveryHoldRetiringCleanup:
			return transfer.FileStart{}, internalCleanupNeedsAttentionFault(
				"hold retiring file with ambiguous internal cleanup evidence",
			)
		case resumestate.RecoveryRemoveRetiringStageAndSync,
			resumestate.RecoverySyncStageRemoveAnchorAndSync,
			resumestate.RecoverySyncParentsRemoveRecordAndSync:
			binding, bindErr := outputBindingForRecord(session.SessionID(), file.Descriptor, resumable.Bound().Record())
			if bindErr != nil {
				return transfer.FileStart{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, bindErr)
			}
			settlement, _, cleanupErr := session.retireBoundFile(recordDir, recordName, resumable.Bound(), binding)
			if cleanupErr != nil {
				return transfer.FileStart{}, cleanupErr
			}
			return transfer.NewFileSettlementStart(settlement)
		default:
			return transfer.FileStart{}, outputFault(
				transfer.OutputFaultFile, transfer.OutputFaultContract,
				fmt.Errorf("unsupported recovery action %d", decision.Action()),
			)
		}
	}
}

func (session *filesystemOutputSession) createWitnessObject(
	record resumestate.FileRecord,
) (operationErr error, cleanupErr error) {
	stageName := resumestate.StageName(record.OutputObject())
	anchorName := resumestate.AnchorName(record.OutputObject())
	stageDir, _, operationErr := openOutputShard(session.stagesDir, stageName.Shard(), true)
	if operationErr != nil {
		cleanupErr = closeOutputV3Directory(stageDir)
		return
	}
	defer func() { cleanupErr = errors.Join(cleanupErr, stageDir.Close()) }()
	anchorDir, _, operationErr := openOutputShard(session.anchorsDir, anchorName.Shard(), true)
	if operationErr != nil {
		cleanupErr = closeOutputV3Directory(anchorDir)
		return
	}
	defer func() { cleanupErr = errors.Join(cleanupErr, anchorDir.Close()) }()
	stage, operationErr := stageDir.CreateFile(stageName.Name(), true, int64(record.ExactSize()))
	if stage != nil {
		defer func() { cleanupErr = errors.Join(cleanupErr, stage.Close()) }()
	}
	if operationErr != nil {
		return
	}
	operationErr = errors.Join(stage.Sync(), stageDir.Sync())
	if operationErr != nil {
		return
	}
	anchor, operationErr := anchorDir.LinkFileNoReplace(stage, anchorName.Name())
	if anchor != nil {
		defer func() { cleanupErr = errors.Join(cleanupErr, anchor.Close()) }()
	}
	if operationErr != nil {
		return
	}
	operationErr = anchorDir.Sync()
	if operationErr != nil {
		return
	}
	same, sameErr := anchor.SameFile(stage)
	if sameErr != nil || !same {
		operationErr = errors.Join(errOutputV3Unsafe, sameErr)
		return
	}
	stageSize, stageErr := stage.Size()
	anchorSize, anchorErr := anchor.Size()
	if stageErr != nil || anchorErr != nil || stageSize != record.ExactSize() || anchorSize != record.ExactSize() {
		operationErr = errors.Join(errOutputV3Unsafe, stageErr, anchorErr)
		return
	}
	return
}

func (session *filesystemOutputSession) observeFile(
	validation *outputAncestryValidation,
	record resumestate.FileRecord,
	finalParentSynced bool,
) (
	observation resumestate.FileObservation,
	cleanupErr error,
	observationErr error,
) {
	var anchor, stage, final outputV3File
	var anchorDir, stageDir, parent outputV3Directory
	defer func() {
		cleanupErr = errors.Join(
			cleanupErr,
			closeOutputV3File(final),
			closeOutputV3File(stage), closeOutputV3Directory(stageDir),
			closeOutputV3File(anchor), closeOutputV3Directory(anchorDir),
		)
	}()

	var anchorObservation resumestate.AnchorObservation
	var anchorErr error
	if record.Phase() == resumestate.FileRetiring {
		anchor, anchorDir, anchorObservation, anchorErr = session.observeAnchor(record)
		if anchorErr != nil {
			return resumestate.FileObservation{}, nil, anchorErr
		}
		var stageObservation resumestate.EntryObservation
		var stageErr error
		stage, stageDir, stageObservation, stageErr = session.observeStage(record, anchor, anchorObservation)
		if stageErr != nil {
			partial := resumestate.FileObservation{
				Anchor: anchorObservation, Stage: resumestate.EntryNotObserved,
				Final: resumestate.EntryNotObserved, Metadata: resumestate.MetadataNotObserved,
				FinalParent: resumestate.FinalParentNotObserved,
			}
			if resumestate.InternalFileObservationRequiresQuarantine(record.Phase(), partial) {
				return partial, nil, nil
			}
			return resumestate.FileObservation{}, nil, stageErr
		}
		return resumestate.FileObservation{
			Anchor: anchorObservation, Stage: stageObservation,
			Final: resumestate.EntryNotObserved, Metadata: resumestate.MetadataNotObserved,
			FinalParent: resumestate.FinalParentNotObserved,
		}, nil, nil
	}

	anchor, anchorDir, anchorObservation, anchorErr = session.observeAnchor(record)
	if anchorErr != nil {
		return resumestate.FileObservation{}, nil, anchorErr
	}
	var stageObservation resumestate.EntryObservation
	var stageErr error
	stage, stageDir, stageObservation, stageErr = session.observeStage(record, anchor, anchorObservation)
	if stageErr != nil {
		partial := resumestate.FileObservation{
			Anchor: anchorObservation, Stage: resumestate.EntryNotObserved,
			Final: resumestate.EntryNotObserved, Metadata: resumestate.MetadataNotObserved,
			FinalParent: resumestate.FinalParentNotObserved,
		}
		if resumestate.InternalFileObservationRequiresQuarantine(record.Phase(), partial) {
			return partial, nil, nil
		}
		return resumestate.FileObservation{}, nil, stageErr
	}
	partial := resumestate.FileObservation{
		Anchor: anchorObservation, Stage: stageObservation, Final: resumestate.EntryNotObserved,
		Metadata: resumestate.MetadataNotObserved, FinalParent: resumestate.FinalParentNotObserved,
	}
	if resumestate.InternalFileObservationRequiresQuarantine(record.Phase(), partial) {
		return partial, nil, nil
	}
	finalObservation := resumestate.EntryNotObserved
	metadata := resumestate.MetadataNotObserved
	parentState := resumestate.FinalParentNotObserved
	parentPath, leaf, err := outputLocatorParentAndLeaf(record.CanonicalLocator())
	if err == nil {
		parent, err = validation.directory(parentPath)
	}
	if err == nil {
		err = validation.revalidateRetainedDirectory(parentPath, outputAncestryNoAuthority)
	}
	if err != nil {
		if classifyOutputV3RecoveryFailure(err, outputV3BeforeEntryEvidence) == outputV3RecoveryPauseRequired {
			return resumestate.FileObservation{}, nil, err
		}
		partial.Final = resumestate.EntryUnsafe
		return partial, nil, nil
	}
	if err == nil {
		kind, observeErr := parent.ObserveEntry(leaf)
		switch {
		case observeErr != nil:
			if classifyOutputV3RecoveryFailure(
				observeErr, outputV3BeforeEntryEvidence,
			) == outputV3RecoveryPauseRequired {
				return resumestate.FileObservation{}, nil, observeErr
			}
			finalObservation = resumestate.EntryUnsafe
		case kind == outputV3EntryAbsent:
			finalObservation = resumestate.EntryMissing
		case kind != outputV3EntryRegularFile:
			if anchorObservation == resumestate.AnchorVerified {
				finalObservation = resumestate.EntryDifferentFromAnchor
			} else {
				finalObservation = resumestate.EntryPresentUnresolved
			}
		default:
			var openErr error
			final, openErr = parent.OpenFile(leaf, false, false)
			if openErr != nil {
				finalObservation = resumestate.EntryUnsafe
				break
			}
			if anchorObservation != resumestate.AnchorVerified {
				finalObservation = resumestate.EntryPresentUnresolved
				break
			}
			same, sameErr := final.SameFile(anchor)
			if sameErr != nil {
				finalObservation = resumestate.EntryUnsafe
				break
			}
			if !same {
				finalObservation = resumestate.EntryDifferentFromAnchor
				break
			}
			finalObservation = resumestate.EntrySameAsAnchor
			matches, metadataErr := final.MetadataMatches(
				record.ExactSize(), record.ExpectedMetadata().ModifiedTime,
			)
			switch {
			case metadataErr != nil:
				metadata = resumestate.MetadataUnsafe
			case matches:
				metadata = resumestate.MetadataMatches
			default:
				metadata = resumestate.MetadataDiffers
			}
			if finalParentSynced || record.Phase() == resumestate.FilePublished {
				parentState = resumestate.FinalParentSynced
			} else {
				parentState = resumestate.FinalParentSyncRequired
			}
		}
	}
	return resumestate.FileObservation{
		Anchor: anchorObservation, Stage: stageObservation, Final: finalObservation,
		Metadata: metadata, FinalParent: parentState,
	}, nil, nil
}

func closeOutputV3File(file outputV3File) error {
	if file == nil {
		return nil
	}
	return file.Close()
}

func (session *filesystemOutputSession) observeAnchor(
	record resumestate.FileRecord,
) (outputV3File, outputV3Directory, resumestate.AnchorObservation, error) {
	name := resumestate.AnchorName(record.OutputObject())
	directory, present, err := openOutputShard(session.anchorsDir, name.Shard(), false)
	if err != nil {
		if classifyOutputV3RecoveryFailure(err, outputV3BeforeEntryEvidence) == outputV3RecoveryAmbiguous {
			return nil, nil, resumestate.AnchorUnsafe, nil
		}
		return nil, nil, 0, err
	}
	if !present {
		return nil, nil, resumestate.AnchorMissing, nil
	}
	kind, err := directory.ObserveEntry(name.Name())
	if err != nil {
		if classifyOutputV3RecoveryFailure(err, outputV3BeforeEntryEvidence) == outputV3RecoveryAmbiguous {
			return nil, directory, resumestate.AnchorUnsafe, nil
		}
		return nil, directory, 0, err
	}
	if kind == outputV3EntryAbsent {
		return nil, directory, resumestate.AnchorMissing, nil
	}
	if kind != outputV3EntryRegularFile {
		return nil, directory, resumestate.AnchorUnsafe, nil
	}
	anchor, err := directory.OpenFile(name.Name(), true, false)
	if err != nil {
		return anchor, directory, resumestate.AnchorUnsafe, nil
	}
	size, err := anchor.Size()
	if err != nil || size != record.ExactSize() {
		return anchor, directory, resumestate.AnchorUnsafe, nil
	}
	return anchor, directory, resumestate.AnchorVerified, nil
}

func (session *filesystemOutputSession) observeStage(
	record resumestate.FileRecord,
	anchor outputV3File,
	anchorObservation resumestate.AnchorObservation,
) (outputV3File, outputV3Directory, resumestate.EntryObservation, error) {
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
	if kind == outputV3EntryAbsent {
		return nil, directory, resumestate.EntryMissing, nil
	}
	if kind != outputV3EntryRegularFile {
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

func (session *filesystemOutputSession) installFileRecord(
	directory outputV3Directory,
	name string,
	previous resumestate.BoundFileRecord,
	next resumestate.BoundFileRecord,
) error {
	session.stateInstall.Lock()
	defer session.stateInstall.Unlock()
	if session.stateWritesDisabled() {
		return pauseRequiredFileOutputFault(outputFault(
			transfer.OutputFaultFile, transfer.OutputFaultOwnership, errOutputSessionClosed,
		))
	}
	if previous.Record().LocatorDigest() != next.Record().LocatorDigest() ||
		next.Record().StateGeneration() <= previous.Record().StateGeneration() {
		return outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, resumestate.ErrInvalidTransition)
	}
	currentEncoded, err := resumestate.EncodeFileRecord(previous)
	if err != nil {
		return outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	nextEncoded, err := resumestate.EncodeFileRecord(next)
	if err != nil {
		return outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	outcome, replaceErr := session.store.replaceRecord(
		directory,
		name,
		outputStateRecordImage{encoded: currentEncoded, generation: previous.Record().StateGeneration()},
		outputStateRecordImage{encoded: nextEncoded, generation: next.Record().StateGeneration()},
		resumestate.MaxFileStateBytes,
	)
	var resultErr error
	switch outcome {
	case outputStateReplaceAdopted:
		if replaceErr != nil {
			// The durable record is already the next generation, but the current
			// owner cannot safely continue with handles whose close/cleanup status is
			// unknown. A fresh process can adopt the exact installed generation.
			session.poisonState()
			resultErr = pauseRequiredFileOutputFault(fileOutputFault("finish adopted file-state replacement", replaceErr))
		}
	case outputStateReplaceUnchanged:
		if replaceErr == nil {
			replaceErr = errOutputV3Unsafe
		}
		return pauseRequiredFileOutputFault(fileOutputFault("replace file state", replaceErr))
	case outputStateReplaceUncertain:
		session.poisonState()
		return pauseRequiredFileOutputFault(fileOutputFault(
			"replace file state with uncertain authority", errors.Join(errOutputV3Unsafe, replaceErr),
		))
	default:
		session.poisonState()
		return pauseRequiredFileOutputFault(outputFault(
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

func (session *filesystemOutputSession) ensureInitialFileRecord(
	directory outputV3Directory,
	name string,
	encoded []byte,
) (outputStateCreateOutcome, error) {
	session.stateInstall.Lock()
	defer session.stateInstall.Unlock()
	if session.stateWritesDisabled() {
		return outputStateCreateNotInstalled, errOutputSessionClosed
	}
	return session.store.ensureInitialRecord(directory, name, encoded, resumestate.MaxFileStateBytes)
}

func (session *filesystemOutputSession) transactionStart(
	descriptor content.FileRevisionDescriptor,
	resumable resumestate.ResumableFileAuthority,
	recordDir outputV3Directory,
	recordName string,
) (resultStart transfer.FileStart, resultErr error) {
	record := resumable.Bound().Record()
	stageName := resumestate.StageName(record.OutputObject())
	anchorName := resumestate.AnchorName(record.OutputObject())
	var stageDir, anchorDir outputV3Directory
	var data, anchor outputV3File
	cleanupOwned := true
	defer func() {
		if !cleanupOwned {
			return
		}
		if closeErr := errors.Join(
			closeOutputV3File(data), closeOutputV3File(anchor),
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
			return transfer.FileStart{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
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
	data, err = stageDir.OpenFile(stageName.Name(), true, true)
	if err != nil {
		return quarantine(resumestate.QuarantineStageUnsafe)
	}
	anchor, err = anchorDir.OpenFile(anchorName.Name(), true, false)
	if err != nil {
		return quarantine(resumestate.QuarantineAnchorUnsafe)
	}
	same, err := data.SameFile(anchor)
	if err != nil || !same {
		return quarantine(resumestate.QuarantineStageUnsafe)
	}
	binding, err := outputBindingForRecord(session.SessionID(), descriptor, record)
	if err != nil {
		return transfer.FileStart{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	durable, err := transfer.VerifyDurableRanges(
		binding, transfer.CheckpointGeneration(record.CheckpointGeneration()), record.DurableRanges(),
	)
	if err != nil {
		return transfer.FileStart{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	pending, _ := content.NewRangeSet(nil)
	transaction := &filesystemFileTransaction{
		session: session, descriptor: descriptor, resumable: resumable, binding: binding,
		recordDir: recordDir, recordName: recordName,
		anchorDir: anchorDir, anchorName: anchorName.Name(), stageDir: stageDir, stageName: stageName.Name(),
		anchor: anchor, data: data, pending: pending,
		lifecycle: filesystemFileTransactionOpen, reduceFile: session.reduceFile,
	}
	session.mu.Lock()
	session.active[record.LocatorDigest()] = transaction
	session.mu.Unlock()
	start, err := transfer.NewFileTransactionStart(transaction, durable)
	if err != nil {
		session.finishFile(record.LocatorDigest(), transaction)
		return transfer.FileStart{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	cleanupOwned = false
	return start, nil
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

func (session *filesystemOutputSession) verifiedStart(
	kind transfer.FileSettlementKind,
	resumable resumestate.ResumableFileAuthority,
) (transfer.FileStart, error) {
	record := resumable.Bound().Record()
	binding, err := outputBindingForRecord(session.SessionID(), resumable.Descriptor(), record)
	if err != nil {
		return transfer.FileStart{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	checkpoint, err := transfer.VerifyDurableRanges(
		binding, transfer.CheckpointGeneration(record.CheckpointGeneration()), record.DurableRanges(),
	)
	if err != nil {
		return transfer.FileStart{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	settlement, err := transfer.NewVerifiedFileSettlement(kind, checkpoint)
	if err != nil {
		return transfer.FileStart{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	return transfer.NewFileSettlementStart(settlement)
}

func (session *filesystemOutputSession) collisionStart(file transfer.OutputFile) (transfer.FileStart, error) {
	settlement, err := transfer.NewCollisionFileSettlement(file.Target)
	if err != nil {
		return transfer.FileStart{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	return transfer.NewFileSettlementStart(settlement)
}

func (session *filesystemOutputSession) quarantinedStart(
	target transfer.OutputFileTarget,
	digest resumestate.LocatorDigest,
	reason transfer.QuarantineReason,
) (transfer.FileStart, error) {
	reference, err := transfer.NewOutputStateRef(session.SessionID(), digest.OutputLocatorDigest())
	if err != nil {
		return transfer.FileStart{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	settlement, err := transfer.NewImmediateQuarantinedFileSettlement(target, reference, reason)
	if err != nil {
		return transfer.FileStart{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	return transfer.NewFileSettlementStart(settlement)
}

func (session *filesystemOutputSession) quarantineRecoveryStart(
	file transfer.OutputFile,
	recordDir outputV3Directory,
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

func (session *filesystemOutputSession) quarantineRecoveryStartWithCleanup(
	file transfer.OutputFile,
	recordDir outputV3Directory,
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

func (session *filesystemOutputSession) installUnsafeNamespaceQuarantine(
	recordDir outputV3Directory,
	recordName string,
	bound resumestate.BoundFileRecord,
	reason resumestate.QuarantineReason,
) (resumestate.BoundFileRecord, error) {
	quarantined, err := resumestate.PrepareUnsafeNamespaceQuarantine(bound, reason)
	if err != nil {
		return resumestate.BoundFileRecord{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if err := session.installFileRecord(recordDir, recordName, bound, quarantined); err != nil {
		// The original ambiguity remains unresolved until a fresh owner reopens
		// this exact state record, so even a safely unchanged CAS must pause the job.
		return resumestate.BoundFileRecord{}, pauseRequiredFileOutputFault(err)
	}
	return quarantined, nil
}

func (session *filesystemOutputSession) openPublicationWitness(
	record resumestate.FileRecord,
	expected outputV3File,
) (*outputPublicationWitness, error, error) {
	stageName := resumestate.StageName(record.OutputObject())
	stageDir, present, err := openOutputShard(session.stagesDir, stageName.Shard(), false)
	if err != nil {
		return nil, err, closeOutputV3Directory(stageDir)
	}
	if !present {
		return nil, errors.Join(errOutputV3Unsafe, fs.ErrNotExist), closeOutputV3Directory(stageDir)
	}
	anchorName := resumestate.AnchorName(record.OutputObject())
	anchorDir, present, err := openOutputShard(session.anchorsDir, anchorName.Shard(), false)
	if err != nil {
		return nil, err, errors.Join(stageDir.Close(), closeOutputV3Directory(anchorDir))
	}
	if !present {
		return nil, errors.Join(errOutputV3Unsafe, fs.ErrNotExist),
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
	stageDir outputV3Directory,
	anchorDir outputV3Directory,
	expected outputV3File,
) (*outputPublicationWitness, error) {
	witness, operationErr, cleanupErr := openPublicationWitnessInDirectoriesResult(
		record, stageDir, anchorDir, expected,
	)
	return witness, errors.Join(operationErr, cleanupErr)
}

func openPublicationWitnessInDirectoriesResult(
	record resumestate.FileRecord,
	stageDir outputV3Directory,
	anchorDir outputV3Directory,
	expected outputV3File,
) (*outputPublicationWitness, error, error) {
	if stageDir == nil || anchorDir == nil {
		return nil, errors.Join(errOutputV3Unsafe, errors.New("publication witness directory is absent")), nil
	}
	stageName := resumestate.StageName(record.OutputObject())
	stage, err := stageDir.OpenFile(stageName.Name(), true, false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errOutputV3Unsafe) {
			err = errors.Join(errOutputV3Unsafe, err)
		}
		return nil, err, closeOutputV3File(stage)
	}
	witness := &outputPublicationWitness{stage: stage}
	fail := func(cause error, unsafe bool) (*outputPublicationWitness, error, error) {
		if unsafe {
			cause = errors.Join(errOutputV3Unsafe, cause)
		}
		return nil, cause, witness.Close()
	}
	anchorName := resumestate.AnchorName(record.OutputObject())
	anchor, err := anchorDir.OpenFile(anchorName.Name(), true, false)
	if err != nil {
		return fail(err, errors.Is(err, fs.ErrNotExist) || errors.Is(err, errOutputV3Unsafe))
	}
	witness.anchor = anchor
	stageSize, stageErr := stage.Size()
	anchorSize, anchorErr := anchor.Size()
	if stageErr != nil || anchorErr != nil {
		return fail(
			errors.Join(stageErr, anchorErr),
			errors.Is(stageErr, errOutputV3Unsafe) || errors.Is(anchorErr, errOutputV3Unsafe),
		)
	}
	if stageSize != record.ExactSize() || anchorSize != record.ExactSize() {
		return fail(errors.New("publication stage or anchor size differs from its record"), true)
	}
	same, sameErr := stage.SameFile(anchor)
	if sameErr != nil {
		return fail(sameErr, errors.Is(sameErr, errOutputV3Unsafe))
	}
	if !same {
		return fail(errors.New("publication stage and anchor identify different objects"), true)
	}
	if expected != nil {
		stageMatches, stageMatchErr := stage.SameFile(expected)
		anchorMatches, anchorMatchErr := anchor.SameFile(expected)
		if stageMatchErr != nil || anchorMatchErr != nil {
			return fail(
				errors.Join(stageMatchErr, anchorMatchErr),
				errors.Is(stageMatchErr, errOutputV3Unsafe) || errors.Is(anchorMatchErr, errOutputV3Unsafe),
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
		return fail(metadataErr, errors.Is(metadataErr, errOutputV3Unsafe))
	}
	if !metadataMatches {
		return fail(errors.New("publication witness metadata differs from its record"), true)
	}
	return witness, nil, nil
}

func (session *filesystemOutputSession) linkFinalNoReplace(
	bound resumestate.BoundFileRecord,
	expected outputV3File,
) (resumestate.PublishResult, error) {
	result, operationErr, cleanupErr := session.linkFinalNoReplaceResult(bound, expected)
	if errors.Is(operationErr, errOutputAncestryUnsafe) {
		return 0, outputAncestryPauseFault("revalidate final publication", operationErr)
	}
	return result, errors.Join(operationErr, cleanupErr)
}

func (session *filesystemOutputSession) linkFinalNoReplaceResult(
	bound resumestate.BoundFileRecord,
	expected outputV3File,
) (result resumestate.PublishResult, operationErr error, cleanupErr error) {
	record := bound.Record()
	if expected == nil {
		return 0, errors.Join(errOutputV3Unsafe, errors.New("publication source handle is absent")), nil
	}
	// Linux must link by a name beneath the pinned private anchor directory, while
	// Windows can link directly from the source handle. In both cases, reopen both
	// private names and compare them with the retained witness immediately before
	// invoking the platform primitive.
	witness, witnessErr, witnessCleanupErr := session.openPublicationWitness(record, expected)
	if witnessErr != nil || witnessCleanupErr != nil {
		return 0, witnessErr, witnessCleanupErr
	}
	defer func() { cleanupErr = errors.Join(cleanupErr, witness.Close()) }()
	parentPath, leaf, err := outputLocatorParentAndLeaf(record.CanonicalLocator())
	if err != nil {
		return 0, err, nil
	}
	requirement := outputAncestryRequirement{path: parentPath, authority: outputAncestryCreateAuthority}
	validation, err := session.validateOutputAncestry(requirement)
	if err != nil {
		session.traceOutputAncestry(FilesystemOutputAncestryPublicationPre, record.LocatorDigest(), err)
		return 0, err, nil
	}
	session.traceOutputAncestry(FilesystemOutputAncestryPublicationPre, record.LocatorDigest(), nil)
	defer func() {
		revalidateErr := validation.Revalidate(requirement)
		closeErr := closeOutputAncestryValidation(validation)
		session.traceOutputAncestry(
			FilesystemOutputAncestryPublicationPost, record.LocatorDigest(),
			errors.Join(revalidateErr, closeErr),
		)
		if revalidateErr != nil {
			result = 0
			operationErr = errors.Join(operationErr, revalidateErr)
		}
		if closeErr != nil {
			cleanupErr = errors.Join(cleanupErr, closeErr)
		}
	}()
	parent, err := validation.directory(parentPath)
	if err != nil {
		return 0, err, nil
	}
	if err := validation.revalidateRetainedDirectory(parentPath, outputAncestryCreateAuthority); err != nil {
		session.traceOutputAncestry(FilesystemOutputAncestryPublicationPre, record.LocatorDigest(), err)
		return 0, err, nil
	}
	linked, err := parent.LinkFileNoReplace(expected, leaf)
	if err == nil {
		if linked == nil {
			return 0, errors.Join(errOutputV3Unsafe, errors.New("publication returned no final handle")), nil
		}
		same, sameErr := linked.SameFile(expected)
		metadataMatches, metadataErr := linked.MetadataMatches(
			record.ExactSize(), record.ExpectedMetadata().ModifiedTime,
		)
		if sameErr != nil || metadataErr != nil || !same || !metadataMatches {
			return 0, errors.Join(errOutputV3Unsafe, sameErr, metadataErr), linked.Close()
		}
		return resumestate.PublishLinkCreated, parent.Sync(), linked.Close()
	}
	linkedCloseErr := closeOutputV3File(linked)
	if !errors.Is(err, errOutputV3Collision) {
		return 0, err, linkedCloseErr
	}
	kind, observeErr := parent.ObserveEntry(leaf)
	if observeErr != nil {
		if classifyOutputV3RecoveryFailure(
			observeErr, outputV3ExistingEntryUnclassified,
		) == outputV3RecoveryAmbiguous {
			return resumestate.PublishExistingAmbiguous, nil, linkedCloseErr
		}
	}
	if kind == outputV3EntryAbsent {
		return resumestate.PublishExistingAmbiguous, nil, linkedCloseErr
	}
	if kind != outputV3EntryRegularFile {
		return resumestate.PublishAlreadyExistsDifferent, nil, linkedCloseErr
	}
	final, openErr := parent.OpenFile(leaf, false, false)
	if openErr != nil {
		return resumestate.PublishExistingAmbiguous, nil, errors.Join(linkedCloseErr, closeOutputV3File(final))
	}
	defer func() { cleanupErr = errors.Join(cleanupErr, final.Close()) }()
	same, sameErr := final.SameFile(expected)
	if sameErr != nil {
		return resumestate.PublishExistingAmbiguous, nil, linkedCloseErr
	}
	if same {
		metadataMatches, metadataErr := final.MetadataMatches(
			record.ExactSize(), record.ExpectedMetadata().ModifiedTime,
		)
		if metadataErr != nil || !metadataMatches {
			return resumestate.PublishExistingAmbiguous, nil, linkedCloseErr
		}
		return resumestate.PublishLinkCreated, parent.Sync(), linkedCloseErr
	}
	return resumestate.PublishAlreadyExistsDifferent, nil, linkedCloseErr
}

func (session *filesystemOutputSession) syncFinalParent(
	locator string,
) (operationErr error, cleanupErr error) {
	parentPath := outputLocatorParentPath(locator)
	locatorDigest := resumestate.DigestCanonicalLocator(locator)
	requirement := outputAncestryRequirement{path: parentPath, authority: outputAncestryCreateAuthority}
	validation, err := session.validateOutputAncestry(requirement)
	if err != nil {
		session.traceOutputAncestry(FilesystemOutputAncestryPublicationPost, locatorDigest, err)
		return err, nil
	}
	defer func() {
		revalidateErr := validation.Revalidate(requirement)
		closeErr := closeOutputAncestryValidation(validation)
		session.traceOutputAncestry(
			FilesystemOutputAncestryPublicationPost, locatorDigest,
			errors.Join(revalidateErr, closeErr),
		)
		operationErr = errors.Join(operationErr, revalidateErr)
		cleanupErr = errors.Join(cleanupErr, closeErr)
	}()
	parent, err := validation.directory(parentPath)
	if err != nil {
		return err, nil
	}
	if err := validation.revalidateRetainedDirectory(parentPath, outputAncestryCreateAuthority); err != nil {
		session.traceOutputAncestry(FilesystemOutputAncestryPublicationPost, locatorDigest, err)
		return err, nil
	}
	return parent.Sync(), nil
}

// recoverFinalParentSync gives the publication evidence precedence over later
// cleanup failures. A positive structural contradiction makes the observed
// final path ambiguous and must be quarantined, while inability to perform the
// operation proves nothing about identity and therefore preserves the current
// publishing cut for retry.
func (session *filesystemOutputSession) recoverFinalParentSync(
	file transfer.OutputFile,
	recordDir outputV3Directory,
	recordName string,
	bound resumestate.BoundFileRecord,
	operation string,
) (transfer.FileStart, bool, error) {
	operationErr, cleanupErr := session.syncFinalParent(bound.Record().CanonicalLocator())
	if errors.Is(operationErr, errOutputAncestryUnsafe) {
		return transfer.FileStart{}, true, pauseRequiredFileOperationFault(operation, operationErr, cleanupErr)
	}
	if operationErr == nil {
		if cleanupErr != nil {
			return transfer.FileStart{}, true, pauseRequiredFileOperationFault(operation, nil, cleanupErr)
		}
		return transfer.FileStart{}, false, nil
	}
	if classifyOutputV3RecoveryFailure(
		operationErr, outputV3AuthorizedMutation,
	) == outputV3RecoveryPauseRequired {
		return transfer.FileStart{}, true, pauseRequiredFileOperationFault(operation, operationErr, cleanupErr)
	}

	start, quarantineErr := session.quarantineRecoveryStart(
		file, recordDir, recordName, bound, resumestate.QuarantineFinalUnsafe,
	)
	if quarantineErr != nil {
		return transfer.FileStart{}, true, errors.Join(
			quarantineErr,
			pauseRequiredFileOperationFault("quarantined "+operation, nil, cleanupErr),
		)
	}
	if cleanupErr != nil {
		return transfer.FileStart{}, true, pauseRequiredFileOperationFault(
			"quarantined "+operation, nil, cleanupErr,
		)
	}
	return start, true, nil
}

func (session *filesystemOutputSession) removeStage(record resumestate.FileRecord) (error, error) {
	stageName := resumestate.StageName(record.OutputObject())
	directory, present, err := openOutputShard(session.stagesDir, stageName.Shard(), false)
	if err != nil {
		return errors.Join(errOutputV3PositiveEntryEvidence, err), closeOutputV3Directory(directory)
	}
	if !present {
		return errors.Join(errOutputV3Unsafe, fs.ErrNotExist), closeOutputV3Directory(directory)
	}
	stage, err := directory.OpenFile(stageName.Name(), true, false)
	if err != nil {
		return errors.Join(errOutputV3PositiveEntryEvidence, err),
			errors.Join(closeOutputV3File(stage), directory.Close())
	}
	operationErr := directory.RemoveFile(stageName.Name(), stage)
	if operationErr == nil {
		operationErr = directory.Sync()
	}
	return operationErr, errors.Join(stage.Close(), directory.Close())
}

func (session *filesystemOutputSession) syncObjectShard(
	parent outputV3Directory,
	name resumestate.ShardedName,
) (error, error) {
	directory, present, err := openOutputShard(parent, name.Shard(), false)
	if err != nil {
		return err, closeOutputV3Directory(directory)
	}
	if !present {
		return parent.Sync(), closeOutputV3Directory(directory)
	}
	return directory.Sync(), directory.Close()
}

func (session *filesystemOutputSession) retireBoundFile(
	recordDir outputV3Directory,
	recordName string,
	bound resumestate.BoundFileRecord,
	binding transfer.OutputFileBinding,
) (resultSettlement transfer.FileSettlement, resultQuarantined bool, resultErr error) {
	requirement := outputAncestryRequirement{}
	validation, err := session.validateOutputAncestry(requirement)
	if err != nil {
		session.traceOutputAncestry(FilesystemOutputAncestryRecovery, bound.Record().LocatorDigest(), err)
		return transfer.FileSettlement{}, false,
			outputAncestryOperationFault("validate ancestry before file retirement", err)
	}
	defer func() {
		ancestryErr := finishOutputAncestryOperation(
			session, validation, requirement, FilesystemOutputAncestryRecovery,
			bound.Record().LocatorDigest(), "finish file retirement ancestry", nil,
		)
		if ancestryErr != nil {
			resultSettlement = transfer.FileSettlement{}
			resultQuarantined = false
			resultErr = errors.Join(resultErr, ancestryErr)
		}
	}()
	for {
		observation, observationCleanupErr, observationErr := session.observeFile(
			validation, bound.Record(), false,
		)
		if observationErr != nil {
			return transfer.FileSettlement{}, false, pauseRequiredFileOutputFault(fileOutputFault(
				"observe file retirement", errors.Join(observationErr, observationCleanupErr),
			))
		}
		decision, err := resumestate.ReduceFileRecovery(bound, observation)
		if err != nil {
			return transfer.FileSettlement{}, false, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
		}
		session.owner.trace(FilesystemOutputTrace{
			Operation: TraceFileRecoveryDecision, ResumeIntent: session.resumeIntent,
			SessionID: session.SessionID(), LocatorDigest: outputLocatorDigestFromState(bound.Record().LocatorDigest()),
			OutputObjectID:   outputObjectIdentityFromState(bound.Record().OutputObject()),
			PreviousPhase:    filesystemOutputFilePhaseFromState(bound.Record().Phase()),
			RecoveryAction:   filesystemOutputRecoveryActionFromState(decision.Action()),
			QuarantineReason: recoveryDecisionQuarantineReason(decision),
		})
		quarantineDecision := decision.Action() == resumestate.RecoveryInstallQuarantine ||
			decision.Action() == resumestate.RecoveryHoldQuarantine
		if observationCleanupErr != nil && !quarantineDecision {
			return transfer.FileSettlement{}, false, pauseRequiredFileOutputFault(fileOutputFault(
				"close retiring-file observation", observationCleanupErr,
			))
		}
		switch decision.Action() {
		case resumestate.RecoveryRemoveRetiringStageAndSync:
			operationErr, cleanupErr := session.removeStage(bound.Record())
			if operationErr != nil {
				return transfer.FileSettlement{}, false, pauseRequiredFileOperationFault(
					"remove retiring stage", operationErr, cleanupErr,
				)
			}
			if cleanupErr != nil {
				return transfer.FileSettlement{}, false, pauseRequiredFileOperationFault(
					"close removed retiring stage", nil, cleanupErr,
				)
			}
		case resumestate.RecoverySyncStageRemoveAnchorAndSync:
			stageName := resumestate.StageName(bound.Record().OutputObject())
			operationErr, cleanupErr := session.syncObjectShard(session.stagesDir, stageName)
			if operationErr != nil {
				return transfer.FileSettlement{}, false, pauseRequiredFileOperationFault(
					"sync retiring stage shard", operationErr, cleanupErr,
				)
			}
			if cleanupErr != nil {
				return transfer.FileSettlement{}, false, pauseRequiredFileOperationFault(
					"close synced retiring-stage shard", nil, cleanupErr,
				)
			}
			anchorName := resumestate.AnchorName(bound.Record().OutputObject())
			anchorDir, present, openErr := openOutputShard(session.anchorsDir, anchorName.Shard(), false)
			if openErr != nil || !present {
				operationErr := openErr
				if !present {
					operationErr = errors.Join(errOutputV3Unsafe, fs.ErrNotExist)
				}
				cleanupErr := closeOutputV3Directory(anchorDir)
				return transfer.FileSettlement{}, false, pauseRequiredFileOperationFault(
					"open retiring anchor shard", operationErr, cleanupErr,
				)
			}
			anchor, openErr := anchorDir.OpenFile(anchorName.Name(), true, false)
			if openErr != nil {
				return transfer.FileSettlement{}, false, pauseRequiredFileOperationFault(
					"open retiring anchor", openErr,
					errors.Join(closeOutputV3File(anchor), anchorDir.Close()),
				)
			}
			removeErr := anchorDir.RemoveFile(anchorName.Name(), anchor)
			var syncErr error
			if removeErr == nil {
				syncErr = anchorDir.Sync()
			}
			operationErr = errors.Join(removeErr, syncErr)
			closeErr := errors.Join(anchor.Close(), anchorDir.Close())
			if operationErr != nil {
				return transfer.FileSettlement{}, false, pauseRequiredFileOperationFault(
					"remove retiring anchor", operationErr, closeErr,
				)
			}
			if closeErr != nil {
				return transfer.FileSettlement{}, false, pauseRequiredFileOperationFault(
					"close removed retiring anchor", nil, closeErr,
				)
			}
		case resumestate.RecoverySyncParentsRemoveRecordAndSync:
			operationErr, cleanupErr := session.syncObjectShard(
				session.stagesDir, resumestate.StageName(bound.Record().OutputObject()),
			)
			if operationErr != nil {
				return transfer.FileSettlement{}, false, pauseRequiredFileOperationFault(
					"sync retiring stage parent", operationErr, cleanupErr,
				)
			}
			if cleanupErr != nil {
				return transfer.FileSettlement{}, false, pauseRequiredFileOperationFault(
					"close synced retiring-stage parent", nil, cleanupErr,
				)
			}
			operationErr, cleanupErr = session.syncObjectShard(
				session.anchorsDir, resumestate.AnchorName(bound.Record().OutputObject()),
			)
			if operationErr != nil {
				return transfer.FileSettlement{}, false, pauseRequiredFileOperationFault(
					"sync retiring anchor parent", operationErr, cleanupErr,
				)
			}
			if cleanupErr != nil {
				return transfer.FileSettlement{}, false, pauseRequiredFileOperationFault(
					"close synced retiring-anchor parent", nil, cleanupErr,
				)
			}
			operationErr, cleanupErr = removeBoundFileRecord(recordDir, recordName, bound)
			if operationErr != nil {
				if classifyOutputV3RecoveryFailure(
					operationErr, outputV3AuthorizedMutation,
				) == outputV3RecoveryAmbiguous {
					session.poisonState()
					return transfer.FileSettlement{}, false, pauseRequiredFileOperationFault(
						"remove retiring file state with uncertain authority", operationErr, cleanupErr,
					)
				}
				return transfer.FileSettlement{}, false, pauseRequiredFileOperationFault(
					"remove retiring file state", operationErr, cleanupErr,
				)
			}
			if cleanupErr != nil {
				return transfer.FileSettlement{}, false, pauseRequiredFileOperationFault(
					"close removed retiring file state", nil, cleanupErr,
				)
			}
			if binding.BackendID() == "" {
				return transfer.FileSettlement{}, false, nil
			}
			if decision.Settlement() == resumestate.RecoveryCollision {
				settlement, err := transfer.NewCollisionFileSettlement(binding.Target())
				if err != nil {
					return transfer.FileSettlement{}, false, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
				}
				return settlement, false, nil
			}
			settlement, err := transfer.NewRetiredFileSettlement(binding)
			if err != nil {
				return transfer.FileSettlement{}, false, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
			}
			return settlement, false, nil
		case resumestate.RecoveryHoldRetiringCleanup:
			return transfer.FileSettlement{}, false, internalCleanupNeedsAttentionFault(
				"hold retiring file with ambiguous internal cleanup evidence",
			)
		case resumestate.RecoveryInstallQuarantine:
			next, err := resumestate.ApplyRecoveryDecision(bound, decision)
			if err != nil {
				return transfer.FileSettlement{}, false, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
			}
			if err := session.installFileRecord(recordDir, recordName, bound, next); err != nil {
				return transfer.FileSettlement{}, false, err
			}
			if observationCleanupErr != nil {
				return transfer.FileSettlement{}, true, pauseRequiredFileOutputFault(fileOutputFault(
					"close quarantined retirement observation", observationCleanupErr,
				))
			}
			if binding.BackendID() == "" {
				return transfer.FileSettlement{}, true, nil
			}
			settlement, err := quarantinedSettlement(binding, next.Record())
			return settlement, true, err
		case resumestate.RecoveryHoldQuarantine:
			if observationCleanupErr != nil {
				return transfer.FileSettlement{}, true, pauseRequiredFileOutputFault(fileOutputFault(
					"close held retirement observation", observationCleanupErr,
				))
			}
			if binding.BackendID() == "" {
				return transfer.FileSettlement{}, true, nil
			}
			settlement, err := quarantinedSettlement(binding, bound.Record())
			return settlement, true, err
		default:
			return transfer.FileSettlement{}, false, outputFault(
				transfer.OutputFaultFile, transfer.OutputFaultContract,
				fmt.Errorf("unexpected retirement action %d", decision.Action()),
			)
		}
	}
}

func removeBoundFileRecord(
	directory outputV3Directory,
	name string,
	bound resumestate.BoundFileRecord,
) (error, error) {
	expected, err := resumestate.EncodeFileRecord(bound)
	if err != nil {
		return err, nil
	}
	file, err := directory.OpenFile(name, true, false)
	if err != nil {
		return err, closeOutputV3File(file)
	}
	actual, err := readStateFile(file, resumestate.MaxFileStateBytes)
	if err != nil || !bytes.Equal(actual, expected) {
		return errors.Join(errOutputV3Unsafe, err), file.Close()
	}
	operationErr := directory.RemoveFile(name, file)
	if operationErr == nil {
		operationErr = directory.Sync()
	}
	return operationErr, file.Close()
}

func quarantinedSettlement(
	binding transfer.OutputFileBinding,
	record resumestate.FileRecord,
) (transfer.FileSettlement, error) {
	reference, err := transfer.NewOutputStateRef(binding.OutputSessionID(), record.LocatorDigest().OutputLocatorDigest())
	if err != nil {
		return transfer.FileSettlement{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	settlement, err := transfer.NewTransactionQuarantinedFileSettlement(
		binding, reference, mapQuarantineReason(record.QuarantineReason()),
	)
	if err != nil {
		return transfer.FileSettlement{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	return settlement, nil
}

func mapQuarantineReason(reason resumestate.QuarantineReason) transfer.QuarantineReason {
	switch reason {
	case resumestate.QuarantinePublicationHistory, resumestate.QuarantineFinalMismatch,
		resumestate.QuarantineFinalUnsafe, resumestate.QuarantineMetadataMismatch:
		return transfer.QuarantinePublicationAmbiguous
	case resumestate.QuarantinePartialObjectCreation:
		return transfer.QuarantineRetirementMismatch
	case resumestate.QuarantineUpdateTemporary, resumestate.QuarantineOutputObjectDuplicate:
		return transfer.QuarantineStateCorrupt
	default:
		return transfer.QuarantineOwnershipMismatch
	}
}

func openOutputShard(
	parent outputV3Directory,
	name string,
	create bool,
) (outputV3Directory, bool, error) {
	if !validStateShard(name) {
		return nil, false, errOutputV3Unsafe
	}
	if create {
		directory, _, err := ensureOutputDirectory(parent, name, true)
		return directory, err == nil, err
	}
	directory, present, err := openOptionalOutputDirectory(parent, name, true)
	return directory, present, err
}

func (transaction *filesystemFileTransaction) Binding() transfer.OutputFileBinding {
	if transaction == nil {
		return transfer.OutputFileBinding{}
	}
	return transaction.binding
}

func (transaction *filesystemFileTransaction) WriteRange(
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
	if transaction.lifecycle != filesystemFileTransactionOpen || transaction.session.operationDisabled() || transaction.data == nil ||
		transaction.resumable.Bound().Record().Phase() != resumestate.FileWitnessed {
		return outputFault(transfer.OutputFaultFile, transfer.OutputFaultOwnership, errOutputSessionClosed)
	}
	if len(data) == 0 {
		return nil
	}
	if offset > transaction.binding.ExactSize() || uint64(len(data)) > transaction.binding.ExactSize()-offset ||
		offset > math.MaxInt64 || uint64(len(data)) > math.MaxInt64-offset {
		return outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, ErrOutOfRange)
	}
	end := offset + uint64(len(data))
	record := transaction.resumable.Bound().Record()
	if rangeSetIntersects(record.DurableRanges(), offset, end) || rangeSetIntersects(transaction.pending, offset, end) {
		// A verified checkpoint remains authoritative across process restart. Any
		// later overwrite would let that old record authenticate different bytes;
		// pending writes are equally single-owner because the protocol has no
		// idempotent rewrite proof.
		return outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, errOutputV3RangeOverlap)
	}
	writtenRange, _ := content.NewRangeSet([]content.Range{{Offset: offset, End: end}})
	pending, err := transfer.MergeRanges(transaction.pending, writtenRange)
	if err != nil || pending.Len() > resumestate.MaxDurableRangesPerFile {
		return outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, errors.Join(err, resumestate.ErrInvalidState))
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

func (transaction *filesystemFileTransaction) Checkpoint(
	ctx context.Context,
) (transfer.VerifiedDurableRanges, error) {
	if err := ctx.Err(); err != nil {
		return transfer.VerifiedDurableRanges{}, err
	}
	if transaction == nil {
		return transfer.VerifiedDurableRanges{}, transfer.ErrInvalidOutputBinding
	}
	if err := transaction.session.beginOperation(); err != nil {
		return transfer.VerifiedDurableRanges{}, err
	}
	defer transaction.session.endOperation()
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	if transaction.lifecycle != filesystemFileTransactionOpen || transaction.session.operationDisabled() {
		return transfer.VerifiedDurableRanges{}, outputFault(
			transfer.OutputFaultFile, transfer.OutputFaultOwnership, errOutputSessionClosed,
		)
	}
	return transaction.checkpointLocked()
}

func (transaction *filesystemFileTransaction) checkpointLocked() (transfer.VerifiedDurableRanges, error) {
	transaction.session.mu.Lock()
	poisoned := transaction.session.poisoned
	transaction.session.mu.Unlock()
	if poisoned {
		return transfer.VerifiedDurableRanges{}, outputFault(
			transfer.OutputFaultFile, transfer.OutputFaultOwnership, errOutputSessionClosed,
		)
	}
	if transaction.lifecycle != filesystemFileTransactionOpen &&
		transaction.lifecycle != filesystemFileTransactionSettling || transaction.data == nil || transaction.anchor == nil {
		return transfer.VerifiedDurableRanges{}, outputFault(
			transfer.OutputFaultFile, transfer.OutputFaultOwnership, errOutputSessionClosed,
		)
	}
	record := transaction.resumable.Bound().Record()
	if transaction.pending.IsEmpty() {
		return transfer.VerifyDurableRanges(
			transaction.binding, transfer.CheckpointGeneration(record.CheckpointGeneration()), record.DurableRanges(),
		)
	}
	merged, err := transfer.MergeRanges(record.DurableRanges(), transaction.pending)
	if err != nil || merged.Len() > resumestate.MaxDurableRangesPerFile {
		return transfer.VerifiedDurableRanges{}, outputFault(
			transfer.OutputFaultFile, transfer.OutputFaultContract, errors.Join(err, resumestate.ErrInvalidState),
		)
	}
	if err := transaction.data.Sync(); err != nil {
		return transfer.VerifiedDurableRanges{}, fileOutputFault("sync checkpoint data", err)
	}
	same, err := transaction.data.SameFile(transaction.anchor)
	if err != nil {
		if !errors.Is(err, errOutputV3Unsafe) {
			return transfer.VerifiedDurableRanges{}, pauseRequiredFileOutputFault(fileOutputFault(
				"compare checkpoint witness", err,
			))
		}
		if _, quarantineErr := transaction.installWitnessQuarantineLocked(resumestate.QuarantineStageUnsafe); quarantineErr != nil {
			return transfer.VerifiedDurableRanges{}, quarantineErr
		}
		transaction.session.poisonState()
		return transfer.VerifiedDurableRanges{}, pauseRequiredFileOutputFault(fileOutputFault(
			"verify checkpoint witness identity", errors.Join(errOutputV3Unsafe, err),
		))
	}
	if !same {
		if _, quarantineErr := transaction.installWitnessQuarantineLocked(resumestate.QuarantineStageUnsafe); quarantineErr != nil {
			return transfer.VerifiedDurableRanges{}, quarantineErr
		}
		transaction.session.poisonState()
		return transfer.VerifiedDurableRanges{}, pauseRequiredFileOutputFault(fileOutputFault(
			"verify checkpoint witness identity", errOutputV3Unsafe,
		))
	}
	candidate, err := transaction.resumable.WithCheckpoint(record.CheckpointGeneration()+1, merged)
	if err != nil {
		return transfer.VerifiedDurableRanges{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if err := transaction.session.installFileRecord(
		transaction.recordDir, transaction.recordName, transaction.resumable.Bound(), candidate.Bound(),
	); err != nil {
		return transfer.VerifiedDurableRanges{}, err
	}
	transaction.resumable = candidate
	transaction.pending, _ = content.NewRangeSet(nil)
	return transfer.VerifyDurableRanges(
		transaction.binding, transfer.CheckpointGeneration(candidate.Bound().Record().CheckpointGeneration()), merged,
	)
}

func (transaction *filesystemFileTransaction) installWitnessQuarantineLocked(
	reason resumestate.QuarantineReason,
) (transfer.FileSettlement, error) {
	quarantined, err := transaction.session.installUnsafeNamespaceQuarantine(
		transaction.recordDir, transaction.recordName, transaction.resumable.Bound(), reason,
	)
	if err != nil {
		return transfer.FileSettlement{}, err
	}
	transaction.resumable, err = resumestate.BindResumableFile(quarantined, transaction.descriptor)
	if err != nil {
		return transfer.FileSettlement{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	return quarantinedSettlement(transaction.binding, quarantined.Record())
}

func (transaction *filesystemFileTransaction) installWitnessQuarantineWithCleanupLocked(
	reason resumestate.QuarantineReason,
	cleanupOperation string,
	cleanupErr error,
) (transfer.FileSettlement, bool, error) {
	settlement, quarantineErr := transaction.installWitnessQuarantineLocked(reason)
	if quarantineErr != nil {
		if cleanupErr != nil {
			quarantineErr = errors.Join(
				quarantineErr,
				pauseRequiredFileOperationFault(cleanupOperation, nil, cleanupErr),
			)
		}
		return transfer.FileSettlement{}, false, quarantineErr
	}
	if cleanupErr != nil {
		return transfer.FileSettlement{}, false, pauseRequiredFileOperationFault(
			cleanupOperation, nil, cleanupErr,
		)
	}
	return settlement, true, nil
}

func (transaction *filesystemFileTransaction) Commit(
	ctx context.Context,
) (settlement transfer.FileSettlement, resultErr error) {
	if err := ctx.Err(); err != nil {
		return transfer.FileSettlement{}, fileSettlementFailure(err)
	}
	if transaction == nil {
		return transfer.FileSettlement{}, outputFault(
			transfer.OutputFaultFile, transfer.OutputFaultContract, transfer.ErrInvalidOutputBinding,
		)
	}
	if err := transaction.session.beginOperation(); err != nil {
		return transfer.FileSettlement{}, err
	}
	defer transaction.session.endOperation()
	if err := transaction.claimTerminalSettlement(true); err != nil {
		return transfer.FileSettlement{}, err
	}
	defer func() {
		transaction.session.traceReturnedFileSettlement(filesystemOutputFileSettlementTraceContext{
			boundary: FilesystemOutputSettlementCommit,
		}, settlement, resultErr)
	}()
	var validation *outputAncestryValidation
	defer func() {
		transaction.finishTerminalResult(&resultErr, "close committed output")
		if validation == nil {
			return
		}
		ancestryErr := finishOutputAncestryOperation(
			transaction.session, validation,
			outputAncestryRequirement{
				path:      outputLocatorParentPath(transaction.binding.Locator().CanonicalPath()),
				authority: outputAncestryCreateAuthority,
			},
			FilesystemOutputAncestryPublicationPost,
			transaction.resumable.Bound().Record().LocatorDigest(),
			"finish committed output ancestry",
			nil,
		)
		if ancestryErr != nil {
			settlement = transfer.FileSettlement{}
			resultErr = errors.Join(resultErr, ancestryErr)
		}
	}()

	settlement, settled, err := transaction.preparePublication()
	if err != nil || settled {
		return settlement, err
	}
	parentPath := outputLocatorParentPath(transaction.binding.Locator().CanonicalPath())
	requirement := outputAncestryRequirement{path: parentPath, authority: outputAncestryCreateAuthority}
	validation, err = transaction.session.validateOutputAncestry(requirement)
	if err != nil {
		transaction.session.traceOutputAncestry(
			FilesystemOutputAncestryPublicationPre,
			transaction.resumable.Bound().Record().LocatorDigest(),
			err,
		)
		return transfer.FileSettlement{}, outputAncestryOperationFault(
			"validate ancestry before committing output", err,
		)
	}
	return transaction.commitSettling(ctx)
}

func (transaction *filesystemFileTransaction) preparePublication() (
	transfer.FileSettlement,
	bool,
	error,
) {
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	return transaction.preparePublicationLocked()
}

func (transaction *filesystemFileTransaction) commitSettling(
	ctx context.Context,
) (transfer.FileSettlement, error) {
	transaction.mu.Lock()
	settlement, settled, err := transaction.publishPreparedLocked()
	transaction.mu.Unlock()
	if err != nil || settled {
		return settlement, err
	}

	if transaction.reduceFile == nil {
		return transfer.FileSettlement{}, outputFault(
			transfer.OutputFaultFile, transfer.OutputFaultContract, transfer.ErrInvalidOutputSettlement,
		)
	}
	start, err := transaction.reduceFile(
		context.WithoutCancel(ctx),
		transfer.OutputFile{
			Path: transaction.binding.Locator().CanonicalPath(), ExpectedSize: transaction.binding.ExactSize(),
			Descriptor: transaction.descriptor, Target: transaction.binding.Target(),
		},
		transaction.resumable, transaction.recordDir, transaction.recordName,
	)
	if err != nil {
		return transfer.FileSettlement{}, err
	}
	settlement, ok := start.ImmediateSettlement()
	if !ok || settlement.Kind() != transfer.FilePublished && settlement.Kind() != transfer.FilePublishBlocked &&
		settlement.Kind() != transfer.FileQuarantined {
		return transfer.FileSettlement{}, outputFault(
			transfer.OutputFaultFile, transfer.OutputFaultContract, transfer.ErrInvalidOutputSettlement,
		)
	}
	if settlement.Kind() == transfer.FileQuarantined {
		reference, reason, valid := settlement.Quarantine()
		if !valid {
			return transfer.FileSettlement{}, outputFault(
				transfer.OutputFaultFile, transfer.OutputFaultContract, transfer.ErrInvalidOutputSettlement,
			)
		}
		settlement, err = transfer.NewTransactionQuarantinedFileSettlement(
			transaction.binding, reference, reason,
		)
		if err != nil {
			return transfer.FileSettlement{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
		}
		return settlement, nil
	}
	settledBinding, bound := settlement.OutputBinding()
	if !bound || settledBinding != transaction.binding {
		return transfer.FileSettlement{}, outputFault(
			transfer.OutputFaultFile, transfer.OutputFaultContract, transfer.ErrInvalidOutputSettlement,
		)
	}
	return settlement, nil
}

func (transaction *filesystemFileTransaction) preparePublicationLocked() (
	transfer.FileSettlement,
	bool,
	error,
) {
	if transaction.lifecycle != filesystemFileTransactionSettling || transaction.session.operationDisabled() {
		return transfer.FileSettlement{}, false, outputFault(
			transfer.OutputFaultFile, transfer.OutputFaultOwnership, errOutputSessionClosed,
		)
	}
	checkpoint, err := transaction.checkpointLocked()
	if err != nil {
		return transfer.FileSettlement{}, false, err
	}
	if !transfer.RangesCoverFile(transaction.binding.ExactSize(), checkpoint.Ranges()) {
		return transfer.FileSettlement{}, false, outputFault(
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
		if !errors.Is(sameErr, errOutputV3Unsafe) {
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
		if errors.Is(witnessErr, errOutputV3Unsafe) {
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
		return transfer.FileSettlement{}, false, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if err := transaction.session.installFileRecord(
		transaction.recordDir, transaction.recordName, transaction.resumable.Bound(), publishing,
	); err != nil {
		return transfer.FileSettlement{}, false, err
	}
	transaction.resumable, err = resumestate.BindResumableFile(publishing, transaction.descriptor)
	if err != nil {
		return transfer.FileSettlement{}, false, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	return transfer.FileSettlement{}, false, nil
}

func (transaction *filesystemFileTransaction) publishPreparedLocked() (
	transfer.FileSettlement,
	bool,
	error,
) {
	publishResult, linkErr, linkCleanupErr := transaction.session.linkFinalNoReplaceResult(
		transaction.resumable.Bound(), transaction.anchor,
	)
	if errors.Is(linkErr, errOutputAncestryUnsafe) {
		return transfer.FileSettlement{}, false,
			outputAncestryPauseFault("revalidate live final publication", linkErr)
	}
	if publishResult == 0 && linkErr != nil {
		if classifyOutputV3RecoveryFailure(linkErr, outputV3AuthorizedMutation) == outputV3RecoveryAmbiguous {
			return transaction.installWitnessQuarantineWithCleanupLocked(
				resumestate.QuarantinePublicationHistory,
				"close quarantined final-link evidence",
				linkCleanupErr,
			)
		}
		return transfer.FileSettlement{}, false, pauseRequiredFileOperationFault(
			"publish final link", linkErr, linkCleanupErr,
		)
	}
	if publishResult == 0 {
		return transfer.FileSettlement{}, false, pauseRequiredFileOperationFault(
			"close unclassified final-link evidence", nil, linkCleanupErr,
		)
	}
	if publishResult == resumestate.PublishLinkCreated {
		if linkErr != nil || linkCleanupErr != nil {
			return transfer.FileSettlement{}, false, pauseRequiredFileOperationFault(
				"finish final publication", linkErr, linkCleanupErr,
			)
		}
		return transfer.FileSettlement{}, false, nil
	}
	decision, reduceErr := resumestate.ReducePublishResult(
		transaction.resumable.Bound(), publishResult,
	)
	if reduceErr != nil {
		return transfer.FileSettlement{}, false, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, reduceErr)
	}
	publishBlocked, applyErr := resumestate.ApplyRecoveryDecision(transaction.resumable.Bound(), decision)
	if applyErr != nil {
		return transfer.FileSettlement{}, false, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, applyErr)
	}
	if err := transaction.session.installFileRecord(
		transaction.recordDir, transaction.recordName, transaction.resumable.Bound(), publishBlocked,
	); err != nil {
		return transfer.FileSettlement{}, false, err
	}
	resumable, bindErr := resumestate.BindResumableFile(publishBlocked, transaction.descriptor)
	if bindErr != nil {
		return transfer.FileSettlement{}, false, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, bindErr)
	}
	transaction.resumable = resumable
	if linkErr != nil {
		return transfer.FileSettlement{}, false, pauseRequiredFileOperationFault(
			"settle classified publication evidence", linkErr, linkCleanupErr,
		)
	}
	if linkCleanupErr != nil {
		return transfer.FileSettlement{}, false, pauseRequiredFileOperationFault(
			"close classified publication evidence", nil, linkCleanupErr,
		)
	}
	if publishResult == resumestate.PublishExistingAmbiguous {
		settlement, settleErr := quarantinedSettlement(transaction.binding, publishBlocked.Record())
		return settlement, true, settleErr
	}
	start, startErr := transaction.session.verifiedStart(transfer.FilePublishBlocked, transaction.resumable)
	if startErr != nil {
		return transfer.FileSettlement{}, false, startErr
	}
	settlement, ok := start.ImmediateSettlement()
	if !ok {
		return transfer.FileSettlement{}, false, outputFault(
			transfer.OutputFaultFile, transfer.OutputFaultContract, transfer.ErrInvalidOutputSettlement,
		)
	}
	return settlement, true, nil
}

func (transaction *filesystemFileTransaction) Pause(
	ctx context.Context,
	reason transfer.FilePauseReason,
) (settlement transfer.FileSettlement, resultErr error) {
	if transaction == nil || reason < transfer.FilePauseInterrupted || reason > transfer.FilePauseOutputFailure {
		return transfer.FileSettlement{}, outputFault(
			transfer.OutputFaultFile, transfer.OutputFaultContract, transfer.ErrInvalidOutputSettlement,
		)
	}
	if err := transaction.session.beginOperation(); err != nil {
		return transfer.FileSettlement{}, err
	}
	defer transaction.session.endOperation()
	return transaction.pause(ctx, reason, true, FilesystemOutputSettlementPause)
}

func (transaction *filesystemFileTransaction) pauseForSessionSettlement(
	ctx context.Context,
	reason transfer.FilePauseReason,
) (transfer.FileSettlement, error) {
	return transaction.pause(ctx, reason, false, FilesystemOutputSettlementJobPause)
}

func (transaction *filesystemFileTransaction) pauseForBeginFileCleanup(
	ctx context.Context,
	reason transfer.FilePauseReason,
) (transfer.FileSettlement, error) {
	return transaction.pause(ctx, reason, false, FilesystemOutputSettlementBeginFileCleanup)
}

func (transaction *filesystemFileTransaction) pause(
	ctx context.Context,
	reason transfer.FilePauseReason,
	requireActiveSession bool,
	boundary FilesystemOutputFileSettlementBoundary,
) (settlement transfer.FileSettlement, resultErr error) {
	if err := transaction.claimTerminalSettlement(requireActiveSession); err != nil {
		return transfer.FileSettlement{}, err
	}
	defer func() {
		transaction.session.traceReturnedFileSettlement(filesystemOutputFileSettlementTraceContext{
			boundary: boundary, pauseReason: reason,
		}, settlement, resultErr)
	}()
	defer transaction.finishTerminalResult(&resultErr, "close paused output")
	return transaction.pauseSettling(ctx, reason)
}

func (transaction *filesystemFileTransaction) pauseSettling(
	ctx context.Context,
	reason transfer.FilePauseReason,
) (transfer.FileSettlement, error) {
	settleErr := ctx.Err()
	transaction.mu.Lock()
	if transaction.lifecycle != filesystemFileTransactionSettling {
		transaction.mu.Unlock()
		return transfer.FileSettlement{}, outputFault(
			transfer.OutputFaultFile, transfer.OutputFaultOwnership, errOutputSessionClosed,
		)
	}
	var checkpoint transfer.VerifiedDurableRanges
	checkpointErr := settleErr
	if checkpointErr == nil {
		checkpoint, checkpointErr = transaction.checkpointLocked()
	}
	if checkpointErr != nil {
		record := transaction.resumable.Bound().Record()
		checkpoint, _ = transfer.VerifyDurableRanges(
			transaction.binding, transfer.CheckpointGeneration(record.CheckpointGeneration()), record.DurableRanges(),
		)
	}
	settlement, err := transfer.NewVerifiedFileSettlement(transfer.FilePaused, checkpoint)
	transaction.mu.Unlock()
	if err != nil {
		return transfer.FileSettlement{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract,
			errors.Join(err, checkpointErr))
	}
	if checkpointErr != nil {
		return settlement, fileSettlementFailure(checkpointErr)
	}
	return settlement, nil
}

func (transaction *filesystemFileTransaction) Retire(
	ctx context.Context,
	reason transfer.FileRetireReason,
) (settlement transfer.FileSettlement, resultErr error) {
	if err := ctx.Err(); err != nil {
		return transfer.FileSettlement{}, fileSettlementFailure(err)
	}
	if transaction == nil || reason < transfer.FileRetireIsolatedPermanentSourceFailure ||
		reason > transfer.FileRetireExplicitPolicySkip {
		return transfer.FileSettlement{}, outputFault(
			transfer.OutputFaultFile, transfer.OutputFaultContract, transfer.ErrInvalidOutputSettlement,
		)
	}
	if err := transaction.session.beginOperation(); err != nil {
		return transfer.FileSettlement{}, err
	}
	defer transaction.session.endOperation()
	if err := transaction.claimTerminalSettlement(true); err != nil {
		return transfer.FileSettlement{}, err
	}
	defer func() {
		transaction.session.traceReturnedFileSettlement(filesystemOutputFileSettlementTraceContext{
			boundary: FilesystemOutputSettlementRetire, retireReason: reason,
		}, settlement, resultErr)
	}()
	var validation *outputAncestryValidation
	defer func() {
		transaction.finishTerminalResult(&resultErr, "close retired output")
		if validation == nil {
			return
		}
		ancestryErr := finishOutputAncestryOperation(
			transaction.session, validation, outputAncestryRequirement{},
			FilesystemOutputAncestryRecovery,
			transaction.resumable.Bound().Record().LocatorDigest(),
			"finish retired output ancestry",
			nil,
		)
		if ancestryErr != nil {
			settlement = transfer.FileSettlement{}
			resultErr = errors.Join(resultErr, ancestryErr)
		}
	}()

	retiring, err := transaction.prepareRetirement(reason)
	if err != nil {
		return transfer.FileSettlement{}, err
	}
	requirement := outputAncestryRequirement{}
	validation, err = transaction.session.validateOutputAncestry(requirement)
	if err != nil {
		transaction.session.traceOutputAncestry(
			FilesystemOutputAncestryRecovery,
			transaction.resumable.Bound().Record().LocatorDigest(),
			err,
		)
		return transfer.FileSettlement{}, outputAncestryOperationFault(
			"validate ancestry before retiring output", err,
		)
	}
	return transaction.retireSettling(retiring)
}

func (transaction *filesystemFileTransaction) prepareRetirement(
	reason transfer.FileRetireReason,
) (resumestate.BoundFileRecord, error) {
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	if transaction.lifecycle != filesystemFileTransactionSettling || transaction.session.operationDisabled() {
		return resumestate.BoundFileRecord{}, outputFault(
			transfer.OutputFaultFile, transfer.OutputFaultOwnership, errOutputSessionClosed,
		)
	}
	var retiring resumestate.BoundFileRecord
	var err error
	if reason == transfer.FileRetireInvalidatedRevision {
		retiring, err = resumestate.PrepareInvalidatedRevisionRetirement(transaction.resumable.Bound())
	} else {
		retiring, err = resumestate.PrepareIsolatedRetirement(transaction.resumable.Bound())
	}
	if err != nil {
		return resumestate.BoundFileRecord{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if err := transaction.session.installFileRecord(
		transaction.recordDir, transaction.recordName, transaction.resumable.Bound(), retiring,
	); err != nil {
		return resumestate.BoundFileRecord{}, err
	}
	return retiring, nil
}

func (transaction *filesystemFileTransaction) retireSettling(
	retiring resumestate.BoundFileRecord,
) (transfer.FileSettlement, error) {
	transaction.mu.Lock()
	closeErr := errors.Join(transaction.data.Close(), transaction.anchor.Close())
	transaction.data, transaction.anchor = nil, nil
	transaction.mu.Unlock()
	if closeErr != nil {
		return transfer.FileSettlement{}, fileOutputFault("close retiring output", closeErr)
	}
	settlement, _, err := transaction.session.retireBoundFile(
		transaction.recordDir, transaction.recordName, retiring, transaction.binding,
	)
	if err != nil {
		return transfer.FileSettlement{}, err
	}
	return settlement, nil
}

func (transaction *filesystemFileTransaction) claimTerminalSettlement(requireActiveSession bool) error {
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	if transaction.lifecycle != filesystemFileTransactionOpen ||
		requireActiveSession && transaction.session.operationDisabled() {
		return outputFault(transfer.OutputFaultFile, transfer.OutputFaultOwnership, errOutputSessionClosed)
	}
	transaction.lifecycle = filesystemFileTransactionSettling
	return nil
}

func (transaction *filesystemFileTransaction) finishTerminalResult(
	resultErr *error,
	operation string,
) {
	closeErr := transaction.finishTerminalSettlement()
	if closeErr != nil {
		*resultErr = errors.Join(
			*resultErr,
			pauseRequiredFileOperationFault(operation, nil, closeErr),
		)
	}
}

func (transaction *filesystemFileTransaction) finishTerminalSettlement() error {
	transaction.mu.Lock()
	if transaction.lifecycle != filesystemFileTransactionSettling {
		transaction.mu.Unlock()
		return errOutputV3Unsafe
	}
	transaction.lifecycle = filesystemFileTransactionClosed
	digest := transaction.resumable.Bound().Record().LocatorDigest()
	closeErr := transaction.closeHandlesLocked()
	transaction.mu.Unlock()
	transaction.session.finishFile(digest, transaction)
	return closeErr
}

func (transaction *filesystemFileTransaction) closeHandles() error {
	if transaction == nil {
		return nil
	}
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	return transaction.closeHandlesLocked()
}

func (transaction *filesystemFileTransaction) closeHandlesLocked() error {
	var result error
	if transaction.data != nil {
		result = errors.Join(result, transaction.data.Close())
		transaction.data = nil
	}
	if transaction.anchor != nil {
		result = errors.Join(result, transaction.anchor.Close())
		transaction.anchor = nil
	}
	if transaction.stageDir != nil {
		result = errors.Join(result, transaction.stageDir.Close())
		transaction.stageDir = nil
	}
	if transaction.anchorDir != nil {
		result = errors.Join(result, transaction.anchorDir.Close())
		transaction.anchorDir = nil
	}
	if transaction.recordDir != nil {
		result = errors.Join(result, transaction.recordDir.Close())
		transaction.recordDir = nil
	}
	return result
}

func (session *filesystemOutputSession) finishFile(
	digest resumestate.LocatorDigest,
	transaction *filesystemFileTransaction,
) {
	session.mu.Lock()
	if session.active[digest] == transaction {
		delete(session.active, digest)
	}
	session.mu.Unlock()
}

func fileOutputFault(operation string, cause error) error {
	code := transfer.OutputFaultStateIO
	if errors.Is(cause, errOutputV3Unsafe) {
		code = transfer.OutputFaultOwnership
	}
	return outputFault(transfer.OutputFaultFile, code, fmt.Errorf("%s: %w", operation, cause))
}

func internalCleanupNeedsAttentionFault(operation string) error {
	// Once publication has established the public final, ambiguous internal
	// cleanup evidence revokes mutation authority but not ownership history.
	return pauseRequiredFileOutputFault(fileOutputFault(
		operation,
		errors.Join(errOutputV3Unsafe, errOutputV3InternalCleanupNeedsAttention),
	))
}

func isInternalCleanupNeedsAttentionFault(err error) bool {
	var sessionErr *transfer.OutputSessionError
	return errors.As(err, &sessionErr) && err == sessionErr &&
		errors.Is(err, errOutputV3InternalCleanupNeedsAttention)
}

func directoryOutputFault(operation string, cause error) error {
	code := transfer.OutputFaultStateIO
	if errors.Is(cause, errOutputV3Unsafe) {
		code = transfer.OutputFaultOwnership
	}
	return outputFault(transfer.OutputFaultSession, code, fmt.Errorf("%s: %w", operation, cause))
}

func outputDirectoryOperationFault(operation string, cause error) error {
	if cause == nil {
		return nil
	}
	if errors.Is(cause, errOutputAncestryUnsafe) ||
		(errors.Is(cause, errOutputV3Unsafe) && !errors.Is(cause, errOutputAncestryAuthorityDenied)) ||
		(errors.Is(cause, errOutputV3PositiveEntryEvidence) && errors.Is(cause, errOutputV3Collision)) {
		return outputAncestryPauseFault(operation, cause)
	}
	return transfer.NewOutputSessionError(directoryOutputFault(operation, cause), true)
}

func fileSettlementFailure(cause error) error {
	var fault *transfer.OutputFault
	if errors.As(cause, &fault) {
		return cause
	}
	return outputFault(transfer.OutputFaultFile, transfer.OutputFaultStateIO, cause)
}

var _ transfer.FileTransaction = (*filesystemFileTransaction)(nil)
