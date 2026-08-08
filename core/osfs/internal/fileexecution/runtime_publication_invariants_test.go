package fileexecution

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
)

func TestMetadataMutationUsesFreshObservationBeforePublication(t *testing.T) {
	t.Run("set error with exact metadata reconciles", func(t *testing.T) {
		fixture := newRuntimeExecutionFixture(t, 1)
		fixture.writeAndCheckpoint(t, []byte("x"))
		state := fixture.platform.onlyState(t)
		state.mu.Lock()
		state.setErr = errors.New("set-time returned after applying metadata")
		state.mu.Unlock()
		var events []TraceEvent
		fixture.engine.trace = TraceSinkFunc(func(event TraceEvent) { events = append(events, event) })

		settlement, cut, err := fixture.transaction.Commit(context.Background())
		if err != nil || cut != outputsession.MutationStable || settlement.Kind() != transfer.FilePublished {
			t.Fatalf("metadata reconciliation cut=%v settlement=%v err=%v", cut, settlement.Kind(), err)
		}
		event := traceByOperation(t, events, TracePublish)
		if event.Outcome != TraceReconciled || event.Next != checkpointmodel.PhasePublished {
			t.Fatalf("metadata reconciliation trace=%+v", event)
		}
	})

	t.Run("metadata observation failure stops before publishing", func(t *testing.T) {
		fixture := newRuntimeExecutionFixture(t, 1)
		fixture.writeAndCheckpoint(t, []byte("x"))
		state := fixture.platform.onlyState(t)
		state.mu.Lock()
		state.setErr = errors.New("set-time failed")
		state.matchErr = errors.New("metadata re-observation failed")
		state.mu.Unlock()

		settlement, cut, err := fixture.transaction.Commit(context.Background())
		if err == nil || cut != outputsession.MutationNoChange || settlement.Kind() != 0 {
			t.Fatalf("metadata observation cut=%v settlement=%v err=%v", cut, settlement.Kind(), err)
		}
		if record := fixture.repository.current(t); record.Phase() != checkpointmodel.PhaseActive {
			t.Fatalf("metadata failure phase=%v want active", record.Phase())
		}
		fixture.namespace.mu.Lock()
		publishCalls := fixture.namespace.publishCalls
		fixture.namespace.mu.Unlock()
		if publishCalls != 0 {
			t.Fatalf("metadata observation failure published %d times", publishCalls)
		}
	})

	t.Run("metadata sync failure stops before publishing", func(t *testing.T) {
		fixture := newRuntimeExecutionFixture(t, 1)
		fixture.writeAndCheckpoint(t, []byte("x"))
		state := fixture.platform.onlyState(t)
		state.mu.Lock()
		state.syncErr = errors.New("metadata durability sync failed")
		state.mu.Unlock()

		_, cut, err := fixture.transaction.Commit(context.Background())
		if err == nil || cut != outputsession.MutationNoChange {
			t.Fatalf("metadata sync cut=%v err=%v", cut, err)
		}
		if record := fixture.repository.current(t); record.Phase() != checkpointmodel.PhaseActive {
			t.Fatalf("metadata sync failure phase=%v want active", record.Phase())
		}
	})
}

func TestWriteAndCheckpointCutsRetainExactlyRetryableRanges(t *testing.T) {
	t.Run("zero-byte native write is retryable", func(t *testing.T) {
		fixture := newRuntimeExecutionFixture(t, 1)
		state := fixture.platform.onlyState(t)
		state.mu.Lock()
		state.writeN = 0
		state.writeErr = errors.New("write stopped before the first byte")
		state.mu.Unlock()

		cut, err := fixture.transaction.WriteRange(context.Background(), 0, []byte("x"))
		if err == nil || cut != outputsession.MutationNoChange || !fixture.transaction.pending.IsEmpty() {
			t.Fatalf("zero-byte write cut=%v pending=%v err=%v", cut, fixture.transaction.pending.Ranges(), err)
		}
		if cut, err = fixture.transaction.WriteRange(context.Background(), 0, []byte("x")); err != nil || cut != outputsession.MutationStable {
			t.Fatalf("zero-byte retry cut=%v err=%v", cut, err)
		}
	})

	t.Run("full write with diagnostic is reconciled", func(t *testing.T) {
		fixture := newRuntimeExecutionFixture(t, 1)
		state := fixture.platform.onlyState(t)
		state.mu.Lock()
		state.writeErr = errors.New("write diagnostic after full count")
		state.mu.Unlock()
		var events []TraceEvent
		fixture.engine.trace = TraceSinkFunc(func(event TraceEvent) { events = append(events, event) })

		cut, err := fixture.transaction.WriteRange(context.Background(), 0, []byte("x"))
		if err != nil || cut != outputsession.MutationStable || fixture.transaction.pending.IsEmpty() {
			t.Fatalf("reconciled write cut=%v pending=%v err=%v", cut, fixture.transaction.pending.Ranges(), err)
		}
		event := traceByOperation(t, events, TraceWriteRange)
		if event.Outcome != TraceReconciled {
			t.Fatalf("reconciled write trace=%+v", event)
		}
	})

	t.Run("checkpoint sync failure preserves pending ranges", func(t *testing.T) {
		fixture := newRuntimeExecutionFixture(t, 1)
		if _, err := fixture.transaction.WriteRange(context.Background(), 0, []byte("x")); err != nil {
			t.Fatal(err)
		}
		state := fixture.platform.onlyState(t)
		state.mu.Lock()
		state.syncErr = errors.New("stage sync failed before checkpoint replacement")
		state.mu.Unlock()

		if _, cut, err := fixture.transaction.Checkpoint(context.Background()); err == nil || cut != outputsession.MutationNoChange || fixture.transaction.pending.IsEmpty() {
			t.Fatalf("checkpoint sync cut=%v pending=%v err=%v", cut, fixture.transaction.pending.Ranges(), err)
		}
		state.mu.Lock()
		state.syncErr = nil
		state.mu.Unlock()
		durable, cut, err := fixture.transaction.Checkpoint(context.Background())
		if err != nil || cut != outputsession.MutationStable ||
			!transfer.RangesCoverFile(1, durable.Ranges()) {
			t.Fatalf("checkpoint retry cut=%v ranges=%v err=%v", cut, durable.Ranges().Ranges(), err)
		}
	})
}

func TestIncompletePublicationRemainsActiveAndRetryable(t *testing.T) {
	fixture := newRuntimeExecutionFixture(t, 2)
	if _, err := fixture.transaction.WriteRange(context.Background(), 0, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}

	settlement, cut, err := fixture.transaction.Commit(context.Background())
	if !errors.Is(err, ErrIncompleteFile) || cut != outputsession.MutationNoChange || settlement.Kind() != 0 {
		t.Fatalf("incomplete commit cut=%v settlement=%v err=%v", cut, settlement.Kind(), err)
	}
	if record := fixture.repository.current(t); record.Phase() != checkpointmodel.PhaseActive {
		t.Fatalf("incomplete commit phase=%v want active", record.Phase())
	}
	fixture.namespace.mu.Lock()
	publishCalls := fixture.namespace.publishCalls
	fixture.namespace.mu.Unlock()
	if publishCalls != 0 {
		t.Fatalf("incomplete file reached publication %d times", publishCalls)
	}

	if _, err := fixture.transaction.WriteRange(context.Background(), 1, []byte("y")); err != nil {
		t.Fatal(err)
	}
	settlement, cut, err = fixture.transaction.Commit(context.Background())
	if err != nil || cut != outputsession.MutationStable || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("completed retry cut=%v settlement=%v err=%v", cut, settlement.Kind(), err)
	}
}

func TestPreexistingFinalStateSettlesWithoutNoReplaceMutation(t *testing.T) {
	tests := []struct {
		name             string
		condition        FinalCondition
		settlement       transfer.FileSettlementKind
		phase            checkpointmodel.Phase
		quarantineReason checkpointmodel.QuarantineReason
	}{
		{
			name: "collision", condition: FinalCollision,
			settlement: transfer.FilePublishBlocked, phase: checkpointmodel.PhasePaused,
		},
		{
			name: "unsafe", condition: FinalUnsafe,
			settlement: transfer.FileQuarantined, phase: checkpointmodel.PhaseQuarantined,
			quarantineReason: checkpointmodel.QuarantineFinalUnsafe,
		},
		{
			name: "owned final history", condition: FinalOwnedExact,
			settlement: transfer.FileQuarantined, phase: checkpointmodel.PhaseQuarantined,
			quarantineReason: checkpointmodel.QuarantinePublicationHistory,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRuntimeExecutionFixture(t, 1)
			fixture.writeAndCheckpoint(t, []byte("x"))
			fixture.namespace.mu.Lock()
			fixture.namespace.condition = test.condition
			fixture.namespace.mu.Unlock()

			settlement, cut, err := fixture.transaction.Commit(context.Background())
			if err != nil || cut != outputsession.MutationStable || settlement.Kind() != test.settlement {
				t.Fatalf("preexisting final cut=%v settlement=%v err=%v", cut, settlement.Kind(), err)
			}
			record := fixture.repository.current(t)
			if record.Phase() != test.phase ||
				test.quarantineReason != 0 && record.QuarantineReason() != test.quarantineReason {
				t.Fatalf("preexisting final phase=%v reason=%v", record.Phase(), record.QuarantineReason())
			}
			fixture.namespace.mu.Lock()
			publishCalls := fixture.namespace.publishCalls
			fixture.namespace.mu.Unlock()
			if publishCalls != 0 {
				t.Fatalf("known final state invoked no-replace %d times", publishCalls)
			}
		})
	}
}

func TestMetadataMismatchAndInvalidPublishObservationPreserveEvidence(t *testing.T) {
	t.Run("metadata mismatch is retryable before publishing", func(t *testing.T) {
		fixture := newRuntimeExecutionFixture(t, 1)
		fixture.writeAndCheckpoint(t, []byte("x"))
		unexpected, err := catalog.NewModifiedTime(1, 0, catalog.TimePrecisionSeconds)
		if err != nil {
			t.Fatal(err)
		}
		state := fixture.platform.onlyState(t)
		state.mu.Lock()
		state.modified = unexpected
		state.setErr = errors.New("set-time failed before applying expected value")
		state.mu.Unlock()

		_, cut, err := fixture.transaction.Commit(context.Background())
		if err == nil || cut != outputsession.MutationNoChange {
			t.Fatalf("metadata mismatch cut=%v err=%v", cut, err)
		}
		state.mu.Lock()
		state.setErr = nil
		state.mu.Unlock()
		settlement, cut, err := fixture.transaction.Commit(context.Background())
		if err != nil || cut != outputsession.MutationStable || settlement.Kind() != transfer.FilePublished {
			t.Fatalf("metadata retry cut=%v settlement=%v err=%v", cut, settlement.Kind(), err)
		}
	})

	t.Run("invalid post-publish observation is ambiguous", func(t *testing.T) {
		fixture := newRuntimeExecutionFixture(t, 1)
		fixture.writeAndCheckpoint(t, []byte("x"))
		fixture.namespace.mu.Lock()
		fixture.namespace.publishObservation = FinalCondition(255)
		fixture.namespace.mu.Unlock()

		settlement, cut, err := fixture.transaction.Commit(context.Background())
		if err == nil || cut != outputsession.MutationAmbiguous || settlement.Kind() != 0 {
			t.Fatalf("invalid publish observation cut=%v settlement=%v err=%v", cut, settlement.Kind(), err)
		}
		if record := fixture.repository.current(t); record.Phase() != checkpointmodel.PhasePublishing {
			t.Fatalf("invalid publish observation phase=%v want publishing", record.Phase())
		}
	})
}

func TestPublicationTransitionsRetainTheStrongestDurableEvidence(t *testing.T) {
	t.Run("publish metadata mismatch quarantines", func(t *testing.T) {
		fixture := newRuntimeExecutionFixture(t, 1)
		fixture.writeAndCheckpoint(t, []byte("x"))
		fixture.namespace.mu.Lock()
		fixture.namespace.publishObservation = FinalOwnedMetadataMismatch
		fixture.namespace.mu.Unlock()

		settlement, cut, err := fixture.transaction.Commit(context.Background())
		if err != nil || cut != outputsession.MutationStable || settlement.Kind() != transfer.FileQuarantined {
			t.Fatalf("metadata mismatch cut=%v settlement=%v err=%v", cut, settlement.Kind(), err)
		}
		record := fixture.repository.current(t)
		if record.Phase() != checkpointmodel.PhaseQuarantined ||
			record.QuarantineReason() != checkpointmodel.QuarantineMetadataMismatch {
			t.Fatalf("metadata mismatch phase=%v reason=%v", record.Phase(), record.QuarantineReason())
		}
		state := fixture.platform.onlyState(t)
		state.mu.Lock()
		condition := state.condition
		state.mu.Unlock()
		if condition != OwnedReady {
			t.Fatalf("quarantine discarded stage evidence: %v", condition)
		}
	})

	t.Run("published checkpoint unchanged after public mutation", func(t *testing.T) {
		fixture := newRuntimeExecutionFixture(t, 1)
		fixture.writeAndCheckpoint(t, []byte("x"))
		fixture.repository.hooks = append(fixture.repository.hooks,
			installObservedCheckpoint,
			func(
				repository *fakeCheckpointRepository,
				_ *checkpointmodel.Record,
				_ checkpointmodel.Record,
			) (CheckpointObservation, error) {
				observation, _ := ObservedCheckpoint(repository.record)
				return observation, errors.New("published checkpoint replacement stopped")
			},
		)

		settlement, cut, err := fixture.transaction.Commit(context.Background())
		if err == nil || cut != outputsession.MutationAmbiguous || settlement.Kind() != 0 {
			t.Fatalf("post-publication checkpoint cut=%v settlement=%v err=%v", cut, settlement.Kind(), err)
		}
		record := fixture.repository.current(t)
		if record.Phase() != checkpointmodel.PhasePublishing {
			t.Fatalf("post-publication evidence phase=%v want publishing", record.Phase())
		}
		fixture.namespace.mu.Lock()
		condition := fixture.namespace.condition
		fixture.namespace.mu.Unlock()
		if condition != FinalOwnedExact {
			t.Fatalf("public mutation was not retained as exact evidence: %v", condition)
		}
	})

	t.Run("cleanup failure retains published checkpoint", func(t *testing.T) {
		fixture := newRuntimeExecutionFixture(t, 1)
		fixture.writeAndCheckpoint(t, []byte("x"))
		fixture.platform.retirementErr[RetirementRemoveStage] = errors.New("stage cleanup failed")

		settlement, cut, err := fixture.transaction.Commit(context.Background())
		if err == nil || cut != outputsession.MutationAmbiguous || settlement.Kind() != 0 {
			t.Fatalf("published cleanup cut=%v settlement=%v err=%v", cut, settlement.Kind(), err)
		}
		if record := fixture.repository.current(t); record.Phase() != checkpointmodel.PhasePublished {
			t.Fatalf("cleanup failure phase=%v want published", record.Phase())
		}
		state := fixture.platform.onlyState(t)
		state.mu.Lock()
		condition := state.condition
		state.mu.Unlock()
		if condition != OwnedReady {
			t.Fatalf("failed cleanup changed owned condition to %v", condition)
		}
	})

	t.Run("destination close failure follows durable cleanup", func(t *testing.T) {
		fixture := newRuntimeExecutionFixture(t, 1)
		fixture.writeAndCheckpoint(t, []byte("x"))
		fixture.transaction.destination = &closeFailDestination{
			FileDestination: fixture.transaction.destination,
			err:             errors.New("destination capability close failed"),
		}

		settlement, cut, err := fixture.transaction.Commit(context.Background())
		if err == nil || cut != outputsession.MutationAmbiguous || settlement.Kind() != 0 {
			t.Fatalf("destination close cut=%v settlement=%v err=%v", cut, settlement.Kind(), err)
		}
		if record := fixture.repository.current(t); record.Phase() != checkpointmodel.PhasePublished {
			t.Fatalf("destination close phase=%v want published", record.Phase())
		}
		state := fixture.platform.onlyState(t)
		state.mu.Lock()
		condition := state.condition
		state.mu.Unlock()
		if condition != OwnedStageMissing {
			t.Fatalf("published identity witness was not retained after stage cleanup: %v", condition)
		}
	})

	t.Run("owned file close failure retains published checkpoint and stage", func(t *testing.T) {
		fixture := newRuntimeExecutionFixture(t, 1)
		fixture.writeAndCheckpoint(t, []byte("x"))
		fixture.transaction.file = &closeFailOwnedFile{
			OwnedFile: fixture.transaction.file,
			err:       errors.New("owned handle close failed after publication"),
		}
		fixture.transaction.destination = &unwrapPublishDestination{
			FileDestination: fixture.transaction.destination,
		}

		settlement, cut, err := fixture.transaction.Commit(context.Background())
		if err == nil || cut != outputsession.MutationAmbiguous || settlement.Kind() != 0 {
			t.Fatalf("published file close cut=%v settlement=%v err=%v", cut, settlement.Kind(), err)
		}
		if record := fixture.repository.current(t); record.Phase() != checkpointmodel.PhasePublished {
			t.Fatalf("published file close phase=%v want published", record.Phase())
		}
		state := fixture.platform.onlyState(t)
		state.mu.Lock()
		condition := state.condition
		state.mu.Unlock()
		if condition != OwnedReady || len(fixture.platform.retirement) != 0 {
			t.Fatalf("file close failure condition=%v retirement=%v", condition, fixture.platform.retirement)
		}
	})
}
