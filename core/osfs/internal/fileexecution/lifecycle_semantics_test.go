package fileexecution

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

func TestOwnedLifecycleValuesNeverManufactureCleanupAuthority(t *testing.T) {
	object, err := checkpointmodel.ObjectIDFromBytes(bytes.Repeat(
		[]byte{0xb1}, transfer.OwnedObjectIdentityBytes,
	))
	if err != nil {
		t.Fatal(err)
	}
	for condition := OwnedAbsent; condition <= OwnedStageUnsafe; condition++ {
		observation, err := NewOwnedObservation(object, condition)
		if err != nil || !observation.ValidForCleanup(object) {
			t.Fatalf("owned condition %d = (%+v, %v)", condition, observation, err)
		}
	}
	if (OwnedObservation{}).ValidForCleanup(object) ||
		(OwnedObservation{object: object, condition: OwnedCondition(255)}).ValidForCleanup(object) {
		t.Fatal("invalid observation became cleanup authority")
	}
	for condition := FinalAbsent; condition <= FinalUnsafe; condition++ {
		observation, err := ObserveFinal(condition)
		if err != nil || observation.Condition() != condition || !observation.valid() {
			t.Fatalf("final condition %d = (%+v, %v)", condition, observation, err)
		}
	}

	var file *LiveOwnedFile
	if !file.ObjectID().IsZero() || file.NativeFile() != nil ||
		file.CleanupTicket().Valid() {
		t.Fatal("nil live file exposed owned evidence")
	}
	if _, err := file.WriteAt([]byte("x"), 0); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("nil live write = %v", err)
	}
	if err := file.Sync(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("nil live sync = %v", err)
	}
	if err := file.SetModifiedTime(catalog.ModifiedTime{}); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("nil live metadata = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("nil live close = %v", err)
	}
	if _, err := NewLiveOwnedFile(object, nil, checkpointmodel.LiveCleanupTicket{}); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("invalid live owned file = %v", err)
	}
}

func TestTransactionHelpersPreserveTypedLifecycleSemantics(t *testing.T) {
	warnings := settlementMetadataWarnings([]MetadataWarning{
		{kind: MetadataWarningKind(255)},
		{kind: MetadataModifiedTimeWarning},
	})
	if len(warnings) != 1 || warnings[0] != transfer.FileMetadataModifiedTime {
		t.Fatalf("settlement warnings = %+v", warnings)
	}
	if rangesIntersect(contentRangeSet(t, 2, 4), 0, 2) ||
		!rangesIntersect(contentRangeSet(t, 2, 4), 3, 5) {
		t.Fatal("range intersection boundaries changed")
	}
	for _, test := range []struct {
		reason checkpointmodel.QuarantineReason
		want   transfer.ItemBlockReason
	}{
		{checkpointmodel.QuarantinePublicationHistory, transfer.ItemBlockPublicationAmbiguous},
		{checkpointmodel.QuarantineFinalMismatch, transfer.ItemBlockPublicationAmbiguous},
		{checkpointmodel.QuarantineFinalUnsafe, transfer.ItemBlockPublicationAmbiguous},
		{checkpointmodel.QuarantineMetadataMismatch, transfer.ItemBlockPublicationAmbiguous},
		{checkpointmodel.QuarantinePartialObjectCreation, transfer.ItemBlockRetirementUncertain},
		{checkpointmodel.QuarantineUpdateTemporary, transfer.ItemBlockStateCorrupt},
		{checkpointmodel.QuarantineOutputObjectDuplicate, transfer.ItemBlockStateCorrupt},
		{checkpointmodel.QuarantineAnchorMissing, transfer.ItemBlockOwnershipUnknown},
	} {
		if got := transferQuarantineReason(test.reason); got != test.want {
			t.Fatalf("quarantine reason %d = %d, want %d", test.reason, got, test.want)
		}
	}
	var resumable *resumablePartialFileTransaction
	if resumable.Binding() != (transfer.MaterializedFileBinding{}) {
		t.Fatal("nil resumable transaction exposed a binding")
	}
	if err := resumable.WriteRange(context.Background(), 0, nil); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("nil resumable write = %v", err)
	}
	if _, err := resumable.Checkpoint(context.Background()); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("nil resumable checkpoint = %v", err)
	}
	if _, err := resumable.Commit(context.Background()); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("nil resumable commit = %v", err)
	}
}

func TestRecoveryTraceDistinguishesItemBlockedFromGenericRecovery(t *testing.T) {
	for action := RecoveryOpenActive; action <= RecoveryInstallQuarantine; action++ {
		decision := RecoveryDecision{action: action}
		want := TraceRecoverFile
		switch action {
		case RecoveryReturnQuarantined, RecoveryInstallQuarantine:
			want = TraceItemBlocked
		}
		if got := traceOperationForRecovery(decision); got != want {
			t.Fatalf("recovery action %d trace operation = %d, want %d", action, got, want)
		}
		wantOutcome := TraceReconciled
		if action == RecoveryReturnCollision {
			wantOutcome = TraceCollision
		}
		if got := traceOutcomeForRecovery(decision); got != wantOutcome {
			t.Fatalf("recovery action %d trace outcome = %d, want %d", action, got, wantOutcome)
		}
	}
}

func TestTerminalTransactionsEmitPauseRetireAndItemBlockedMilestones(t *testing.T) {
	t.Run("pause", func(t *testing.T) {
		transaction, _, _, _ := newCoveragePartialFileTransaction(t, 1)
		var events []TraceEvent
		transaction.engine.trace = TraceSinkFunc(func(event TraceEvent) { events = append(events, event) })
		if _, err := transaction.Pause(context.Background(), transfer.FilePauseInterrupted); err != nil {
			t.Fatal(err)
		}
		last := events[len(events)-1]
		if last.Operation != TracePause || last.Outcome != TraceSucceeded ||
			last.Previous != checkpointmodel.PhaseActive || last.Next != checkpointmodel.PhasePaused {
			t.Fatalf("pause trace = %+v", last)
		}
	})

	t.Run("retirement item blocked", func(t *testing.T) {
		transaction, _, platform, _ := newCoveragePartialFileTransaction(t, 1)
		var events []TraceEvent
		transaction.engine.trace = TraceSinkFunc(func(event TraceEvent) { events = append(events, event) })
		platform.retirementErr = errors.New("retirement cut unknown")
		settlement, err := transaction.Retire(
			context.Background(), transfer.FileRetireInvalidatedRevision,
		)
		if err != nil || settlement.Kind() != transfer.FileItemBlocked {
			t.Fatalf("retirement settlement = (%d, %v)", settlement.Kind(), err)
		}
		last := events[len(events)-1]
		if last.Operation != TraceItemBlocked || last.Outcome != TraceReconciled ||
			last.Previous != checkpointmodel.PhaseActive || last.Next != checkpointmodel.PhaseQuarantined {
			t.Fatalf("retirement trace = %+v", last)
		}
	})
}

func contentRangeSet(t *testing.T, offset, end uint64) content.RangeSet {
	t.Helper()
	ranges, err := content.NewRangeSet([]content.Range{{Offset: offset, End: end}})
	if err != nil {
		t.Fatal(err)
	}
	return ranges
}
