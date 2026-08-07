package outputruntime

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

type fileReducer func(
	context.Context,
	transfer.OutputFile,
	resumestate.CheckpointRuntimeFile,
) (transfer.FileStart, error)

type fileRecoveryState struct {
	resumable    resumestate.CheckpointRuntimeFile
	parentSynced bool
}

type fileRecoveryIteration struct {
	decision              resumestate.RecoveryDecision
	observationCleanupErr error
}

type fileRecoveryActionResult struct {
	state    fileRecoveryState
	start    transfer.FileStart
	terminal bool
}

func (session *Session) reduceFile(
	ctx context.Context,
	file transfer.OutputFile,
	resumable resumestate.CheckpointRuntimeFile,
) (resultStart transfer.FileStart, resultErr error) {
	requirement := outputAncestryRequirement{}
	locatorDigest := resumable.BoundState().State().LocatorDigest()
	validation, err := session.validateOutputAncestry(requirement)
	if err != nil {
		session.traceOutputAncestry(FilesystemOutputAncestryRecovery, locatorDigest, err)
		return transfer.FileStart{}, outputAncestryOperationFault("validate ancestry before file recovery", err)
	}
	defer func() {
		ancestryErr := finishOutputAncestryOperation(
			session, validation, requirement, FilesystemOutputAncestryRecovery,
			locatorDigest, "finish file recovery ancestry", nil,
		)
		if ancestryErr != nil {
			resultStart = transfer.FileStart{}
			resultErr = errors.Join(resultErr, ancestryErr)
		}
	}()
	return session.runFileRecovery(ctx, file, validation, fileRecoveryState{resumable: resumable})
}

func (session *Session) runFileRecovery(
	ctx context.Context,
	file transfer.OutputFile,
	validation *outputAncestryValidation,
	state fileRecoveryState,
) (transfer.FileStart, error) {
	for {
		if err := ctx.Err(); err != nil {
			return transfer.FileStart{}, err
		}
		iteration, err := session.nextFileRecoveryIteration(validation, state)
		if err != nil {
			return transfer.FileStart{}, err
		}
		result, err := session.applyFileRecoveryAction(file, state, iteration)
		if err != nil {
			return transfer.FileStart{}, err
		}
		if result.terminal {
			return result.start, nil
		}
		state = result.state
	}
}

func (session *Session) nextFileRecoveryIteration(
	validation *outputAncestryValidation,
	state fileRecoveryState,
) (fileRecoveryIteration, error) {
	record := state.resumable.BoundState().State()
	observation, cleanupErr, observationErr := session.observeFile(validation, record, state.parentSynced)
	if observationErr != nil {
		return fileRecoveryIteration{}, pauseRequiredFileOutputFault(fileOutputFault(
			"observe file recovery", errors.Join(observationErr, cleanupErr),
		))
	}
	decision, err := resumestate.ReduceCheckpointRuntimeFileRecovery(state.resumable, observation)
	if err != nil {
		return fileRecoveryIteration{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if cleanupErr != nil && !recoveryActionRetainsObservationCleanup(decision.Action()) {
		return fileRecoveryIteration{}, pauseRequiredFileOutputFault(fileOutputFault(
			"close file recovery observation", cleanupErr,
		))
	}
	session.traceFileRecoveryDecision(record, decision)
	return fileRecoveryIteration{decision: decision, observationCleanupErr: cleanupErr}, nil
}

func recoveryActionRetainsObservationCleanup(action resumestate.RecoveryAction) bool {
	return action == resumestate.RecoveryInstallQuarantine || action == resumestate.RecoveryHoldQuarantine
}

func (session *Session) traceFileRecoveryDecision(
	record resumestate.CheckpointRuntimeState,
	decision resumestate.RecoveryDecision,
) {
	session.owner.trace(FilesystemOutputTrace{
		Operation: TraceFileRecoveryDecision, IntentDigest: session.intentDigest,
		SessionID: session.SessionID(), LocatorDigest: outputLocatorDigestFromState(record.LocatorDigest()),
		OutputObjectID:   outputObjectIdentityFromState(record.OutputObject()),
		PreviousPhase:    filesystemOutputFilePhaseFromState(record.Phase()),
		RecoveryAction:   filesystemOutputRecoveryActionFromState(decision.Action()),
		QuarantineReason: recoveryDecisionQuarantineReason(decision),
	})
}

func (session *Session) createWitnessObject(
	record resumestate.CheckpointRuntimeState,
) (operationErr error, cleanupErr error) {
	stageName := resumestate.StageName(record.OutputObject())
	anchorName := resumestate.AnchorName(record.OutputObject())
	stageDir, _, operationErr := openOutputShard(session.stagesDir, stageName.Shard(), true)
	if operationErr != nil {
		cleanupErr = closeOutputDirectory(stageDir)
		return
	}
	defer func() { cleanupErr = errors.Join(cleanupErr, stageDir.Close()) }()
	anchorDir, _, operationErr := openOutputShard(session.anchorsDir, anchorName.Shard(), true)
	if operationErr != nil {
		cleanupErr = closeOutputDirectory(anchorDir)
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
		operationErr = errors.Join(outputcap.ErrUnsafeNamespace, sameErr)
		return
	}
	stageSize, stageErr := stage.Size()
	anchorSize, anchorErr := anchor.Size()
	if stageErr != nil || anchorErr != nil || stageSize != record.ExactSize() || anchorSize != record.ExactSize() {
		operationErr = errors.Join(outputcap.ErrUnsafeNamespace, stageErr, anchorErr)
		return
	}
	return
}

type fileObservationResources struct {
	anchor    outputcap.File
	anchorDir outputcap.Directory
	stage     outputcap.File
	stageDir  outputcap.Directory
	final     outputcap.File
}

func (resources fileObservationResources) close() error {
	return errors.Join(
		closeOutputFile(resources.final),
		closeOutputFile(resources.stage), closeOutputDirectory(resources.stageDir),
		closeOutputFile(resources.anchor), closeOutputDirectory(resources.anchorDir),
	)
}

type finalFileObservation struct {
	file       outputcap.File
	entry      resumestate.EntryObservation
	metadata   resumestate.MetadataObservation
	parentSync resumestate.FinalParentObservation
}

func (session *Session) observeFile(
	validation *outputAncestryValidation,
	record resumestate.CheckpointRuntimeState,
	finalParentSynced bool,
) (
	observation resumestate.FileObservation,
	cleanupErr error,
	observationErr error,
) {
	resources := fileObservationResources{}
	defer func() { cleanupErr = errors.Join(cleanupErr, resources.close()) }()

	var err error
	var anchorObservation resumestate.AnchorObservation
	resources.anchor, resources.anchorDir, anchorObservation, err = session.observeAnchor(record)
	if err != nil {
		return resumestate.FileObservation{}, nil, err
	}
	var stageObservation resumestate.EntryObservation
	resources.stage, resources.stageDir, stageObservation, err = session.observeStage(
		record, resources.anchor, anchorObservation,
	)
	if err != nil {
		observation, observationErr = fileObservationAfterStageFailure(record.Phase(), anchorObservation, err)
		return observation, nil, observationErr
	}

	observation = fileObservationBeforeFinal(anchorObservation, stageObservation)
	if record.Phase() == resumestate.CheckpointRuntimeRetiring ||
		resumestate.InternalFileObservationRequiresQuarantine(record.Phase(), observation) {
		return observation, nil, nil
	}
	final, err := session.observeFinalFile(
		validation, record, resources.anchor, anchorObservation, finalParentSynced,
	)
	resources.final = final.file
	if err != nil {
		return resumestate.FileObservation{}, nil, err
	}
	observation.Final = final.entry
	observation.Metadata = final.metadata
	observation.FinalParent = final.parentSync
	return observation, nil, nil
}

func fileObservationBeforeFinal(
	anchor resumestate.AnchorObservation,
	stage resumestate.EntryObservation,
) resumestate.FileObservation {
	return resumestate.FileObservation{
		Anchor: anchor, Stage: stage,
		Final: resumestate.EntryNotObserved, Metadata: resumestate.MetadataNotObserved,
		FinalParent: resumestate.FinalParentNotObserved,
	}
}

func fileObservationAfterStageFailure(
	phase resumestate.CheckpointRuntimePhase,
	anchor resumestate.AnchorObservation,
	stageErr error,
) (resumestate.FileObservation, error) {
	partial := fileObservationBeforeFinal(anchor, resumestate.EntryNotObserved)
	if resumestate.InternalFileObservationRequiresQuarantine(phase, partial) {
		return partial, nil
	}
	return resumestate.FileObservation{}, stageErr
}

func (session *Session) observeFinalFile(
	validation *outputAncestryValidation,
	record resumestate.CheckpointRuntimeState,
	anchor outputcap.File,
	anchorObservation resumestate.AnchorObservation,
	finalParentSynced bool,
) (finalFileObservation, error) {
	parentPath, leaf, err := outputLocatorParentAndLeaf(record.CanonicalLocator())
	var parent outputcap.Directory
	if err == nil {
		parent, err = validation.directory(parentPath)
	}
	if err == nil {
		err = validation.revalidateRetainedDirectory(parentPath, outputAncestryNoAuthority)
	}
	if err != nil {
		return classifyFinalObservationFailure(err)
	}
	kind, err := parent.ObserveEntry(leaf)
	if err != nil {
		return classifyFinalObservationFailure(err)
	}
	switch kind {
	case outputcap.EntryAbsent:
		return finalFileObservation{entry: resumestate.EntryMissing}, nil
	case outputcap.EntryRegularFile:
		return observeRegularFinalFile(parent, leaf, anchor, anchorObservation, record, finalParentSynced)
	default:
		entry := resumestate.EntryPresentUnresolved
		if anchorObservation == resumestate.AnchorVerified {
			entry = resumestate.EntryDifferentFromAnchor
		}
		return finalFileObservation{entry: entry}, nil
	}
}

func classifyFinalObservationFailure(err error) (finalFileObservation, error) {
	if classifyNativeRecoveryFailure(err, nativeBeforeEntryEvidence) == nativeRecoveryPauseRequired {
		return finalFileObservation{}, err
	}
	return finalFileObservation{entry: resumestate.EntryUnsafe}, nil
}

func observeRegularFinalFile(
	parent outputcap.Directory,
	leaf string,
	anchor outputcap.File,
	anchorObservation resumestate.AnchorObservation,
	record resumestate.CheckpointRuntimeState,
	finalParentSynced bool,
) (finalFileObservation, error) {
	final, err := parent.OpenFile(leaf, false, false)
	if err != nil {
		return finalFileObservation{file: final, entry: resumestate.EntryUnsafe}, nil
	}
	if anchorObservation != resumestate.AnchorVerified {
		return finalFileObservation{file: final, entry: resumestate.EntryPresentUnresolved}, nil
	}
	same, err := final.SameFile(anchor)
	if err != nil {
		return finalFileObservation{file: final, entry: resumestate.EntryUnsafe}, nil
	}
	if !same {
		return finalFileObservation{file: final, entry: resumestate.EntryDifferentFromAnchor}, nil
	}
	return finalFileObservation{
		file: final, entry: resumestate.EntrySameAsAnchor,
		metadata:   observeFinalMetadata(final, record),
		parentSync: finalParentSyncObservation(record, finalParentSynced),
	}, nil
}

func observeFinalMetadata(
	final outputcap.File,
	record resumestate.CheckpointRuntimeState,
) resumestate.MetadataObservation {
	matches, err := final.MetadataMatches(record.ExactSize(), record.ExpectedMetadata().ModifiedTime)
	if err != nil {
		return resumestate.MetadataUnsafe
	}
	if matches {
		return resumestate.MetadataMatches
	}
	return resumestate.MetadataDiffers
}

func (session *Session) observeAnchor(
	record resumestate.CheckpointRuntimeState,
) (outputcap.File, outputcap.Directory, resumestate.AnchorObservation, error) {
	name := resumestate.AnchorName(record.OutputObject())
	directory, present, err := openOutputShard(session.anchorsDir, name.Shard(), false)
	if err != nil {
		if classifyNativeRecoveryFailure(err, nativeBeforeEntryEvidence) == nativeRecoveryAmbiguous {
			return nil, nil, resumestate.AnchorUnsafe, nil
		}
		return nil, nil, 0, err
	}
	if !present {
		return nil, nil, resumestate.AnchorMissing, nil
	}
	kind, err := directory.ObserveEntry(name.Name())
	if err != nil {
		if classifyNativeRecoveryFailure(err, nativeBeforeEntryEvidence) == nativeRecoveryAmbiguous {
			return nil, directory, resumestate.AnchorUnsafe, nil
		}
		return nil, directory, 0, err
	}
	if kind == outputcap.EntryAbsent {
		return nil, directory, resumestate.AnchorMissing, nil
	}
	if kind != outputcap.EntryRegularFile {
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
