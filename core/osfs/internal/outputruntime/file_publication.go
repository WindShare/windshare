package outputruntime

import (
	"context"
	"errors"
	"io/fs"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func (session *Session) linkFinalNoReplace(
	bound resumestate.BoundFileRecord,
	witness *publicationWitness,
) (resumestate.PublishResult, error) {
	result, operationErr, cleanupErr := session.linkFinalNoReplaceResult(bound, witness)
	if errors.Is(operationErr, errOutputAncestryUnsafe) {
		return 0, outputAncestryPauseFault("revalidate final publication", operationErr)
	}
	return result, errors.Join(operationErr, cleanupErr)
}

func (session *Session) linkFinalNoReplaceResult(
	bound resumestate.BoundFileRecord,
	retained *publicationWitness,
) (result resumestate.PublishResult, operationErr error, cleanupErr error) {
	record := bound.Record()
	if retained == nil || !retained.stage.valid() || !retained.anchor.valid() {
		return 0, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("publication source handle is absent")), nil
	}
	// Linux must link by a name beneath the pinned private anchor directory, while
	// Windows can link directly from the source handle. In both cases, reopen both
	// private names and compare them with the retained witness immediately before
	// invoking the platform primitive.
	witness, witnessErr, witnessCleanupErr := session.openPublicationWitness(record, retained.anchor)
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
	linked, err := parent.LinkFileNoReplace(witness.anchor.file, leaf)
	if err == nil {
		if linked == nil {
			return 0, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("publication returned no final handle")), nil
		}
		same, sameErr := linked.SameFile(witness.anchor.file)
		metadataMatches, metadataErr := linked.MetadataMatches(
			record.ExactSize(), record.ExpectedMetadata().ModifiedTime,
		)
		if sameErr != nil || metadataErr != nil || !same || !metadataMatches {
			return 0, errors.Join(outputcap.ErrUnsafeNamespace, sameErr, metadataErr), linked.Close()
		}
		return resumestate.PublishLinkCreated, parent.Sync(), linked.Close()
	}
	linkedCloseErr := closeOutputV3File(linked)
	if !errors.Is(err, outputcap.ErrNamespaceCollision) {
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
	if kind == outputcap.EntryAbsent {
		return resumestate.PublishExistingAmbiguous, nil, linkedCloseErr
	}
	if kind != outputcap.EntryRegularFile {
		return resumestate.PublishAlreadyExistsDifferent, nil, linkedCloseErr
	}
	final, openErr := parent.OpenFile(leaf, false, false)
	if openErr != nil {
		return resumestate.PublishExistingAmbiguous, nil, errors.Join(linkedCloseErr, closeOutputV3File(final))
	}
	defer func() { cleanupErr = errors.Join(cleanupErr, final.Close()) }()
	same, sameErr := final.SameFile(witness.anchor.file)
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

func (session *Session) syncFinalParent(
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
func (session *Session) recoverFinalParentSync(
	file transfer.OutputFile,
	recordDir outputcap.Directory,
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

func (session *Session) removeStage(record resumestate.FileRecord) (error, error) {
	stageName := resumestate.StageName(record.OutputObject())
	directory, present, err := openOutputShard(session.stagesDir, stageName.Shard(), false)
	if err != nil {
		return errors.Join(outputnamespace.ErrPositiveEntryEvidence, err), closeOutputV3Directory(directory)
	}
	if !present {
		return errors.Join(outputcap.ErrUnsafeNamespace, fs.ErrNotExist), closeOutputV3Directory(directory)
	}
	stage, err := directory.OpenFile(stageName.Name(), true, false)
	if err != nil {
		return errors.Join(outputnamespace.ErrPositiveEntryEvidence, err),
			errors.Join(closeOutputV3File(stage), directory.Close())
	}
	operationErr := directory.RemoveFile(stageName.Name(), stage)
	if operationErr == nil {
		operationErr = directory.Sync()
	}
	return operationErr, errors.Join(stage.Close(), directory.Close())
}

func (session *Session) syncObjectShard(
	parent outputcap.Directory,
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

func (transaction *FileTransaction) Checkpoint(
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
	if transaction.lifecycle != FileTransactionOpen || transaction.session.operationDisabled() {
		return transfer.VerifiedDurableRanges{}, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultOwnership, outputfault.ErrSessionClosed,
		)
	}
	return transaction.checkpointLocked()
}

func (transaction *FileTransaction) checkpointLocked() (transfer.VerifiedDurableRanges, error) {
	transaction.session.mu.Lock()
	poisoned := transaction.session.poisoned
	transaction.session.mu.Unlock()
	if poisoned {
		return transfer.VerifiedDurableRanges{}, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultOwnership, outputfault.ErrSessionClosed,
		)
	}
	if transaction.lifecycle != FileTransactionOpen &&
		transaction.lifecycle != FileTransactionSettling || !transaction.data.valid() || !transaction.anchor.valid() {
		return transfer.VerifiedDurableRanges{}, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultOwnership, outputfault.ErrSessionClosed,
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
		return transfer.VerifiedDurableRanges{}, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultContract, errors.Join(err, resumestate.ErrInvalidState),
		)
	}
	if err := transaction.data.Sync(); err != nil {
		return transfer.VerifiedDurableRanges{}, fileOutputFault("sync checkpoint data", err)
	}
	same, err := transaction.data.SameFile(transaction.anchor)
	if err != nil {
		if !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			return transfer.VerifiedDurableRanges{}, pauseRequiredFileOutputFault(fileOutputFault(
				"compare checkpoint witness", err,
			))
		}
		if _, quarantineErr := transaction.installWitnessQuarantineLocked(resumestate.QuarantineStageUnsafe); quarantineErr != nil {
			return transfer.VerifiedDurableRanges{}, quarantineErr
		}
		transaction.session.poisonState()
		return transfer.VerifiedDurableRanges{}, pauseRequiredFileOutputFault(fileOutputFault(
			"verify checkpoint witness identity", errors.Join(outputcap.ErrUnsafeNamespace, err),
		))
	}
	if !same {
		if _, quarantineErr := transaction.installWitnessQuarantineLocked(resumestate.QuarantineStageUnsafe); quarantineErr != nil {
			return transfer.VerifiedDurableRanges{}, quarantineErr
		}
		transaction.session.poisonState()
		return transfer.VerifiedDurableRanges{}, pauseRequiredFileOutputFault(fileOutputFault(
			"verify checkpoint witness identity", outputcap.ErrUnsafeNamespace,
		))
	}
	candidate, err := transaction.resumable.WithCheckpoint(record.CheckpointGeneration()+1, merged)
	if err != nil {
		return transfer.VerifiedDurableRanges{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
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

func (transaction *FileTransaction) installWitnessQuarantineLocked(
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
		return transfer.FileSettlement{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	return quarantinedSettlement(transaction.binding, quarantined.Record())
}

func (transaction *FileTransaction) installWitnessQuarantineWithCleanupLocked(
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

func (transaction *FileTransaction) Commit(
	ctx context.Context,
) (settlement transfer.FileSettlement, resultErr error) {
	if err := ctx.Err(); err != nil {
		return transfer.FileSettlement{}, fileSettlementFailure(err)
	}
	if transaction == nil {
		return transfer.FileSettlement{}, outputfault.New(
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

func (transaction *FileTransaction) commitSettling(
	ctx context.Context,
) (transfer.FileSettlement, error) {
	transaction.mu.Lock()
	settlement, settled, err := transaction.publishPreparedLocked()
	transaction.mu.Unlock()
	if err != nil || settled {
		return settlement, err
	}

	if transaction.reduceFile == nil {
		return transfer.FileSettlement{}, outputfault.New(
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
		return transfer.FileSettlement{}, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultContract, transfer.ErrInvalidOutputSettlement,
		)
	}
	if settlement.Kind() == transfer.FileQuarantined {
		reference, reason, valid := settlement.Quarantine()
		if !valid {
			return transfer.FileSettlement{}, outputfault.New(
				transfer.OutputFaultFile, transfer.OutputFaultContract, transfer.ErrInvalidOutputSettlement,
			)
		}
		settlement, err = transfer.NewTransactionQuarantinedFileSettlement(
			transaction.binding, reference, reason,
		)
		if err != nil {
			return transfer.FileSettlement{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
		}
		return settlement, nil
	}
	settledBinding, bound := settlement.OutputBinding()
	if !bound || settledBinding != transaction.binding {
		return transfer.FileSettlement{}, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultContract, transfer.ErrInvalidOutputSettlement,
		)
	}
	return settlement, nil
}

func (transaction *FileTransaction) publishPreparedLocked() (
	transfer.FileSettlement,
	bool,
	error,
) {
	witness, witnessErr, witnessCleanupErr := transaction.session.openPublicationWitness(
		transaction.resumable.Bound().Record(), transaction.anchor,
	)
	var publishResult resumestate.PublishResult
	linkErr, linkCleanupErr := witnessErr, witnessCleanupErr
	if witnessErr == nil && witnessCleanupErr == nil {
		publishResult, linkErr, linkCleanupErr = transaction.session.linkFinalNoReplaceResult(
			transaction.resumable.Bound(), witness,
		)
		linkCleanupErr = errors.Join(linkCleanupErr, witness.Close())
	}
	if errors.Is(linkErr, errOutputAncestryUnsafe) {
		return transfer.FileSettlement{}, false,
			outputAncestryPauseFault("revalidate live final publication", linkErr)
	}
	if publishResult == 0 && errors.Is(linkErr, outputcap.ErrFixedLinkSourceChanged) {
		return transaction.installWitnessQuarantineWithCleanupLocked(
			resumestate.QuarantineAnchorUnsafe,
			"close invalidated live publication witness",
			linkCleanupErr,
		)
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
		return transfer.FileSettlement{}, false, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, reduceErr)
	}
	publishBlocked, applyErr := resumestate.ApplyRecoveryDecision(transaction.resumable.Bound(), decision)
	if applyErr != nil {
		return transfer.FileSettlement{}, false, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, applyErr)
	}
	if err := transaction.session.installFileRecord(
		transaction.recordDir, transaction.recordName, transaction.resumable.Bound(), publishBlocked,
	); err != nil {
		return transfer.FileSettlement{}, false, err
	}
	resumable, bindErr := resumestate.BindResumableFile(publishBlocked, transaction.descriptor)
	if bindErr != nil {
		return transfer.FileSettlement{}, false, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, bindErr)
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
		return transfer.FileSettlement{}, false, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultContract, transfer.ErrInvalidOutputSettlement,
		)
	}
	return settlement, true, nil
}
