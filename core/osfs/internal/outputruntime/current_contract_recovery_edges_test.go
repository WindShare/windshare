package outputruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestCurrentRecoveryDispatchPreservesTerminalMeanings(t *testing.T) {
	rootSpec := newRuntimeTestRootSpec(t)
	session, intent := currentCoverageSession(t, rootSpec, 0x21, 0x22)
	parent := currentCoverageRoot(t, session, intent, 0x23)
	file := currentCoverageFile(t, session, intent, "decisions.bin", parent, 0x24, 0x25, 1)
	transaction := currentCoverageTransaction(t, session, file)
	t.Cleanup(func() { _, _ = session.PauseJob(context.Background(), transfer.JobPauseInterrupted) })
	if err := transaction.WriteRange(context.Background(), 0, []byte{0x5a}); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}

	witnessed := transaction.resumable.BoundState()
	publishing, err := resumestate.PrepareCheckpointRuntimePublication(transaction.resumable)
	if err != nil {
		t.Fatal(err)
	}
	blockedInstall := currentPublishDecision(t, publishing, resumestate.PublishAlreadyExistsDifferent)
	blocked := currentApplyDecision(t, publishing, blockedInstall)
	blockedFile := currentBindDecisionFile(t, blocked, transaction)
	blockedHold := currentRecoveryDecision(t, blocked, resumestate.FileObservation{
		Anchor: resumestate.AnchorVerified, Stage: resumestate.EntrySameAsAnchor,
		Final: resumestate.EntryDifferentFromAnchor,
	})
	result, err := session.inner.applyFileRecoveryAction(
		file, fileRecoveryState{resumable: blockedFile}, fileRecoveryIteration{decision: blockedHold},
	)
	if err != nil || !result.terminal {
		t.Fatalf("hold publish-blocked decision = (%+v, %v)", result, err)
	}

	publishedInstall := currentRecoveryDecision(t, publishing, resumestate.FileObservation{
		Anchor: resumestate.AnchorVerified, Stage: resumestate.EntrySameAsAnchor,
		Final: resumestate.EntrySameAsAnchor, Metadata: resumestate.MetadataMatches,
		FinalParent: resumestate.FinalParentSynced,
	})
	published := currentApplyDecision(t, publishing, publishedInstall)
	publishedFile := currentBindDecisionFile(t, published, transaction)
	publishedHold := currentRecoveryDecision(t, published, resumestate.FileObservation{
		Anchor: resumestate.AnchorVerified, Stage: resumestate.EntryDifferentFromAnchor,
		Final: resumestate.EntrySameAsAnchor, Metadata: resumestate.MetadataMatches,
	})
	if _, err := session.inner.applyFileRecoveryAction(
		file, fileRecoveryState{resumable: publishedFile}, fileRecoveryIteration{decision: publishedHold},
	); err == nil {
		t.Fatal("ambiguous published cleanup did not require attention")
	}

	retiring, err := resumestate.PrepareCheckpointRuntimeIsolatedRetirement(witnessed)
	if err != nil {
		t.Fatal(err)
	}
	retiringFile := currentBindDecisionFile(t, retiring, transaction)
	retiringHold := currentRecoveryDecision(t, retiring, resumestate.FileObservation{
		Anchor: resumestate.AnchorMissing, Stage: resumestate.EntryPresentUnresolved,
	})
	if _, err := session.inner.applyFileRecoveryAction(
		file, fileRecoveryState{resumable: retiringFile}, fileRecoveryIteration{decision: retiringHold},
	); err == nil {
		t.Fatal("ambiguous retirement cleanup did not require attention")
	}

	quarantineInstall := currentRecoveryDecision(t, witnessed, resumestate.FileObservation{
		Anchor: resumestate.AnchorMissing, Stage: resumestate.EntryMissing, Final: resumestate.EntryMissing,
	})
	quarantined := currentApplyDecision(t, witnessed, quarantineInstall)
	quarantinedFile := currentBindDecisionFile(t, quarantined, transaction)
	quarantineHold := currentRecoveryDecision(t, quarantined, resumestate.FileObservation{})
	closeFailure := errors.New("observation close failed")
	if _, err := session.inner.applyFileRecoveryAction(
		file, fileRecoveryState{resumable: quarantinedFile},
		fileRecoveryIteration{decision: quarantineHold, observationCleanupErr: closeFailure},
	); !errors.Is(err, closeFailure) {
		t.Fatalf("held quarantine cleanup error = %v", err)
	}

	if _, err := session.inner.applyFileRecoveryAction(file, fileRecoveryState{}, fileRecoveryIteration{}); err == nil {
		t.Fatal("zero recovery action was accepted")
	}
	if got := recoveryDecisionQuarantineReason(quarantineInstall); got != transfer.QuarantineOwnershipMismatch {
		t.Fatalf("install quarantine reason = %d", got)
	}
	if recoveryDecisionQuarantineReason(blockedHold) != 0 {
		t.Fatal("non-quarantine decision reported a quarantine reason")
	}
	if err := fileRetirementObservationCleanupFault(quarantineInstall, closeFailure); err != nil {
		t.Fatalf("quarantine did not retain observation cleanup: %v", err)
	}
	if err := fileRetirementObservationCleanupFault(resumestate.RecoveryDecision{}, closeFailure); !errors.Is(err, closeFailure) {
		t.Fatalf("ordinary retirement cleanup error = %v", err)
	}
}

func TestCurrentRecoveryErrorEdgesFailClosed(t *testing.T) {
	t.Parallel()

	ordinary := errors.New("operation failed")
	zeroSession := &Session{}
	zeroFile := transfer.OutputFile{}
	zeroState := fileRecoveryState{}

	if result, handled, err := zeroSession.handleUnclassifiedRecoveredPublication(
		zeroFile, zeroState, recoveryPublicationAttempt{result: resumestate.PublishLinkCreated},
	); handled || err != nil || result.terminal {
		t.Fatalf("classified publication was intercepted: (%+v, %t, %v)", result, handled, err)
	}
	for _, attempt := range []recoveryPublicationAttempt{
		{linkErr: errOutputAncestryUnsafe},
		{linkErr: outputcap.ErrFixedLinkSourceChanged},
		{linkErr: outputcap.ErrUnsafeNamespace},
		{linkErr: ordinary},
		{cleanupErr: ordinary},
	} {
		if _, handled, err := zeroSession.handleUnclassifiedRecoveredPublication(
			zeroFile, zeroState, attempt,
		); !handled || err == nil {
			t.Fatalf("unclassified publication attempt %+v = handled:%t err:%v", attempt, handled, err)
		}
	}

	if _, err := zeroSession.settleClassifiedRecoveredPublication(
		zeroFile, zeroState, recoveryPublicationAttempt{result: resumestate.PublishAlreadyExistsDifferent},
	); err == nil {
		t.Fatal("classified publication accepted missing durable authority")
	}
	if _, err := zeroSession.finishInstalledRetirement(
		zeroFile, zeroState, resumestate.RecoveryDecision{},
	); err == nil {
		t.Fatal("installed retirement accepted missing durable authority")
	}
	if _, err := zeroSession.retireRecoveredFile(zeroFile, zeroState); err == nil {
		t.Fatal("recovered retirement accepted missing durable authority")
	}
	if _, err := zeroSession.holdRecoveredQuarantine(zeroFile, zeroState, ordinary); !errors.Is(err, ordinary) {
		t.Fatalf("held quarantine lost cleanup failure: %v", err)
	}

	if step, err := heldRetirementQuarantine(
		transfer.OutputFileBinding{}, resumestate.CheckpointRuntimeState{}, nil,
	); err != nil || !step.complete || !step.quarantined {
		t.Fatalf("binding-free held quarantine = (%+v, %v)", step, err)
	}
	if _, err := heldRetirementQuarantine(
		transfer.OutputFileBinding{}, resumestate.CheckpointRuntimeState{}, ordinary,
	); !errors.Is(err, ordinary) {
		t.Fatalf("held retirement quarantine lost cleanup failure: %v", err)
	}
	if step, err := zeroSession.applyFileRetirementDecision(
		resumestate.BoundCheckpointRuntimeState{}, transfer.OutputFileBinding{},
		resumestate.RecoveryDecision{}, nil,
	); err == nil || !step.complete {
		t.Fatalf("zero retirement decision = (%+v, %v)", step, err)
	}
	if settlement, err := retiredFileSettlement(
		transfer.OutputFileBinding{}, resumestate.RecoveryDecision{},
	); err != nil || settlement.Kind() != 0 {
		t.Fatalf("binding-free retirement settlement = (%d, %v)", settlement.Kind(), err)
	}
}

func TestCurrentObservationFailureClassifiersRetainOnlyPositiveEvidence(t *testing.T) {
	t.Parallel()

	stageFailure := errors.New("stage observation failed")
	if observation, err := fileObservationAfterStageFailure(
		resumestate.CheckpointRuntimeWitnessed, resumestate.AnchorVerified, stageFailure,
	); !errors.Is(err, stageFailure) || observation != (resumestate.FileObservation{}) {
		t.Fatalf("ordinary stage failure = (%+v, %v)", observation, err)
	}
	if observation, err := fileObservationAfterStageFailure(
		resumestate.CheckpointRuntimeWitnessed, resumestate.AnchorUnsafe, stageFailure,
	); err != nil || observation.Stage != resumestate.EntryNotObserved {
		t.Fatalf("conclusive internal evidence = (%+v, %v)", observation, err)
	}
	if observed, err := classifyFinalObservationFailure(stageFailure); !errors.Is(err, stageFailure) || observed.entry != 0 {
		t.Fatalf("ordinary final failure = (%+v, %v)", observed, err)
	}
	if observed, err := classifyFinalObservationFailure(outputcap.ErrUnsafeNamespace); err != nil ||
		observed.entry != resumestate.EntryUnsafe {
		t.Fatalf("unsafe final evidence = (%+v, %v)", observed, err)
	}

	if finalParentSyncObservation(resumestate.CheckpointRuntimeState{}, false) != resumestate.FinalParentSyncRequired {
		t.Fatal("unpublished final skipped parent sync")
	}
}

func currentRecoveryDecision(
	t *testing.T,
	bound resumestate.BoundCheckpointRuntimeState,
	observation resumestate.FileObservation,
) resumestate.RecoveryDecision {
	t.Helper()
	decision, err := resumestate.ReduceCheckpointRuntimeStateRecovery(bound, observation)
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

func currentPublishDecision(
	t *testing.T,
	bound resumestate.BoundCheckpointRuntimeState,
	result resumestate.PublishResult,
) resumestate.RecoveryDecision {
	t.Helper()
	decision, err := resumestate.ReduceCheckpointRuntimePublishResult(bound, result)
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

func currentApplyDecision(
	t *testing.T,
	bound resumestate.BoundCheckpointRuntimeState,
	decision resumestate.RecoveryDecision,
) resumestate.BoundCheckpointRuntimeState {
	t.Helper()
	next, err := resumestate.ApplyCheckpointRuntimeRecoveryDecision(bound, decision)
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func currentBindDecisionFile(
	t *testing.T,
	bound resumestate.BoundCheckpointRuntimeState,
	transaction *FileTransaction,
) resumestate.CheckpointRuntimeFile {
	t.Helper()
	file, err := resumestate.BindCheckpointRuntimeDescriptor(bound, transaction.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	return file
}
