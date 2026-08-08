package fileexecution

import (
	"context"
	"errors"
	"io"
	"reflect"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
)

func TestFileExecutionCreatesCheckpointsReopensAndPublishes(t *testing.T) {
	fixture := newEngineFixture(t, 6)
	namespace := newFakePublicNamespace()
	directories := &fakeDirectoryAuthority{namespace: namespace}
	platform := newFakePlatform()
	repository := &fakeCheckpointRepository{}
	engine := newTestEngine(t, fixture, directories, platform, repository)

	transaction := beginTransaction(t, engine, fixture.claim)
	directories.mu.Lock()
	if len(directories.claims) != 1 || directories.claims[0].ID() != fixture.claim.ID() ||
		directories.claims[0].File().Target != fixture.file.Target {
		t.Fatalf("directory authority did not receive the atomic claim: %#v", directories.claims)
	}
	directories.mu.Unlock()
	if cut, err := transaction.WriteRange(context.Background(), 3, []byte("def")); err != nil ||
		cut != outputsession.MutationStable {
		t.Fatalf("write tail: cut=%v err=%v", cut, err)
	}
	if cut, err := transaction.WriteRange(context.Background(), 0, []byte("abc")); err != nil ||
		cut != outputsession.MutationStable {
		t.Fatalf("write head: cut=%v err=%v", cut, err)
	}
	if cut, err := transaction.WriteRange(context.Background(), 1, []byte("x")); !errors.Is(err, ErrRangeOverlap) || cut != outputsession.MutationNoChange {
		t.Fatalf("overlap: cut=%v err=%v", cut, err)
	}
	durable, cut, err := transaction.Checkpoint(context.Background())
	if err != nil || cut != outputsession.MutationStable ||
		!transfer.RangesCoverFile(fixture.file.ExpectedSize, durable.Ranges()) {
		t.Fatalf("checkpoint: cut=%v ranges=%v err=%v", cut, durable.Ranges().Ranges(), err)
	}
	settlement, cut, err := transaction.Pause(context.Background(), transfer.FilePauseInterrupted)
	if err != nil || cut != outputsession.MutationStable || settlement.Kind() != transfer.FilePaused {
		t.Fatalf("pause: cut=%v settlement=%v err=%v", cut, settlement.Kind(), err)
	}
	if record := repository.current(t); record.Phase() != checkpointmodel.PhasePaused ||
		record.CheckpointGeneration() != 1 {
		t.Fatalf("unexpected paused checkpoint: phase=%v generation=%d", record.Phase(), record.CheckpointGeneration())
	}

	reopened := beginTransaction(t, newTestEngine(t, fixture, directories, platform, repository), fixture.claim)
	settlement, cut, err = reopened.Commit(context.Background())
	if err != nil || cut != outputsession.MutationStable || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("commit: cut=%v settlement=%v err=%v", cut, settlement.Kind(), err)
	}
	record := repository.current(t)
	if record.Phase() != checkpointmodel.PhasePublished || record.CommitState() != checkpointmodel.CommitPublished {
		t.Fatalf("unexpected published checkpoint: phase=%v commit=%v", record.Phase(), record.CommitState())
	}
	namespace.mu.Lock()
	if string(namespace.data) != "abcdef" || namespace.publishCalls != 1 || namespace.syncCalls != 1 {
		t.Fatalf("unexpected public state: data=%q publish=%d sync=%d", namespace.data, namespace.publishCalls, namespace.syncCalls)
	}
	namespace.mu.Unlock()
	wantRetirement := []RetirementStep{
		RetirementRemoveStage,
		RetirementSyncStageNamespace,
	}
	platform.mu.Lock()
	if !reflect.DeepEqual(platform.retirement, wantRetirement) {
		t.Fatalf("retirement order=%v want %v", platform.retirement, wantRetirement)
	}
	platform.mu.Unlock()
	state := platform.onlyState(t)
	state.mu.Lock()
	condition := state.condition
	state.mu.Unlock()
	if condition != OwnedStageMissing {
		t.Fatalf("published identity witness condition=%v want %v", condition, OwnedStageMissing)
	}

	terminal, err := engine.BeginFile(context.Background(), fixture.claim)
	if err != nil {
		t.Fatal(err)
	}
	terminalSettlement, ok := terminal.Settlement.VerifiedCheckpoint()
	if terminal.Cut != outputsession.MutationStable || terminal.Settlement.Kind() != transfer.FilePublished || !ok ||
		!transfer.RangesCoverFile(fixture.file.ExpectedSize, terminalSettlement.Ranges()) {
		t.Fatalf("terminal retry did not return the durable publication: %#v", terminal)
	}
	platform.mu.Lock()
	wantRetirement = append(wantRetirement, RetirementSyncStageNamespace)
	if !reflect.DeepEqual(platform.retirement, wantRetirement) {
		t.Fatalf("retry witness reconciliation=%v want %v", platform.retirement, wantRetirement)
	}
	platform.mu.Unlock()
}

func TestPublicationNoReplaceRacePausesWithStageIntact(t *testing.T) {
	fixture := newEngineFixture(t, 3)
	namespace := newFakePublicNamespace()
	namespace.publishObservation = FinalCollision
	directories := &fakeDirectoryAuthority{namespace: namespace}
	platform := newFakePlatform()
	repository := &fakeCheckpointRepository{}
	transaction := beginTransaction(t, newTestEngine(t, fixture, directories, platform, repository), fixture.claim)
	if cut, err := transaction.WriteRange(context.Background(), 0, []byte("new")); err != nil || cut != outputsession.MutationStable {
		t.Fatalf("write: cut=%v err=%v", cut, err)
	}

	settlement, cut, err := transaction.Commit(context.Background())
	if err != nil || cut != outputsession.MutationStable || settlement.Kind() != transfer.FilePublishBlocked {
		t.Fatalf("commit: cut=%v settlement=%v err=%v", cut, settlement.Kind(), err)
	}
	record := repository.current(t)
	if record.Phase() != checkpointmodel.PhasePaused {
		t.Fatalf("phase=%v want paused", record.Phase())
	}
	state := platform.onlyState(t)
	state.mu.Lock()
	condition := state.condition
	state.mu.Unlock()
	if condition != OwnedReady {
		t.Fatalf("owned evidence was retired after collision: %v", condition)
	}
	namespace.mu.Lock()
	defer namespace.mu.Unlock()
	if namespace.condition != FinalAbsent || namespace.publishCalls != 1 || len(namespace.data) != 0 {
		t.Fatalf("no-replace violated: condition=%v calls=%d data=%q", namespace.condition, namespace.publishCalls, namespace.data)
	}
}

func TestCheckpointReconcilesKnownCrashCuts(t *testing.T) {
	fixture := newEngineFixture(t, 6)
	namespace := newFakePublicNamespace()
	directories := &fakeDirectoryAuthority{namespace: namespace}
	platform := newFakePlatform()
	repository := &fakeCheckpointRepository{}
	transaction := beginTransaction(t, newTestEngine(t, fixture, directories, platform, repository), fixture.claim)

	if _, err := transaction.WriteRange(context.Background(), 0, []byte("ab")); err != nil {
		t.Fatal(err)
	}
	repository.hooks = append(repository.hooks, func(
		repository *fakeCheckpointRepository,
		_ *checkpointmodel.Record,
		_ checkpointmodel.Record,
	) (CheckpointObservation, error) {
		observation, _ := ObservedCheckpoint(repository.record)
		return observation, errors.New("install stopped before replacement")
	})
	if _, cut, err := transaction.Checkpoint(context.Background()); err == nil || cut != outputsession.MutationNoChange {
		t.Fatalf("unchanged cut: cut=%v err=%v", cut, err)
	}
	if transaction.pending.IsEmpty() {
		t.Fatal("known-unchanged checkpoint discarded pending ranges")
	}
	if durable, cut, err := transaction.Checkpoint(context.Background()); err != nil ||
		cut != outputsession.MutationStable || durable.CheckpointGeneration() != 1 {
		t.Fatalf("checkpoint retry: generation=%d cut=%v err=%v", durable.CheckpointGeneration(), cut, err)
	}

	if _, err := transaction.WriteRange(context.Background(), 2, []byte("cd")); err != nil {
		t.Fatal(err)
	}
	repository.hooks = append(repository.hooks, func(
		repository *fakeCheckpointRepository,
		_ *checkpointmodel.Record,
		next checkpointmodel.Record,
	) (CheckpointObservation, error) {
		repository.record = next
		repository.present = true
		observation, _ := ObservedCheckpoint(next)
		return observation, errors.New("replacement completed before diagnostic error")
	})
	if durable, cut, err := transaction.Checkpoint(context.Background()); err != nil ||
		cut != outputsession.MutationStable || durable.CheckpointGeneration() != 2 {
		t.Fatalf("installed reconciliation: generation=%d cut=%v err=%v", durable.CheckpointGeneration(), cut, err)
	}

	if _, err := transaction.WriteRange(context.Background(), 4, []byte("ef")); err != nil {
		t.Fatal(err)
	}
	repository.hooks = append(repository.hooks, func(
		repository *fakeCheckpointRepository,
		_ *checkpointmodel.Record,
		_ checkpointmodel.Record,
	) (CheckpointObservation, error) {
		foreign, err := pauseRecord(repository.record)
		if err != nil {
			panic(err)
		}
		observation, _ := ObservedCheckpoint(foreign)
		return observation, errors.New("fresh target was neither previous nor next")
	})
	if _, cut, err := transaction.Checkpoint(context.Background()); err == nil ||
		cut != outputsession.MutationAmbiguous {
		t.Fatalf("ambiguous cut: cut=%v err=%v", cut, err)
	}
	if record := repository.current(t); record.CheckpointGeneration() != 2 {
		t.Fatalf("ambiguous observation changed fake durable record: %d", record.CheckpointGeneration())
	}
}

func TestPartialRangeWriteIsAmbiguousAndNeverCheckpointed(t *testing.T) {
	fixture := newEngineFixture(t, 3)
	platform := newFakePlatform()
	repository := &fakeCheckpointRepository{}
	transaction := beginTransaction(t, newTestEngine(
		t, fixture, &fakeDirectoryAuthority{namespace: newFakePublicNamespace()}, platform, repository,
	), fixture.claim)
	state := platform.onlyState(t)
	state.mu.Lock()
	state.writeN = 1
	state.writeErr = io.ErrShortWrite
	state.mu.Unlock()
	cut, err := transaction.WriteRange(context.Background(), 0, []byte("abc"))
	if err == nil || cut != outputsession.MutationAmbiguous {
		t.Fatalf("partial write: cut=%v err=%v", cut, err)
	}
	if !transaction.pending.IsEmpty() || repository.current(t).CheckpointGeneration() != 0 {
		t.Fatal("partial bytes were promoted into canonical checkpoint state")
	}
}

func TestPublishingCrashRecoversExactPublicObject(t *testing.T) {
	fixture := newEngineFixture(t, 3)
	namespace := newFakePublicNamespace()
	namespace.syncErr = errors.New("parent sync interrupted")
	directories := &fakeDirectoryAuthority{namespace: namespace}
	platform := newFakePlatform()
	repository := &fakeCheckpointRepository{}
	transaction := beginTransaction(t, newTestEngine(t, fixture, directories, platform, repository), fixture.claim)
	if _, err := transaction.WriteRange(context.Background(), 0, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	if settlement, cut, err := transaction.Commit(context.Background()); err == nil ||
		cut != outputsession.MutationAmbiguous || settlement.Kind() != 0 {
		t.Fatalf("parent-sync crash: cut=%v settlement=%v err=%v", cut, settlement.Kind(), err)
	}
	if record := repository.current(t); record.Phase() != checkpointmodel.PhasePublishing {
		t.Fatalf("phase=%v want publishing", record.Phase())
	}
	namespace.mu.Lock()
	if namespace.condition != FinalOwnedExact {
		t.Fatalf("public observation=%v want exact", namespace.condition)
	}
	namespace.syncErr = nil
	namespace.mu.Unlock()

	observation, err := newTestEngine(t, fixture, directories, platform, repository).BeginFile(
		context.Background(), fixture.claim,
	)
	if err != nil || observation.Cut != outputsession.MutationStable ||
		observation.Settlement.Kind() != transfer.FilePublished {
		t.Fatalf("recovery: cut=%v settlement=%v err=%v", observation.Cut, observation.Settlement.Kind(), err)
	}
	if record := repository.current(t); record.Phase() != checkpointmodel.PhasePublished {
		t.Fatalf("phase=%v want published", record.Phase())
	}
}

func TestAbsentPublishErrorRetainsPublishingEvidence(t *testing.T) {
	fixture := newEngineFixture(t, 1)
	namespace := newFakePublicNamespace()
	namespace.publishObservation = FinalAbsent
	namespace.publishErr = errors.New("no-replace syscall failed before installation")
	repository := &fakeCheckpointRepository{}
	transaction := beginTransaction(t, newTestEngine(
		t, fixture, &fakeDirectoryAuthority{namespace: namespace}, newFakePlatform(), repository,
	), fixture.claim)
	if _, err := transaction.WriteRange(context.Background(), 0, []byte("x")); err != nil {
		t.Fatal(err)
	}
	settlement, cut, err := transaction.Commit(context.Background())
	if err == nil || !errors.Is(err, ErrPublicationAmbiguous) || cut != outputsession.MutationAmbiguous ||
		settlement.Kind() != 0 {
		t.Fatalf("publish failure: cut=%v settlement=%v err=%v", cut, settlement.Kind(), err)
	}
	if record := repository.current(t); record.Phase() != checkpointmodel.PhasePublishing {
		t.Fatalf("phase=%v want publishing evidence", record.Phase())
	}
}

func TestRecoveryQuarantinesMissingStage(t *testing.T) {
	fixture := newEngineFixture(t, 2)
	namespace := newFakePublicNamespace()
	directories := &fakeDirectoryAuthority{namespace: namespace}
	platform := newFakePlatform()
	repository := &fakeCheckpointRepository{}
	transaction := beginTransaction(t, newTestEngine(t, fixture, directories, platform, repository), fixture.claim)
	if _, _, err := transaction.Pause(context.Background(), transfer.FilePauseInterrupted); err != nil {
		t.Fatal(err)
	}
	state := platform.onlyState(t)
	state.mu.Lock()
	state.condition = OwnedStageMissing
	state.mu.Unlock()

	observation, err := newTestEngine(t, fixture, directories, platform, repository).BeginFile(
		context.Background(), fixture.claim,
	)
	if err != nil || observation.Cut != outputsession.MutationStable ||
		observation.Settlement.Kind() != transfer.FileQuarantined {
		t.Fatalf("quarantine recovery: cut=%v settlement=%v err=%v", observation.Cut, observation.Settlement.Kind(), err)
	}
	record := repository.current(t)
	if record.Phase() != checkpointmodel.PhaseQuarantined ||
		record.QuarantineReason() != checkpointmodel.QuarantineStageMissing {
		t.Fatalf("quarantine checkpoint: phase=%v reason=%v", record.Phase(), record.QuarantineReason())
	}
}

func TestRetirementPersistsBeforeCleanupAndResumesInOrder(t *testing.T) {
	fixture := newEngineFixture(t, 2)
	platform := newFakePlatform()
	repository := &fakeCheckpointRepository{}
	directories := &fakeDirectoryAuthority{namespace: newFakePublicNamespace()}
	transaction := beginTransaction(t, newTestEngine(t, fixture, directories, platform, repository), fixture.claim)
	if _, cut, err := transaction.Retire(context.Background(), transfer.FileRetireExplicitPolicySkip); !errors.Is(err, ErrRetirementUnauthorized) || cut != outputsession.MutationNoChange {
		t.Fatalf("policy retirement: cut=%v err=%v", cut, err)
	}
	platform.retirementErr[RetirementRemoveStage] = errors.New("remove interrupted")
	if settlement, cut, err := transaction.Retire(
		context.Background(), transfer.FileRetireInvalidatedRevision,
	); err == nil || cut != outputsession.MutationAmbiguous || settlement.Kind() != 0 {
		t.Fatalf("interrupted retirement: cut=%v settlement=%v err=%v", cut, settlement.Kind(), err)
	}
	if record := repository.current(t); record.Phase() != checkpointmodel.PhaseRetired ||
		record.RetirementReason() != checkpointmodel.RetirementInvalidatedRevision {
		t.Fatalf("retirement was not persisted first: phase=%v reason=%v", record.Phase(), record.RetirementReason())
	}
	delete(platform.retirementErr, RetirementRemoveStage)
	observation, err := newTestEngine(t, fixture, directories, platform, repository).BeginFile(
		context.Background(), fixture.claim,
	)
	if err != nil || observation.Cut != outputsession.MutationStable ||
		observation.Settlement.Kind() != transfer.FileRetired {
		t.Fatalf("retirement recovery: cut=%v settlement=%v err=%v", observation.Cut, observation.Settlement.Kind(), err)
	}
	want := []RetirementStep{
		RetirementRemoveStage,
		RetirementRemoveStage,
		RetirementSyncStageNamespace,
		RetirementRemoveAnchor,
		RetirementSyncAnchorNamespace,
	}
	platform.mu.Lock()
	if !reflect.DeepEqual(platform.retirement, want) {
		t.Fatalf("retirement steps=%v want %v", platform.retirement, want)
	}
	platform.mu.Unlock()
}

func TestWrongClaimCapabilityFailsBeforeFileMutation(t *testing.T) {
	fixture := newEngineFixture(t, 1)
	platform := newFakePlatform()
	repository := &fakeCheckpointRepository{}
	observation, err := newTestEngine(
		t, fixture,
		&fakeDirectoryAuthority{namespace: newFakePublicNamespace(), wrongClaim: true},
		platform, repository,
	).BeginFile(context.Background(), fixture.claim)
	if err == nil || observation.Cut != outputsession.MutationNoChange {
		t.Fatalf("wrong capability: cut=%v err=%v", observation.Cut, err)
	}
	platform.mu.Lock()
	objects := len(platform.objects)
	platform.mu.Unlock()
	if objects != 0 || repository.present {
		t.Fatalf("mutation preceded capability validation: objects=%d checkpoint=%v", objects, repository.present)
	}
}

func TestConcurrentDisjointWritesSerializeIntoOneCanonicalCheckpoint(t *testing.T) {
	fixture := newEngineFixture(t, 2)
	transaction := beginTransaction(t, newTestEngine(
		t, fixture,
		&fakeDirectoryAuthority{namespace: newFakePublicNamespace()},
		newFakePlatform(), &fakeCheckpointRepository{},
	), fixture.claim)
	start := make(chan struct{})
	errorsByWrite := make(chan error, 2)
	var writers sync.WaitGroup
	for offset, value := range []byte{'a', 'b'} {
		writers.Add(1)
		go func(offset uint64, value byte) {
			defer writers.Done()
			<-start
			cut, err := transaction.WriteRange(context.Background(), offset, []byte{value})
			if err == nil && cut != outputsession.MutationStable {
				err = errors.New("write did not reach a stable cut")
			}
			errorsByWrite <- err
		}(uint64(offset), value)
	}
	close(start)
	writers.Wait()
	close(errorsByWrite)
	for err := range errorsByWrite {
		if err != nil {
			t.Fatal(err)
		}
	}
	durable, cut, err := transaction.Checkpoint(context.Background())
	if err != nil || cut != outputsession.MutationStable ||
		!transfer.RangesCoverFile(fixture.file.ExpectedSize, durable.Ranges()) {
		t.Fatalf("concurrent checkpoint: cut=%v ranges=%v err=%v", cut, durable.Ranges().Ranges(), err)
	}
}
