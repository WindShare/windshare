package resumeauthority

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
)

func TestDiscardTreatsDuplicatePublicationPinsAsIntentAtomicAmbiguity(t *testing.T) {
	binding, record := resumeRecord(
		t, 0xa1, 0xb1, checkpointmodel.PhasePublished, checkpointmodel.CommitPublished,
	)
	checkpoint := mustCheckpointObservation(
		t, record, EvidenceExact, EvidenceExact, EvidenceExact,
	)
	snapshot := mustSnapshot(
		t, binding, []CheckpointObservation{checkpoint, checkpoint}, nil,
	)
	fixture := newAuthorityExecutorFixture(t, snapshot, map[checkpointmodel.RecordID]Evidence{
		record.RecordID(): EvidenceExact,
	})

	result, err := Discard(context.Background(), fixture.reference)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status() != DiscardNeedsAttention || result.RemovedArtifacts() != 0 ||
		!hasAttentionReason(result.Attention(), AttentionAmbiguousPublication) {
		t.Fatalf("duplicate publication settlement = %+v", result)
	}
	if len(fixture.leased.actionKinds()) != 0 {
		t.Fatalf("ambiguous publication authorized actions: %v", fixture.leased.actionKinds())
	}
	if len(fixture.observer.pins) != 2 {
		t.Fatalf("publication pins = %d, want 2", len(fixture.observer.pins))
	}
	for _, pin := range fixture.observer.pins {
		if pin.closeCalls != 1 {
			t.Fatalf("ambiguous publication pin close calls = %d", pin.closeCalls)
		}
	}
	if err := fixture.inventory.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDiscardSettlesStaleAdapterResultsWithoutInventingRemovedArtifacts(t *testing.T) {
	binding, record := resumeRecord(
		t, 0xa2, 0xb2, checkpointmodel.PhasePaused, checkpointmodel.CommitVerified,
	)
	checkpoint := mustCheckpointObservation(
		t, record, EvidenceExact, EvidenceExact, EvidenceExact,
	)
	snapshot := mustSnapshot(t, binding, []CheckpointObservation{checkpoint}, nil)
	publications := map[checkpointmodel.RecordID]Evidence{record.RecordID(): EvidenceAbsent}

	t.Run("all removals already satisfied", func(t *testing.T) {
		fixture := newAuthorityExecutorFixture(t, snapshot, publications)
		script := &settlementScriptLease{
			authorityExecutorLease: fixture.leased,
			statuses: []ApplyStatus{
				ApplyAlreadySatisfied,
				ApplyCompleted,
				ApplyAlreadySatisfied,
				ApplyCompleted,
				ApplyAlreadySatisfied,
				ApplyCompleted,
			},
		}
		fixture.pinned.leased = script

		result, err := Discard(context.Background(), fixture.reference)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status() != AlreadyAbsent || result.RemovedArtifacts() != 0 {
			t.Fatalf("stale-plan settlement = %v/%d", result.Status(), result.RemovedArtifacts())
		}
		want := []ActionKind{
			ActionRemoveStage, ActionSyncStages,
			ActionRemoveAnchor, ActionSyncAnchors,
			ActionRemoveRecord, ActionSyncRecords,
		}
		if !reflect.DeepEqual(fixture.leased.actionKinds(), want) {
			t.Fatalf("stale-plan actions = %v, want %v", fixture.leased.actionKinds(), want)
		}
		if checkpoint.RecordEvidence() != EvidenceExact ||
			checkpoint.StageEvidence() != EvidenceExact ||
			checkpoint.AnchorEvidence() != EvidenceExact ||
			snapshot.Binding() != binding {
			t.Fatal("discard observation lost its opaque evidence or certified binding")
		}
		if err := fixture.inventory.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("adapter needs attention is a stable settlement", func(t *testing.T) {
		fixture := newAuthorityExecutorFixture(t, snapshot, publications)
		attention := derivedAttention(AttentionReplacement, record.RecordID().Bytes())
		script := &settlementScriptLease{
			authorityExecutorLease: fixture.leased,
			statuses:               []ApplyStatus{ApplyNeedsAttention},
			attention:              []Attention{attention},
		}
		fixture.pinned.leased = script

		result, err := Discard(context.Background(), fixture.reference)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status() != DiscardNeedsAttention || result.RemovedArtifacts() != 0 ||
			len(result.Attention()) != 1 || result.Attention()[0] != attention {
			t.Fatalf("adapter-attention settlement = %+v", result)
		}
		if got := fixture.leased.actionKinds(); !reflect.DeepEqual(got, []ActionKind{ActionRemoveStage}) {
			t.Fatalf("actions after stable attention = %v", got)
		}
		if err := fixture.inventory.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestDiscardKeepsCancellationAndPublicationRevalidationFailuresOutOfSettlement(t *testing.T) {
	binding, record := resumeRecord(
		t, 0xa3, 0xb3, checkpointmodel.PhasePublished, checkpointmodel.CommitPublished,
	)
	snapshot := mustSnapshot(t, binding, []CheckpointObservation{
		mustCheckpointObservation(t, record, EvidenceExact, EvidenceExact, EvidenceExact),
	}, nil)

	t.Run("publication changed before first action", func(t *testing.T) {
		fixture := newAuthorityExecutorFixture(t, snapshot, map[checkpointmodel.RecordID]Evidence{
			record.RecordID(): EvidenceExact,
		})
		fixture.observer.revalidate[record.RecordID()] = []Evidence{EvidenceReplaced}
		result, err := Discard(context.Background(), fixture.reference)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status() != DiscardNeedsAttention || result.RemovedArtifacts() != 0 ||
			!hasAttentionReason(result.Attention(), AttentionReplacement) {
			t.Fatalf("pre-action replacement settlement = %+v", result)
		}
		if len(fixture.leased.actionKinds()) != 0 {
			t.Fatal("publication replacement allowed a private witness removal")
		}
		if err := fixture.inventory.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("canceled call does not consume reference", func(t *testing.T) {
		fixture := newAuthorityExecutorFixture(t, snapshot, map[checkpointmodel.RecordID]Evidence{
			record.RecordID(): EvidenceExact,
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := Discard(ctx, fixture.reference); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled discard error = %v", err)
		}
		result, err := Discard(context.Background(), fixture.reference)
		if err != nil || result.Status() != Discarded {
			t.Fatalf("reference after canceled call = %+v, %v", result, err)
		}
		if err := fixture.inventory.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestDiscardRequiresFinalWitnessWhilePublishedRetirementIsQuarantined(t *testing.T) {
	binding, record := resumeRecordWithClaims(
		t,
		0xa4,
		0xb4,
		checkpointmodel.PhaseQuarantined,
		checkpointmodel.CommitQuarantined,
		checkpointmodel.QuarantinePartialObjectCreation,
		checkpointmodel.QuarantineOriginRetiring,
		checkpointmodel.RetirementPublished,
	)
	snapshot := mustSnapshot(t, binding, []CheckpointObservation{
		mustCheckpointObservation(t, record, EvidenceExact, EvidenceAbsent, EvidenceExact),
	}, nil)

	exact := ReduceDiscard(snapshot, []PublicationObservation{
		mustPublicationObservation(t, record.RecordID(), EvidenceExact),
	})
	if !exact.Valid() || exact.ExpectedStatus() != Discarded {
		t.Fatalf("quarantined published-retirement plan = %+v", exact)
	}
	for _, action := range exact.Actions() {
		if action.ObjectID() != record.OwnedOutputObject() {
			t.Fatal("discard action lost its private object binding")
		}
	}

	missing := ReduceDiscard(snapshot, []PublicationObservation{
		mustPublicationObservation(t, record.RecordID(), EvidenceAbsent),
	})
	if !missing.Valid() || missing.ExpectedStatus() != DiscardNeedsAttention ||
		len(missing.Actions()) != 0 ||
		!hasAttentionReason(missing.Attention(), AttentionAmbiguousPublication) {
		t.Fatalf("missing published-retirement witness plan = %+v", missing)
	}
}

type settlementScriptLease struct {
	*authorityExecutorLease
	statuses  []ApplyStatus
	attention []Attention
}

func (lease *settlementScriptLease) Apply(
	_ context.Context,
	action Action,
) (ApplyResult, error) {
	lease.actions = append(lease.actions, action)
	index := len(lease.actions) - 1
	status := ApplyCompleted
	if index < len(lease.statuses) {
		status = lease.statuses[index]
	}
	if status == ApplyNeedsAttention {
		return NewApplyResult(status, lease.attention)
	}
	return NewApplyResult(status, nil)
}
