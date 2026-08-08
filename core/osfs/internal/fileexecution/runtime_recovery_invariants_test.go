package fileexecution

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
)

func TestRetirementRetriesOnlyBeforeDurableRetiredState(t *testing.T) {
	t.Run("known unchanged checkpoint can retry", func(t *testing.T) {
		fixture := newRuntimeExecutionFixture(t, 1)
		fixture.repository.hooks = append(fixture.repository.hooks, func(
			repository *fakeCheckpointRepository,
			_ *checkpointmodel.Record,
			_ checkpointmodel.Record,
		) (CheckpointObservation, error) {
			observation, _ := ObservedCheckpoint(repository.record)
			return observation, errors.New("retirement record was not replaced")
		})

		settlement, cut, err := fixture.transaction.Retire(
			context.Background(), transfer.FileRetireInvalidatedRevision,
		)
		if err == nil || cut != outputsession.MutationNoChange || settlement.Kind() != 0 {
			t.Fatalf("unchanged retirement cut=%v settlement=%v err=%v", cut, settlement.Kind(), err)
		}
		if record := fixture.repository.current(t); record.Phase() != checkpointmodel.PhaseActive {
			t.Fatalf("unchanged retirement phase=%v want active", record.Phase())
		}
		if len(fixture.platform.retirement) != 0 {
			t.Fatalf("unchanged retirement deleted state: %v", fixture.platform.retirement)
		}

		settlement, cut, err = fixture.transaction.Retire(
			context.Background(), transfer.FileRetireInvalidatedRevision,
		)
		if err != nil || cut != outputsession.MutationStable || settlement.Kind() != transfer.FileRetired {
			t.Fatalf("retirement retry cut=%v settlement=%v err=%v", cut, settlement.Kind(), err)
		}
	})

	t.Run("unsafe public state quarantines instead of deleting", func(t *testing.T) {
		fixture := newRuntimeExecutionFixture(t, 1)
		fixture.namespace.mu.Lock()
		fixture.namespace.condition = FinalUnsafe
		fixture.namespace.mu.Unlock()

		settlement, cut, err := fixture.transaction.Retire(
			context.Background(), transfer.FileRetireInvalidatedRevision,
		)
		if err != nil || cut != outputsession.MutationStable || settlement.Kind() != transfer.FileQuarantined {
			t.Fatalf("unsafe retirement cut=%v settlement=%v err=%v", cut, settlement.Kind(), err)
		}
		record := fixture.repository.current(t)
		if record.Phase() != checkpointmodel.PhaseQuarantined ||
			record.QuarantineReason() != checkpointmodel.QuarantineFinalUnsafe {
			t.Fatalf("unsafe retirement phase=%v reason=%v", record.Phase(), record.QuarantineReason())
		}
		if len(fixture.platform.retirement) != 0 {
			t.Fatalf("unsafe retirement deleted evidence: %v", fixture.platform.retirement)
		}
	})

	t.Run("destination close failure retains durable reason", func(t *testing.T) {
		fixture := newRuntimeExecutionFixture(t, 1)
		fixture.transaction.destination = &closeFailDestination{
			FileDestination: fixture.transaction.destination,
			err:             errors.New("destination close failed after retirement"),
		}

		settlement, cut, err := fixture.transaction.Retire(
			context.Background(), transfer.FileRetireIsolatedPermanentSourceFailure,
		)
		if err == nil || cut != outputsession.MutationAmbiguous || settlement.Kind() != 0 {
			t.Fatalf("retirement close cut=%v settlement=%v err=%v", cut, settlement.Kind(), err)
		}
		record := fixture.repository.current(t)
		if record.Phase() != checkpointmodel.PhaseRetired ||
			record.RetirementReason() != checkpointmodel.RetirementIsolatedFailure {
			t.Fatalf("retirement close phase=%v reason=%v", record.Phase(), record.RetirementReason())
		}
		state := fixture.platform.onlyState(t)
		state.mu.Lock()
		condition := state.condition
		state.mu.Unlock()
		if condition != OwnedAbsent {
			t.Fatalf("retirement cleanup did not finish before close failure: %v", condition)
		}
	})

	t.Run("owned file close failure retains retired checkpoint and stage", func(t *testing.T) {
		fixture := newRuntimeExecutionFixture(t, 1)
		fixture.transaction.file = &closeFailOwnedFile{
			OwnedFile: fixture.transaction.file,
			err:       errors.New("owned handle close failed after retirement record"),
		}

		settlement, cut, err := fixture.transaction.Retire(
			context.Background(), transfer.FileRetireInvalidatedRevision,
		)
		if err == nil || cut != outputsession.MutationAmbiguous || settlement.Kind() != 0 {
			t.Fatalf("retired file close cut=%v settlement=%v err=%v", cut, settlement.Kind(), err)
		}
		record := fixture.repository.current(t)
		if record.Phase() != checkpointmodel.PhaseRetired ||
			record.RetirementReason() != checkpointmodel.RetirementInvalidatedRevision {
			t.Fatalf("retired file close phase=%v reason=%v", record.Phase(), record.RetirementReason())
		}
		state := fixture.platform.onlyState(t)
		state.mu.Lock()
		condition := state.condition
		state.mu.Unlock()
		if condition != OwnedReady || len(fixture.platform.retirement) != 0 {
			t.Fatalf("retirement close failure condition=%v retirement=%v", condition, fixture.platform.retirement)
		}
	})
}

func TestRecoveryUsesReadyObservationDespiteDiagnosticError(t *testing.T) {
	fixture := newRuntimeExecutionFixture(t, 1)
	if _, _, err := fixture.transaction.Pause(
		context.Background(), transfer.FilePauseInterrupted,
	); err != nil {
		t.Fatal(err)
	}
	fixture.platform.openErr = errors.New("open returned after exact owned observation")
	recovery := newTestEngine(t, fixture.identity, fixture.directories, fixture.platform, fixture.repository)
	var events []TraceEvent
	recovery.trace = TraceSinkFunc(func(event TraceEvent) { events = append(events, event) })

	observation, err := recovery.BeginFile(context.Background(), fixture.identity.claim)
	if err != nil || observation.Cut != outputsession.MutationStable || observation.Transaction == nil {
		t.Fatalf("ready recovery cut=%v transaction=%T err=%v", observation.Cut, observation.Transaction, err)
	}
	event := traceByOperation(t, events, TraceRecoverFile)
	if event.Outcome != TraceReconciled || event.Previous != checkpointmodel.PhasePaused ||
		event.Next != checkpointmodel.PhaseActive {
		t.Fatalf("ready recovery trace=%+v", event)
	}
}

func TestPublishingRecoveryConvergesCollisionWithoutRepublishing(t *testing.T) {
	fixture := newReducerFixture(t, 1)
	record, err := publishingRecord(fixture.full)
	if err != nil {
		t.Fatal(err)
	}
	repository := &fakeCheckpointRepository{present: true, record: record}
	platform := newFakePlatform()
	platform.objects[fixture.object] = &fakeOwnedState{
		object: fixture.object, condition: OwnedReady, data: []byte("x"), writeN: -1,
	}
	namespace := newFakePublicNamespace()
	namespace.condition = FinalCollision

	observation, err := newTestEngine(
		t, fixture.engineFixture, &fakeDirectoryAuthority{namespace: namespace}, platform, repository,
	).BeginFile(context.Background(), fixture.claim)
	if err != nil || observation.Cut != outputsession.MutationStable ||
		observation.Settlement.Kind() != transfer.FilePublishBlocked {
		t.Fatalf("publishing collision cut=%v settlement=%v err=%v", observation.Cut, observation.Settlement.Kind(), err)
	}
	if current := repository.current(t); current.Phase() != checkpointmodel.PhasePaused {
		t.Fatalf("publishing collision phase=%v want paused", current.Phase())
	}
	namespace.mu.Lock()
	publishCalls := namespace.publishCalls
	namespace.mu.Unlock()
	if publishCalls != 0 {
		t.Fatalf("known collision repeated no-replace publication %d times", publishCalls)
	}
}
