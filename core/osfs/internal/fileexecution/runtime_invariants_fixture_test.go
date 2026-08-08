package fileexecution

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
)

type runtimeExecutionFixture struct {
	identity    engineFixture
	namespace   *fakePublicNamespace
	directories *fakeDirectoryAuthority
	platform    *fakePlatform
	repository  *fakeCheckpointRepository
	engine      *Engine
	transaction *Transaction
}

func newRuntimeExecutionFixture(t *testing.T, exactSize uint64) runtimeExecutionFixture {
	t.Helper()
	identity := newEngineFixture(t, exactSize)
	namespace := newFakePublicNamespace()
	directories := &fakeDirectoryAuthority{namespace: namespace}
	platform := newFakePlatform()
	repository := &fakeCheckpointRepository{}
	engine := newTestEngine(t, identity, directories, platform, repository)
	return runtimeExecutionFixture{
		identity: identity, namespace: namespace, directories: directories,
		platform: platform, repository: repository, engine: engine,
		transaction: beginTransaction(t, engine, identity.claim),
	}
}

func (fixture runtimeExecutionFixture) writeAndCheckpoint(t *testing.T, data []byte) {
	t.Helper()
	if cut, err := fixture.transaction.WriteRange(context.Background(), 0, data); err != nil ||
		cut != outputsession.MutationStable {
		t.Fatalf("write: cut=%v err=%v", cut, err)
	}
	if _, cut, err := fixture.transaction.Checkpoint(context.Background()); err != nil ||
		cut != outputsession.MutationStable {
		t.Fatalf("checkpoint: cut=%v err=%v", cut, err)
	}
}

func newRuntimeEngine(
	t *testing.T,
	fixture engineFixture,
	directories DirectoryAuthority,
	platform Platform,
	repository CheckpointRepository,
) *Engine {
	t.Helper()
	engine, err := New(Config{
		Intent: fixture.intent, Ownership: fixture.ownership, SessionID: fixture.sessionID,
		Directories: directories, Platform: platform, Checkpoints: repository,
		Random: deterministicRandom(0x70),
	})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func installObservedCheckpoint(
	repository *fakeCheckpointRepository,
	_ *checkpointmodel.Record,
	next checkpointmodel.Record,
) (CheckpointObservation, error) {
	repository.present = true
	repository.record = next
	observation, _ := ObservedCheckpoint(next)
	return observation, nil
}

type syncFailCreatePlatform struct {
	*fakePlatform
	err error
}

func (platform *syncFailCreatePlatform) CreateOwnedFile(
	ctx context.Context,
	object checkpointmodel.ObjectID,
	exactSize uint64,
) (OwnedFile, OwnedObservation, error) {
	file, observation, err := platform.fakePlatform.CreateOwnedFile(ctx, object, exactSize)
	platform.mu.Lock()
	state := platform.objects[object]
	platform.mu.Unlock()
	if state != nil {
		state.mu.Lock()
		state.syncErr = platform.err
		state.mu.Unlock()
	}
	return file, observation, err
}

type closeFailDestination struct {
	FileDestination
	err error
}

func (destination *closeFailDestination) Close() error {
	underlyingErr := destination.FileDestination.Close()
	return errors.Join(underlyingErr, destination.err)
}

type closeFailOwnedFile struct {
	OwnedFile
	err error
}

func (file *closeFailOwnedFile) Close() error {
	underlyingErr := file.OwnedFile.Close()
	return errors.Join(underlyingErr, file.err)
}

type directoryAuthorityFunc func(context.Context, outputsession.FileClaim) (FileDestination, error)

func (function directoryAuthorityFunc) BindFile(
	ctx context.Context,
	claim outputsession.FileClaim,
) (FileDestination, error) {
	return function(ctx, claim)
}

type presenceOverrideDestination struct {
	FileDestination
	observation FinalObservation
	err         error
}

func (destination *presenceOverrideDestination) ObserveFinalPresence(
	context.Context,
) (FinalObservation, error) {
	return destination.observation, destination.err
}

type createOverridePlatform struct {
	*fakePlatform
	create func(context.Context, checkpointmodel.ObjectID, uint64) (OwnedFile, OwnedObservation, error)
}

func (platform *createOverridePlatform) CreateOwnedFile(
	ctx context.Context,
	object checkpointmodel.ObjectID,
	exactSize uint64,
) (OwnedFile, OwnedObservation, error) {
	return platform.create(ctx, object, exactSize)
}

type unwrapPublishDestination struct {
	FileDestination
}

func (destination *unwrapPublishDestination) PublishNoReplace(
	ctx context.Context,
	file OwnedFile,
	expectation FinalExpectation,
) (FinalObservation, error) {
	if wrapper, ok := file.(*closeFailOwnedFile); ok {
		file = wrapper.OwnedFile
	}
	return destination.FileDestination.PublishNoReplace(ctx, file, expectation)
}

func traceByOperation(t *testing.T, events []TraceEvent, operation TraceOperation) TraceEvent {
	t.Helper()
	for index := range slices.Backward(events) {
		if events[index].Operation == operation {
			return events[index]
		}
	}
	t.Fatalf("trace operation %d was not emitted: %+v", operation, events)
	return TraceEvent{}
}
