package outputruntime

import (
	"context"
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

type fileReducer func(
	context.Context,
	transfer.OutputFile,
	resumestate.ResumableFileAuthority,
	outputcap.Directory,
	string,
) (transfer.FileStart, error)

type fileRecoveryState struct {
	resumable    resumestate.ResumableFileAuthority
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
	resumable resumestate.ResumableFileAuthority,
	recordDir outputcap.Directory,
	recordName string,
) (resultStart transfer.FileStart, resultErr error) {
	requirement := outputAncestryRequirement{}
	locatorDigest := resumable.Bound().Record().LocatorDigest()
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
	return session.runFileRecovery(
		ctx, file, recordDir, recordName, validation,
		fileRecoveryState{resumable: resumable},
	)
}

func (session *Session) runFileRecovery(
	ctx context.Context,
	file transfer.OutputFile,
	recordDir outputcap.Directory,
	recordName string,
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
		result, err := session.applyFileRecoveryAction(file, recordDir, recordName, state, iteration)
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
	record := state.resumable.Bound().Record()
	observation, cleanupErr, observationErr := session.observeFile(validation, record, state.parentSynced)
	if observationErr != nil {
		return fileRecoveryIteration{}, pauseRequiredFileOutputFault(fileOutputFault(
			"observe file recovery", errors.Join(observationErr, cleanupErr),
		))
	}
	decision, err := resumestate.ReduceResumableFileRecovery(state.resumable, observation)
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
	record resumestate.FileRecord,
	decision resumestate.RecoveryDecision,
) {
	session.owner.trace(FilesystemOutputTrace{
		Operation: TraceFileRecoveryDecision, ResumeIntent: session.resumeIntent,
		SessionID: session.SessionID(), LocatorDigest: outputLocatorDigestFromState(record.LocatorDigest()),
		OutputObjectID:   outputObjectIdentityFromState(record.OutputObject()),
		PreviousPhase:    filesystemOutputFilePhaseFromState(record.Phase()),
		RecoveryAction:   filesystemOutputRecoveryActionFromState(decision.Action()),
		QuarantineReason: recoveryDecisionQuarantineReason(decision),
	})
}

func (session *Session) gateFileStateTemporary(
	file transfer.OutputFile,
	shard outputcap.Directory,
	recordName resumestate.ShardedName,
	name string,
	bound resumestate.BoundFileRecord,
	digest resumestate.LocatorDigest,
) (transfer.FileStart, bool, error) {
	classified := resumestate.ClassifyFileShardEntry(recordName.Shard(), name)
	kind, err := shard.ObserveEntry(name)
	if err != nil {
		start, quarantineErr := session.quarantineRecoveryStart(
			file, shard, recordName.Name(), bound, resumestate.QuarantineUpdateTemporary,
		)
		return start, true, quarantineErr
	}
	decision, err := reduceGateTemporaryDecision(session, classified, kind)
	if err != nil {
		return transfer.FileStart{}, false, err
	}
	switch decision.Action() {
	case resumestate.UpdateTemporaryAcceptInstalledTarget:
		return transfer.FileStart{}, false, nil
	case resumestate.UpdateTemporaryRemoveAndSyncShard:
		return session.removeGateFileStateTemporary(file, shard, recordName, name, bound, decision)
	case resumestate.UpdateTemporaryInstallFileQuarantine:
		return session.installGateFileStateQuarantine(file, shard, recordName, bound, decision, digest)
	default:
		return session.quarantineFileStateStart(file, digest)
	}
}

func reduceGateTemporaryDecision(
	session *Session,
	classified resumestate.ClassifiedFileShardEntry,
	kind outputcap.EntryKind,
) (resumestate.UpdateTemporaryDecision, error) {
	entry := updateTemporaryEntryForGateKind(kind)
	decision, err := resumestate.ReduceUpdateTemporary(
		session.stateSnapshot().NamespaceAuthority(), classified, entry, resumestate.UpdateTargetValid,
	)
	if err != nil {
		return resumestate.UpdateTemporaryDecision{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	return decision, nil
}

func updateTemporaryEntryForGateKind(kind outputcap.EntryKind) resumestate.UpdateTemporaryEntryObservation {
	switch kind {
	case outputcap.EntryAbsent:
		return resumestate.UpdateTemporaryEntryMissing
	case outputcap.EntryRegularFile:
		return resumestate.UpdateTemporaryEntryRegular
	default:
		return resumestate.UpdateTemporaryEntryUnsafe
	}
}

func (session *Session) removeGateFileStateTemporary(
	file transfer.OutputFile,
	shard outputcap.Directory,
	recordName resumestate.ShardedName,
	name string,
	bound resumestate.BoundFileRecord,
	decision resumestate.UpdateTemporaryDecision,
) (transfer.FileStart, bool, error) {
	temporary, err := shard.OpenFile(name, true, false)
	if err != nil {
		return session.handleGateTemporaryOpenFailure(file, shard, recordName, bound, temporary, err)
	}
	if err := decision.AuthorizeRemoval(bound, recordName.Shard(), name, resumestate.UpdateTemporaryEntryRegular); err != nil {
		return transfer.FileStart{}, false, closeGateUnauthorizedTemporary(temporary, err)
	}
	if removeErr := shard.RemoveFile(name, temporary); removeErr != nil {
		return session.handleGateTemporaryMutationFailure(file, shard, recordName, bound, temporary, removeErr)
	}
	return session.finishGateTemporaryRemoval(file, shard, recordName, bound, temporary)
}

func (session *Session) handleGateTemporaryOpenFailure(
	file transfer.OutputFile,
	shard outputcap.Directory,
	recordName resumestate.ShardedName,
	bound resumestate.BoundFileRecord,
	temporary outputcap.File,
	openErr error,
) (transfer.FileStart, bool, error) {
	// Names and ObserveEntry already established that this exact temporary exists.
	// Losing the ability to classify it through a handle is namespace ambiguity,
	// not a retryable lack of mutation authority.
	start, quarantineErr := session.quarantineRecoveryStart(
		file, shard, recordName.Name(), bound, resumestate.QuarantineUpdateTemporary,
	)
	closeErr := closeOutputV3File(temporary)
	if quarantineErr != nil {
		if closeErr != nil {
			quarantineErr = pauseRequiredFileOutputFault(fileOutputFault(
				"close ambiguous state update temporary", errors.Join(quarantineErr, openErr, closeErr),
			))
		}
		return transfer.FileStart{}, true, quarantineErr
	}
	if closeErr != nil {
		return transfer.FileStart{}, true, pauseRequiredFileOutputFault(fileOutputFault(
			"close quarantined state update temporary", errors.Join(openErr, closeErr),
		))
	}
	return start, true, nil
}

func closeGateUnauthorizedTemporary(temporary outputcap.File, authorizationErr error) error {
	closeErr := temporary.Close()
	if closeErr != nil {
		return pauseRequiredFileOutputFault(fileOutputFault(
			"close unauthorized state update temporary", errors.Join(authorizationErr, closeErr),
		))
	}
	return outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, authorizationErr)
}

func (session *Session) handleGateTemporaryMutationFailure(
	file transfer.OutputFile,
	shard outputcap.Directory,
	recordName resumestate.ShardedName,
	bound resumestate.BoundFileRecord,
	temporary outputcap.File,
	operationErr error,
) (transfer.FileStart, bool, error) {
	closeErr := temporary.Close()
	if classifyOutputV3RecoveryFailure(operationErr, outputV3AuthorizedMutation) == outputV3RecoveryAmbiguous {
		start, quarantineErr := session.quarantineRecoveryStartWithCleanup(
			file, shard, recordName.Name(), bound, resumestate.QuarantineUpdateTemporary,
			"close quarantined state update removal", closeErr,
		)
		return start, true, quarantineErr
	}
	return transfer.FileStart{}, false, pauseRequiredFileOperationFault(
		"remove state update temporary", operationErr, closeErr,
	)
}

func (session *Session) finishGateTemporaryRemoval(
	file transfer.OutputFile,
	shard outputcap.Directory,
	recordName resumestate.ShardedName,
	bound resumestate.BoundFileRecord,
	temporary outputcap.File,
) (transfer.FileStart, bool, error) {
	syncErr := shard.Sync()
	closeErr := temporary.Close()
	if syncErr != nil {
		if classifyOutputV3RecoveryFailure(syncErr, outputV3AuthorizedMutation) == outputV3RecoveryAmbiguous {
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
		return transfer.FileStart{}, false, pauseRequiredFileOutputFault(fileOutputFault(
			"close synced state update temporary", closeErr,
		))
	}
	return transfer.FileStart{}, false, nil
}

func (session *Session) installGateFileStateQuarantine(
	file transfer.OutputFile,
	shard outputcap.Directory,
	recordName resumestate.ShardedName,
	bound resumestate.BoundFileRecord,
	decision resumestate.UpdateTemporaryDecision,
	digest resumestate.LocatorDigest,
) (transfer.FileStart, bool, error) {
	quarantined, err := resumestate.ApplyUpdateTemporaryQuarantine(bound, decision)
	if err != nil {
		return transfer.FileStart{}, false, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if quarantined.Record().StateGeneration() != bound.Record().StateGeneration() {
		if err := session.installFileRecord(shard, recordName.Name(), bound, quarantined); err != nil {
			return transfer.FileStart{}, false, err
		}
	}
	return session.quarantineFileStateStart(file, digest)
}

func (session *Session) createWitnessObject(
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
		closeOutputV3File(resources.final),
		closeOutputV3File(resources.stage), closeOutputV3Directory(resources.stageDir),
		closeOutputV3File(resources.anchor), closeOutputV3Directory(resources.anchorDir),
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
	record resumestate.FileRecord,
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
	if record.Phase() == resumestate.FileRetiring ||
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
	phase resumestate.FilePhase,
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
	record resumestate.FileRecord,
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
	if classifyOutputV3RecoveryFailure(err, outputV3BeforeEntryEvidence) == outputV3RecoveryPauseRequired {
		return finalFileObservation{}, err
	}
	return finalFileObservation{entry: resumestate.EntryUnsafe}, nil
}

func observeRegularFinalFile(
	parent outputcap.Directory,
	leaf string,
	anchor outputcap.File,
	anchorObservation resumestate.AnchorObservation,
	record resumestate.FileRecord,
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
	record resumestate.FileRecord,
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

func finalParentSyncObservation(
	record resumestate.FileRecord,
	finalParentSynced bool,
) resumestate.FinalParentObservation {
	if finalParentSynced || record.Phase() == resumestate.FilePublished {
		return resumestate.FinalParentSynced
	}
	return resumestate.FinalParentSyncRequired
}

func closeOutputV3File(file outputcap.File) error {
	if file == nil {
		return nil
	}
	return file.Close()
}

func (session *Session) observeAnchor(
	record resumestate.FileRecord,
) (outputcap.File, outputcap.Directory, resumestate.AnchorObservation, error) {
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

func internalCleanupNeedsAttentionFault(operation string) error {
	// Once publication has established the public final, ambiguous internal
	// cleanup evidence revokes mutation authority but not ownership history.
	return pauseRequiredFileOutputFault(fileOutputFault(
		operation,
		errors.Join(outputcap.ErrUnsafeNamespace, errOutputV3InternalCleanupNeedsAttention),
	))
}

func isInternalCleanupNeedsAttentionFault(err error) bool {
	sessionErr, found := errors.AsType[*transfer.OutputSessionError](err)
	return found && errors.Is(sessionErr, errOutputV3InternalCleanupNeedsAttention)
}

func directoryOutputFault(operation string, cause error) error {
	code := transfer.OutputFaultStateIO
	if errors.Is(cause, outputcap.ErrUnsafeNamespace) {
		code = transfer.OutputFaultOwnership
	}
	return outputfault.New(transfer.OutputFaultSession, code, fmt.Errorf("%s: %w", operation, cause))
}
