package outputruntime

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3TerminalHoldQuarantineKeepsValidReason(t *testing.T) {
	t.Parallel()
	_, _, _, retiring, _ := outputV3PreparedRetirement(t)
	quarantined, err := resumestate.PrepareUnsafeNamespaceQuarantine(
		retiring,
		resumestate.QuarantineStageUnsafe,
	)
	if err != nil {
		t.Fatalf("prepare quarantine: %v", err)
	}

	outcome, synced, repeat, err := holdPublishedRetirementQuarantine(quarantined, nil)
	if err != nil {
		t.Fatalf("hold valid quarantine: %v", err)
	}
	if outcome.disposition != publishedRetirementQuarantineInstalled ||
		outcome.quarantineReason != resumestate.QuarantineStageUnsafe {
		t.Fatalf("held quarantine outcome = %#v", outcome)
	}
	if synced || repeat {
		t.Fatalf("held quarantine requested follow-up: synced=%t repeat=%t", synced, repeat)
	}

	cleanupErr := errors.New("observation close failed")
	if _, _, _, err := holdPublishedRetirementQuarantine(quarantined, cleanupErr); !errors.Is(err, cleanupErr) {
		t.Fatalf("held quarantine cleanup error = %v, want %v", err, cleanupErr)
	}
}

func TestOutputV3DiscardHeaderReadBindsTheFixedSessionNamespace(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	session := opened.Session
	defer v3RecoveryCloseSession(t, session)

	reference := ResumeStateRef{
		rootPath:      root,
		root:          session.stateSnapshot().Header().OutputRoot(),
		intent:        session.resumeIntent,
		session:       session.sessionID,
		kind:          ResumeStateRecoverable,
		namespaceName: resumestate.ResumeNamespaceName(session.resumeIntent),
		sessionName:   resumestate.SessionDirectoryName(session.sessionID),
	}
	namespace, valid, corrupt, err := readDiscardHeader(session.control.Control(), session.sessionDir, reference)
	if err != nil {
		t.Fatalf("read discard header: %v", err)
	}
	if !valid || corrupt {
		t.Fatalf("discard header validity = valid:%t corrupt:%t", valid, corrupt)
	}
	if namespace.Header() != session.stateSnapshot().Header() {
		t.Fatalf("discard header did not preserve the bound session header")
	}

	if namespace, valid, corrupt, err := installDiscardingHeader(
		outputnamespace.Store{},
		session.control.Control(),
		session.sessionDir,
		reference,
		false,
		func() error { return nil },
	); err != nil || valid || corrupt || namespace.Header().Lifecycle() != 0 {
		t.Fatalf("lockless live discard header = valid:%t corrupt:%t lifecycle:%v err:%v", valid, corrupt, namespace.Header().Lifecycle(), err)
	}
}

func TestOutputV3RetirementDecisionHelpersClassifyCleanupAndSettlement(t *testing.T) {
	t.Parallel()
	cleanupErr := errors.New("observation close failed")
	if err := fileRetirementObservationCleanupFault(resumestate.RecoveryDecision{}, cleanupErr); err == nil {
		t.Fatal("ordinary retirement cleanup failure returned nil")
	}
	if err := fileRetirementObservationCleanupFault(resumestate.RecoveryDecision{}, nil); err != nil {
		t.Fatalf("nil cleanup error classified as fault: %v", err)
	}

	if settlement, err := retiredFileSettlement(transfer.OutputFileBinding{}, resumestate.RecoveryDecision{}); err != nil || settlement.Kind() != 0 {
		t.Fatalf("empty retirement binding settlement = (%v, %v)", settlement.Kind(), err)
	}

	_, _, _, retiring, binding := outputV3PreparedRetirement(t)
	settlement, err := retiredFileSettlement(binding, resumestate.RecoveryDecision{})
	if err != nil || settlement.Kind() != transfer.FileRetired {
		t.Fatalf("retired settlement = (%v, %v)", settlement.Kind(), err)
	}
	quarantined, err := resumestate.PrepareUnsafeNamespaceQuarantine(
		retiring,
		resumestate.QuarantineStageUnsafe,
	)
	if err != nil {
		t.Fatalf("prepare held quarantine: %v", err)
	}
	decision, err := resumestate.ReduceFileRecovery(
		quarantined,
		resumestate.FileObservation{Anchor: resumestate.AnchorMissing, Stage: resumestate.EntryMissing},
	)
	if err != nil || decision.Action() != resumestate.RecoveryHoldQuarantine {
		t.Fatalf("quarantine hold decision = (%v, %v)", decision.Action(), err)
	}
	if err := fileRetirementObservationCleanupFault(decision, cleanupErr); err != nil {
		t.Fatalf("quarantine cleanup failure should be ignored: %v", err)
	}
	held, err := heldRetirementQuarantine(binding, quarantined.Record(), nil)
	if err != nil || held.settlement.Kind() != transfer.FileQuarantined {
		t.Fatalf("held retirement quarantine = (%v, %v)", held.settlement.Kind(), err)
	}
}

func TestOutputV3PublicationRejectsMissingWitnessBeforeIOL(t *testing.T) {
	t.Parallel()
	var session *Session
	result, operationErr, cleanupErr := session.linkFinalNoReplaceResult(resumestate.BoundFileRecord{}, nil)
	if result != 0 || cleanupErr != nil || !errors.Is(operationErr, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("missing publication witness = (result=%v, operation=%v, cleanup=%v)", result, operationErr, cleanupErr)
	}
}
