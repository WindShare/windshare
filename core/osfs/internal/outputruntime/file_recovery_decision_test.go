package outputruntime

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3RecoveryActionRetainsObservationCleanupOnlyForQuarantine(t *testing.T) {
	t.Parallel()
	if recoveryActionRetainsObservationCleanup(0) {
		t.Fatal("zero recovery action retained observation cleanup")
	}
	for action := resumestate.RecoveryRetryObjectCreation; action <= resumestate.RecoveryHoldRetiringCleanup; action++ {
		want := action == resumestate.RecoveryInstallQuarantine || action == resumestate.RecoveryHoldQuarantine
		if got := recoveryActionRetainsObservationCleanup(action); got != want {
			t.Errorf("action %v retains observation cleanup = %t, want %t", action, got, want)
		}
	}
}

func TestOutputV3RecoveryParentSyncObservationIsMonotonic(t *testing.T) {
	t.Parallel()
	if got := finalParentSyncObservation(resumestate.FileRecord{}, false); got != resumestate.FinalParentSyncRequired {
		t.Fatalf("unsynced parent observation = %v, want required", got)
	}
	if got := finalParentSyncObservation(resumestate.FileRecord{}, true); got != resumestate.FinalParentSynced {
		t.Fatalf("synced parent observation = %v, want synced", got)
	}
}

func TestOutputV3RecoveryObservationSnapshotsPreserveEvidence(t *testing.T) {
	t.Parallel()
	anchor := resumestate.AnchorVerified
	stage := resumestate.EntrySameAsAnchor
	partial := fileObservationBeforeFinal(anchor, stage)
	if partial.Anchor != anchor || partial.Stage != stage || partial.Final != resumestate.EntryNotObserved ||
		partial.Metadata != resumestate.MetadataNotObserved || partial.FinalParent != resumestate.FinalParentNotObserved {
		t.Fatalf("partial observation = %#v", partial)
	}

	stageErr := errors.New("stage observation unavailable")
	if _, err := fileObservationAfterStageFailure(resumestate.FilePublished, anchor, stageErr); !errors.Is(err, stageErr) {
		t.Fatalf("non-quarantine stage error = %v, want %v", err, stageErr)
	}
	quarantined, err := fileObservationAfterStageFailure(
		resumestate.FileWitnessed,
		resumestate.AnchorUnsafe,
		stageErr,
	)
	if err != nil || quarantined.Anchor != resumestate.AnchorUnsafe || quarantined.Stage != resumestate.EntryNotObserved {
		t.Fatalf("quarantine stage observation = %#v, err %v", quarantined, err)
	}
}

func TestOutputV3RecoveryResultHelpersPreserveTerminalState(t *testing.T) {
	t.Parallel()
	state := fileRecoveryState{parentSynced: true}
	if got := continuingFileRecovery(state); got.terminal || !got.state.parentSynced {
		t.Fatalf("continuing result = %#v", got)
	}
	if got, err := finishFileRecovery(transfer.FileStart{}, nil); err != nil || !got.terminal {
		t.Fatalf("finished result = %#v, err %v", got, err)
	}
	if err := closeOutputV3File(nil); err != nil {
		t.Fatalf("closing nil output file = %v", err)
	}
}

func TestOutputV3RecoveryGateEntryObservationMapping(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		kind outputcap.EntryKind
		want resumestate.UpdateTemporaryEntryObservation
	}{
		{name: "absent", kind: outputcap.EntryAbsent, want: resumestate.UpdateTemporaryEntryMissing},
		{name: "regular file", kind: outputcap.EntryRegularFile, want: resumestate.UpdateTemporaryEntryRegular},
		{name: "unrecognized", kind: outputcap.EntryKind(0xff), want: resumestate.UpdateTemporaryEntryUnsafe},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := updateTemporaryEntryForGateKind(test.kind); got != test.want {
				t.Fatalf("entry observation = %v, want %v", got, test.want)
			}
		})
	}
}

func TestOutputV3RecoveryObservationFailureClassifiesNamespaceEvidence(t *testing.T) {
	t.Parallel()
	unsafe, err := classifyFinalObservationFailure(outputcap.ErrUnsafeNamespace)
	if err != nil || unsafe.entry != resumestate.EntryUnsafe {
		t.Fatalf("unsafe observation = %#v, err %v", unsafe, err)
	}

	denied := errors.New("directory observation denied")
	if _, err := classifyFinalObservationFailure(denied); !errors.Is(err, denied) {
		t.Fatalf("operational observation error = %v, want %v", err, denied)
	}
}

func TestOutputV3RecoveryCleanupFaultIsRecognizable(t *testing.T) {
	t.Parallel()
	err := internalCleanupNeedsAttentionFault("remove ambiguous stage")
	if !isInternalCleanupNeedsAttentionFault(err) {
		t.Fatalf("cleanup fault was not recognized: %v", err)
	}
	if !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("cleanup fault lost namespace evidence: %v", err)
	}
	if !outputV3FailureRequiresJobPause(err) {
		t.Fatalf("cleanup fault does not require a job pause: %v", err)
	}
	if isInternalCleanupNeedsAttentionFault(errors.New("unrelated")) {
		t.Fatal("unrelated error was recognized as cleanup attention")
	}
}

func TestOutputV3RecoveryActionRejectsUnsupported(t *testing.T) {
	t.Parallel()
	var session *Session
	_, err := session.applyFileRecoveryAction(
		transfer.OutputFile{}, nil, "", fileRecoveryState{},
		fileRecoveryIteration{},
	)
	assertOutputV3Fault(t, err, transfer.OutputFaultFile, transfer.OutputFaultContract)
}

func TestOutputV3RecoveryUnclassifiedPublicationAncestryRequiresPause(t *testing.T) {
	t.Parallel()
	var session *Session
	_, handled, err := session.handleUnclassifiedRecoveredPublication(
		transfer.OutputFile{}, nil, "", fileRecoveryState{},
		recoveryPublicationAttempt{linkErr: errOutputAncestryUnsafe},
	)
	if !handled || !errors.Is(err, errOutputAncestryUnsafe) {
		t.Fatalf("unclassified ancestry result handled=%t err=%v", handled, err)
	}
	if !outputV3FailureRequiresJobPause(err) {
		t.Fatalf("ancestry result does not require a job pause: %v", err)
	}

	_, handled, err = session.handleUnclassifiedRecoveredPublication(
		transfer.OutputFile{}, nil, "", fileRecoveryState{},
		recoveryPublicationAttempt{cleanupErr: errors.New("witness close failed")},
	)
	if !handled || err == nil {
		t.Fatalf("cleanup-only publication result handled=%t err=%v", handled, err)
	}
}

func TestOutputV3TerminalPureDecisionBranches(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		job  transfer.JobPauseReason
		want transfer.FilePauseReason
	}{
		{transfer.JobPauseInterrupted, transfer.FilePauseInterrupted},
		{transfer.JobPauseShutdown, transfer.FilePauseShutdown},
		{transfer.JobPauseTransportFailure, transfer.FilePauseTransportFailure},
		{transfer.JobPauseSessionFailure, transfer.FilePauseSessionFailure},
		{transfer.JobPauseOutputFailure, transfer.FilePauseOutputFailure},
		{transfer.JobPauseReason(0), transfer.FilePauseOutputFailure},
	} {
		if got := filePauseReasonForJob(test.job); got != test.want {
			t.Errorf("job pause %v maps to %v, want %v", test.job, got, test.want)
		}
	}

	plain := errors.New("plain settlement failure")
	wrapped := sessionSettlementFailure(plain)
	assertOutputV3Fault(t, wrapped, transfer.OutputFaultSession, transfer.OutputFaultStateIO)
	contract := outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultContract, plain)
	if got := sessionSettlementFailure(contract); got != contract {
		t.Fatalf("typed settlement failure was rewrapped: %v", got)
	}

	for action := resumestate.RecoveryRetryObjectCreation; action <= resumestate.RecoveryHoldRetiringCleanup; action++ {
		want := action == resumestate.RecoveryInstallQuarantine || action == resumestate.RecoveryHoldQuarantine
		if got := publishedRetirementAllowsObservationCleanup(action); got != want {
			t.Errorf("published retirement action %v allows cleanup = %t, want %t", action, got, want)
		}
	}

	if _, _, _, err := (*Session)(nil).applyPublishedRetirementDecision(
		resumestate.RecoveryDecision{}, nil, nil, "", resumestate.BoundFileRecord{}, false,
	); err == nil {
		t.Fatal("unsupported published-retirement action unexpectedly succeeded")
	}
	if _, _, _, err := holdPublishedRetirementQuarantine(resumestate.BoundFileRecord{}, nil); err == nil {
		t.Fatal("invalid held quarantine unexpectedly succeeded")
	}
	if _, _, _, err := holdPublishedRetirementQuarantine(resumestate.BoundFileRecord{}, errors.New("observation close failed")); err == nil {
		t.Fatal("held quarantine cleanup failure unexpectedly succeeded")
	}
}

func TestOutputV3DiscardHeaderPureTransitionsAndReplacementOutcomes(t *testing.T) {
	t.Parallel()
	opaque := ResumeStateRef{kind: ResumeStateOpaqueUnsafe}
	if namespace, valid, corrupt, err := installDiscardingHeader(
		outputnamespace.Store{}, resumestate.Control{}, nil, opaque, false, nil,
	); err != nil || valid || corrupt || namespace.Header().Lifecycle() != 0 {
		t.Fatalf("opaque discard header = %#v, valid=%t corrupt=%t err=%v", namespace, valid, corrupt, err)
	}
	if namespace, valid, corrupt, err := authorizeLocklessDiscardHeader(resumestate.SessionNamespaceAuthority{}); err != nil || valid || corrupt || namespace.Header().Lifecycle() != 0 {
		t.Fatalf("unbound lockless discard header = %#v, valid=%t corrupt=%t err=%v", namespace, valid, corrupt, err)
	}

	if got := nextDiscardLifecycle(resumestate.SessionPausing); got != resumestate.SessionPaused {
		t.Fatalf("pausing lifecycle advances to %v, want paused", got)
	}
	if got := nextDiscardLifecycle(resumestate.SessionPaused); got != resumestate.SessionDiscarding {
		t.Fatalf("paused lifecycle advances to %v, want discarding", got)
	}

	next := resumestate.SessionNamespaceAuthority{}
	if got, err := settleDiscardHeaderReplacement(outputnamespace.ReplaceAdopted, nil, next); err != nil || got != next {
		t.Fatalf("adopted replacement = %#v, err %v", got, err)
	}
	for _, test := range []struct {
		name    string
		outcome outputnamespace.ReplaceOutcome
		err     error
		wantErr bool
	}{
		{name: "adopted close failure", outcome: outputnamespace.ReplaceAdopted, err: errors.New("close failed"), wantErr: true},
		{name: "unchanged", outcome: outputnamespace.ReplaceUnchanged, wantErr: true},
		{name: "unchanged explicit", outcome: outputnamespace.ReplaceUnchanged, err: errors.New("compare failed"), wantErr: true},
		{name: "uncertain", outcome: outputnamespace.ReplaceUncertain, err: errors.New("replace uncertain"), wantErr: true},
		{name: "invalid outcome", outcome: outputnamespace.ReplaceOutcome(0), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := settleDiscardHeaderReplacement(test.outcome, test.err, next)
			if !test.wantErr {
				t.Fatalf("test case must expect an error")
			}
			if err == nil {
				t.Fatal("replacement failure returned nil error")
			}
		})
	}
}

func TestOutputV3SettledRecoveryCollisionInstallsPublishBlockedCut(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, true, 8)
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, &v3RecoverySessionIDs{}), root, selection)
	file := v3RecoveryOutputFile(t, opened.Session, selection, 8)
	transaction := v3RecoveryBeginTransaction(t, opened.Session, file).(*FileTransaction)
	t.Cleanup(func() {
		if transaction.lifecycle != FileTransactionClosed {
			outputV3AbandonTransaction(t, transaction)
		}
		v3RecoveryCloseSession(t, opened.Session)
	})
	if err := transaction.WriteRange(context.Background(), 0, bytes.Repeat([]byte{0x41}, 8)); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := transaction.data.SetModifiedTime(transaction.descriptor.ModifiedTime()); err != nil {
		t.Fatal(err)
	}
	if err := transaction.data.Sync(); err != nil {
		t.Fatal(err)
	}
	publishing, err := resumestate.PreparePublication(transaction.resumable)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Session.installFileRecord(transaction.recordDir, transaction.recordName, transaction.resumable.Bound(), publishing); err != nil {
		t.Fatal(err)
	}
	transaction.resumable, err = resumestate.BindResumableFile(publishing, transaction.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	result, err := opened.Session.settleClassifiedRecoveredPublication(
		file, transaction.recordDir, transaction.recordName,
		fileRecoveryState{resumable: transaction.resumable},
		recoveryPublicationAttempt{result: resumestate.PublishAlreadyExistsDifferent},
	)
	if err != nil {
		t.Fatal(err)
	}
	settlement, settled := result.start.ImmediateSettlement()
	if !result.terminal || !settled || settlement.Kind() != transfer.FilePublishBlocked {
		t.Fatalf("publish collision result = %#v", result)
	}
}
