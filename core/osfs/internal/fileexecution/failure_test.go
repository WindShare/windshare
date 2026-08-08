package fileexecution

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
)

func TestBeginNewPersistsPartialCreationAsQuarantine(t *testing.T) {
	fixture := newEngineFixture(t, 2)
	platform := newFakePlatform()
	platform.createCondition = OwnedAnchorMissing
	platform.createErr = errors.New("anchor install was interrupted")
	repository := &fakeCheckpointRepository{}
	engine := newTestEngine(
		t, fixture, &fakeDirectoryAuthority{namespace: newFakePublicNamespace()}, platform, repository,
	)
	observation, err := engine.BeginFile(context.Background(), fixture.claim)
	if err != nil || observation.Cut != outputsession.MutationStable ||
		observation.Settlement.Kind() != transfer.FileQuarantined {
		t.Fatalf("partial create: cut=%v settlement=%v err=%v", observation.Cut, observation.Settlement.Kind(), err)
	}
	record := repository.current(t)
	if record.Phase() != checkpointmodel.PhaseQuarantined ||
		record.QuarantineReason() != checkpointmodel.QuarantinePartialObjectCreation {
		t.Fatalf("record: phase=%v reason=%v", record.Phase(), record.QuarantineReason())
	}
	terminal, err := engine.BeginFile(context.Background(), fixture.claim)
	if err != nil || terminal.Settlement.Kind() != transfer.FileQuarantined {
		t.Fatalf("quarantine retry: settlement=%v err=%v", terminal.Settlement.Kind(), err)
	}
}

func TestBeginNewRemovesUnclaimedObjectWhenCheckpointCreateDefinitelyFailed(t *testing.T) {
	fixture := newEngineFixture(t, 2)
	platform := newFakePlatform()
	repository := &fakeCheckpointRepository{}
	repository.hooks = append(repository.hooks, func(
		_ *fakeCheckpointRepository,
		_ *checkpointmodel.Record,
		_ checkpointmodel.Record,
	) (CheckpointObservation, error) {
		return MissingCheckpoint(), errors.New("checkpoint create stopped before rename")
	})
	observation, err := newTestEngine(
		t, fixture, &fakeDirectoryAuthority{namespace: newFakePublicNamespace()}, platform, repository,
	).BeginFile(context.Background(), fixture.claim)
	if err == nil || observation.Cut != outputsession.MutationNoChange || repository.present {
		t.Fatalf("definite create failure: cut=%v checkpoint=%v err=%v", observation.Cut, repository.present, err)
	}
	state := platform.onlyState(t)
	state.mu.Lock()
	condition := state.condition
	state.mu.Unlock()
	if condition != OwnedAbsent {
		t.Fatalf("unclaimed object condition=%v want absent", condition)
	}
	want := []RetirementStep{
		RetirementRemoveStage,
		RetirementSyncStageNamespace,
		RetirementRemoveAnchor,
		RetirementSyncAnchorNamespace,
	}
	platform.mu.Lock()
	if !reflect.DeepEqual(platform.retirement, want) {
		t.Fatalf("cleanup steps=%v want %v", platform.retirement, want)
	}
	platform.mu.Unlock()
}

func TestBeginNewRetainsEvidenceWhenCheckpointCreateIsAmbiguous(t *testing.T) {
	fixture := newEngineFixture(t, 2)
	platform := newFakePlatform()
	repository := &fakeCheckpointRepository{}
	repository.hooks = append(repository.hooks, func(
		_ *fakeCheckpointRepository,
		_ *checkpointmodel.Record,
		_ checkpointmodel.Record,
	) (CheckpointObservation, error) {
		return CheckpointObservation{present: true}, errors.New("checkpoint target unreadable")
	})
	observation, err := newTestEngine(
		t, fixture, &fakeDirectoryAuthority{namespace: newFakePublicNamespace()}, platform, repository,
	).BeginFile(context.Background(), fixture.claim)
	if err == nil || observation.Cut != outputsession.MutationAmbiguous {
		t.Fatalf("ambiguous create: cut=%v err=%v", observation.Cut, err)
	}
	state := platform.onlyState(t)
	state.mu.Lock()
	condition := state.condition
	state.mu.Unlock()
	if condition != OwnedReady || len(platform.retirement) != 0 {
		t.Fatalf("ambiguous evidence was destroyed: condition=%v retirement=%v", condition, platform.retirement)
	}
}

func TestBeginNewCleansItsObjectAfterLosingExactCheckpointCreateRace(t *testing.T) {
	fixture := newEngineFixture(t, 2)
	platform := newFakePlatform()
	repository := &fakeCheckpointRepository{}
	repository.hooks = append(repository.hooks, func(
		repository *fakeCheckpointRepository,
		_ *checkpointmodel.Record,
		next checkpointmodel.Record,
	) (CheckpointObservation, error) {
		foreignObject, err := checkpointmodel.ObjectIDFromBytes(
			bytes.Repeat([]byte{0x29}, transfer.OutputObjectIdentityBytes),
		)
		if err != nil {
			panic(err)
		}
		foreign, err := checkpointmodel.NewRecord(checkpointmodel.RecordSpec{
			TransferIntentDigest: next.TransferIntentDigest(),
			FileID:               next.FileID(),
			FileRevision:         next.FileRevision(),
			CanonicalPath:        next.CanonicalPath(),
			ExactSize:            next.ExactSize(),
			BackendID:            string(next.BackendID()),
			RootIdentity:         next.RootIdentity().Bytes(),
			OwnedOutputObject:    foreignObject.Bytes(),
			StateGeneration:      next.StateGeneration(),
			CheckpointGeneration: next.CheckpointGeneration(),
			VerifiedRanges:       next.VerifiedRanges(),
			Phase:                next.Phase(),
			CommitState:          next.CommitState(),
		})
		if err != nil {
			panic(err)
		}
		repository.present = true
		repository.record = foreign
		observation, _ := ObservedCheckpoint(foreign)
		return observation, errors.New("another exact create won")
	})
	observation, err := newTestEngine(
		t, fixture, &fakeDirectoryAuthority{namespace: newFakePublicNamespace()}, platform, repository,
	).BeginFile(context.Background(), fixture.claim)
	if err == nil || observation.Cut != outputsession.MutationNoChange {
		t.Fatalf("create race: cut=%v err=%v", observation.Cut, err)
	}
	state := platform.onlyState(t)
	state.mu.Lock()
	condition := state.condition
	state.mu.Unlock()
	if condition != OwnedAbsent || !repository.present {
		t.Fatalf("losing object was not retired: condition=%v checkpoint=%v", condition, repository.present)
	}
}

func TestBeginNewReturnsCollisionBeforeAllocatingPrivateState(t *testing.T) {
	fixture := newEngineFixture(t, 1)
	namespace := newFakePublicNamespace()
	namespace.condition = FinalCollision
	platform := newFakePlatform()
	repository := &fakeCheckpointRepository{}
	observation, err := newTestEngine(
		t, fixture, &fakeDirectoryAuthority{namespace: namespace}, platform, repository,
	).BeginFile(context.Background(), fixture.claim)
	if err != nil || observation.Cut != outputsession.MutationStable ||
		observation.Settlement.Kind() != transfer.FileCollision {
		t.Fatalf("collision: cut=%v settlement=%v err=%v", observation.Cut, observation.Settlement.Kind(), err)
	}
	if len(platform.objects) != 0 || repository.present {
		t.Fatalf("collision allocated state: objects=%d checkpoint=%v", len(platform.objects), repository.present)
	}
}

func TestBeginNewBoundsObjectCollisionRetries(t *testing.T) {
	fixture := newEngineFixture(t, 1)
	platform := newFakePlatform()
	raw := bytes.Repeat([]byte{0x62}, transfer.OutputObjectIdentityBytes)
	object, err := checkpointmodel.ObjectIDFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	platform.objects[object] = &fakeOwnedState{
		object: object, condition: OwnedReady, data: []byte{0}, writeN: -1,
	}
	repository := &fakeCheckpointRepository{}
	engine := newTestEngine(
		t, fixture, &fakeDirectoryAuthority{namespace: newFakePublicNamespace()}, platform, repository,
	)
	engine.random = bytes.NewReader(bytes.Repeat(raw, MaximumObjectAllocationAttempts))
	observation, err := engine.BeginFile(context.Background(), fixture.claim)
	if err == nil || observation.Cut != outputsession.MutationNoChange || !errors.Is(err, ErrObjectAllocation) {
		t.Fatalf("allocation exhaustion: cut=%v err=%v", observation.Cut, err)
	}
	if len(platform.objects) != 1 || repository.present {
		t.Fatalf("allocation exhaustion mutated state: objects=%d checkpoint=%v", len(platform.objects), repository.present)
	}
}

func TestRecoveryRetriesPublishingAndReturnsPausedCollision(t *testing.T) {
	t.Run("publishing", func(t *testing.T) {
		fixture := newReducerFixture(t, 3)
		record, err := publishingRecord(fixture.full)
		if err != nil {
			t.Fatal(err)
		}
		repository := &fakeCheckpointRepository{present: true, record: record}
		platform := newFakePlatform()
		platform.objects[fixture.object] = &fakeOwnedState{
			object: fixture.object, condition: OwnedReady, data: []byte("abc"), writeN: -1,
		}
		namespace := newFakePublicNamespace()
		observation, err := newTestEngine(
			t, fixture.engineFixture, &fakeDirectoryAuthority{namespace: namespace}, platform, repository,
		).BeginFile(context.Background(), fixture.claim)
		if err != nil || observation.Cut != outputsession.MutationStable ||
			observation.Settlement.Kind() != transfer.FilePublished {
			t.Fatalf("publishing retry: cut=%v settlement=%v err=%v", observation.Cut, observation.Settlement.Kind(), err)
		}
	})

	t.Run("paused collision", func(t *testing.T) {
		fixture := newReducerFixture(t, 3)
		record, err := pauseRecord(fixture.full)
		if err != nil {
			t.Fatal(err)
		}
		repository := &fakeCheckpointRepository{present: true, record: record}
		platform := newFakePlatform()
		platform.objects[fixture.object] = &fakeOwnedState{
			object: fixture.object, condition: OwnedReady, data: []byte("abc"), writeN: -1,
		}
		namespace := newFakePublicNamespace()
		namespace.condition = FinalCollision
		observation, err := newTestEngine(
			t, fixture.engineFixture, &fakeDirectoryAuthority{namespace: namespace}, platform, repository,
		).BeginFile(context.Background(), fixture.claim)
		if err != nil || observation.Cut != outputsession.MutationStable ||
			observation.Settlement.Kind() != transfer.FilePublishBlocked {
			t.Fatalf("publish-blocked retry: cut=%v settlement=%v err=%v", observation.Cut, observation.Settlement.Kind(), err)
		}
	})
}

func TestRecoveryPromotesInitialCandidateOnlyAfterOwnedObjectSync(t *testing.T) {
	fixture := newReducerFixture(t, 2)
	key, err := fixture.engine.checkpointKey(fixture.claim)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := newInitialRecord(key, fixture.object)
	if err != nil {
		t.Fatal(err)
	}
	repository := &fakeCheckpointRepository{present: true, record: candidate}
	platform := newFakePlatform()
	platform.objects[fixture.object] = &fakeOwnedState{
		object: fixture.object, condition: OwnedReady, data: make([]byte, 2), writeN: -1,
	}
	observation, err := newTestEngine(
		t, fixture.engineFixture,
		&fakeDirectoryAuthority{namespace: newFakePublicNamespace()},
		platform, repository,
	).BeginFile(context.Background(), fixture.claim)
	if err != nil || observation.Cut != outputsession.MutationStable || observation.Transaction == nil {
		t.Fatalf("candidate recovery: cut=%v transaction=%T err=%v", observation.Cut, observation.Transaction, err)
	}
	if record := repository.current(t); record.CommitState() != checkpointmodel.CommitVerified ||
		record.CheckpointGeneration() != 0 {
		t.Fatalf("candidate was not promoted canonically: commit=%v generation=%d", record.CommitState(), record.CheckpointGeneration())
	}
}

func TestRecoveryQuarantinesBrokenInitialCandidateThroughCanonicalReducer(t *testing.T) {
	fixture := newReducerFixture(t, 2)
	key, err := fixture.engine.checkpointKey(fixture.claim)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := newInitialRecord(key, fixture.object)
	if err != nil {
		t.Fatal(err)
	}
	repository := &fakeCheckpointRepository{present: true, record: candidate}
	platform := newFakePlatform()
	platform.objects[fixture.object] = &fakeOwnedState{
		object: fixture.object, condition: OwnedStageMissing, data: make([]byte, 2), writeN: -1,
	}
	observation, err := newTestEngine(
		t, fixture.engineFixture,
		&fakeDirectoryAuthority{namespace: newFakePublicNamespace()},
		platform, repository,
	).BeginFile(context.Background(), fixture.claim)
	if err != nil || observation.Cut != outputsession.MutationStable ||
		observation.Settlement.Kind() != transfer.FileQuarantined {
		t.Fatalf("candidate quarantine: cut=%v settlement=%v err=%v", observation.Cut, observation.Settlement.Kind(), err)
	}
	record := repository.current(t)
	if record.QuarantineReason() != checkpointmodel.QuarantineStageMissing ||
		record.QuarantineOrigin() != checkpointmodel.QuarantineOriginWitnessed {
		t.Fatalf("candidate quarantine reason=%v origin=%v", record.QuarantineReason(), record.QuarantineOrigin())
	}
}

func TestCleanupOfObservedAbsenceClosesBothDirectoryDurabilityCuts(t *testing.T) {
	fixture := newReducerFixture(t, 1)
	platform := newFakePlatform()
	platform.objects[fixture.object] = &fakeOwnedState{
		object: fixture.object, condition: OwnedAbsent, data: []byte{0}, writeN: -1,
	}
	engine := newTestEngine(
		t, fixture.engineFixture,
		&fakeDirectoryAuthority{namespace: newFakePublicNamespace()},
		platform, &fakeCheckpointRepository{},
	)
	if err := engine.cleanupOwned(context.Background(), fixture.object, OwnedAbsent); err != nil {
		t.Fatal(err)
	}
	want := []RetirementStep{
		RetirementSyncStageNamespace,
		RetirementRemoveAnchor,
		RetirementSyncAnchorNamespace,
	}
	platform.mu.Lock()
	if !reflect.DeepEqual(platform.retirement, want) {
		t.Fatalf("absence cleanup=%v want %v", platform.retirement, want)
	}
	platform.mu.Unlock()
}

func TestTraceSinkRunsAfterTransactionLockIsReleased(t *testing.T) {
	fixture := newEngineFixture(t, 1)
	engine := newTestEngine(
		t, fixture,
		&fakeDirectoryAuthority{namespace: newFakePublicNamespace()},
		newFakePlatform(), &fakeCheckpointRepository{},
	)
	var transaction *Transaction
	var observedPhase checkpointmodel.Phase
	events := make([]TraceEvent, 0, 2)
	engine.trace = TraceSinkFunc(func(event TraceEvent) {
		events = append(events, event)
		if transaction != nil {
			transaction.mu.Lock()
			observedPhase = transaction.record.Phase()
			transaction.mu.Unlock()
		}
	})
	transaction = beginTransaction(t, engine, fixture.claim)
	if transaction.Binding().Target() != fixture.file.Target {
		t.Fatal("transaction binding changed target")
	}
	if _, err := transaction.WriteRange(context.Background(), 0, nil); err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 || events[len(events)-1].Operation != TraceWriteRange {
		t.Fatalf("trace events=%v", events)
	}
	if observedPhase != checkpointmodel.PhaseActive {
		t.Fatalf("trace observed transaction phase=%v want active", observedPhase)
	}
}
