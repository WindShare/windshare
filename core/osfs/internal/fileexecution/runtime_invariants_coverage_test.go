package fileexecution

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
)

func TestBeginAuthorityFailuresStopBeforePrivateMutation(t *testing.T) {
	tests := []struct {
		name    string
		claim   func(engineFixture) outputsession.FileClaim
		prepare func(*fakeDirectoryAuthority, *fakeCheckpointRepository)
	}{
		{
			name:  "invalid atomic claim",
			claim: func(engineFixture) outputsession.FileClaim { return outputsession.FileClaim{} },
		},
		{
			name:  "directory binding failure",
			claim: func(fixture engineFixture) outputsession.FileClaim { return fixture.claim },
			prepare: func(directories *fakeDirectoryAuthority, _ *fakeCheckpointRepository) {
				directories.bindErr = errors.New("ancestry capability expired")
			},
		},
		{
			name:  "checkpoint lookup failure",
			claim: func(fixture engineFixture) outputsession.FileClaim { return fixture.claim },
			prepare: func(_ *fakeDirectoryAuthority, repository *fakeCheckpointRepository) {
				repository.lookupErr = errors.New("checkpoint namespace unavailable")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newEngineFixture(t, 1)
			platform := newFakePlatform()
			repository := &fakeCheckpointRepository{}
			directories := &fakeDirectoryAuthority{namespace: newFakePublicNamespace()}
			if test.prepare != nil {
				test.prepare(directories, repository)
			}
			engine := newTestEngine(t, fixture, directories, platform, repository)
			var events []TraceEvent
			engine.trace = TraceSinkFunc(func(event TraceEvent) { events = append(events, event) })

			observation, err := engine.BeginFile(context.Background(), test.claim(fixture))
			if err == nil || observation.Cut != outputsession.MutationNoChange {
				t.Fatalf("begin cut=%v err=%v", observation.Cut, err)
			}
			platform.mu.Lock()
			objects := len(platform.objects)
			platform.mu.Unlock()
			repository.mu.Lock()
			stores := len(repository.stores)
			repository.mu.Unlock()
			if objects != 0 || stores != 0 {
				t.Fatalf("authority failure mutated private state: objects=%d stores=%d", objects, stores)
			}
			event := traceByOperation(t, events, TraceBeginFile)
			if event.Outcome != TraceNoChange || !event.Fault.Valid() {
				t.Fatalf("authority failure trace=%+v", event)
			}
		})
	}
}

func TestNewObjectCreationCutsPreserveOnlyObservableAuthority(t *testing.T) {
	t.Run("entropy failure is no-change", func(t *testing.T) {
		fixture := newEngineFixture(t, 1)
		platform := newFakePlatform()
		repository := &fakeCheckpointRepository{}
		engine := newTestEngine(
			t, fixture, &fakeDirectoryAuthority{namespace: newFakePublicNamespace()}, platform, repository,
		)
		engine.random = &failingReader{err: io.ErrUnexpectedEOF}

		observation, err := engine.BeginFile(context.Background(), fixture.claim)
		if err == nil || observation.Cut != outputsession.MutationNoChange {
			t.Fatalf("entropy failure cut=%v err=%v", observation.Cut, err)
		}
		if len(platform.objects) != 0 || repository.present {
			t.Fatalf("entropy failure allocated state: objects=%d checkpoint=%v", len(platform.objects), repository.present)
		}
	})

	t.Run("proven absent create is no-change", func(t *testing.T) {
		fixture := newEngineFixture(t, 1)
		platform := newFakePlatform()
		platform.createCondition = OwnedAbsent
		platform.createErr = errors.New("create stopped before either name existed")
		repository := &fakeCheckpointRepository{}
		observation, err := newTestEngine(
			t, fixture, &fakeDirectoryAuthority{namespace: newFakePublicNamespace()}, platform, repository,
		).BeginFile(context.Background(), fixture.claim)
		if err == nil || observation.Cut != outputsession.MutationNoChange || repository.present {
			t.Fatalf("absent create cut=%v checkpoint=%v err=%v", observation.Cut, repository.present, err)
		}
		state := platform.onlyState(t)
		state.mu.Lock()
		condition := state.condition
		state.mu.Unlock()
		if condition != OwnedAbsent {
			t.Fatalf("absent create retained condition %v", condition)
		}
	})

	t.Run("ready create diagnostic is reconciled", func(t *testing.T) {
		fixture := newEngineFixture(t, 1)
		platform := newFakePlatform()
		platform.createErr = errors.New("create returned after observing both names")
		repository := &fakeCheckpointRepository{}
		engine := newTestEngine(
			t, fixture, &fakeDirectoryAuthority{namespace: newFakePublicNamespace()}, platform, repository,
		)
		var events []TraceEvent
		engine.trace = TraceSinkFunc(func(event TraceEvent) { events = append(events, event) })

		observation, err := engine.BeginFile(context.Background(), fixture.claim)
		if err != nil || observation.Cut != outputsession.MutationStable || observation.Transaction == nil {
			t.Fatalf("reconciled create cut=%v transaction=%T err=%v", observation.Cut, observation.Transaction, err)
		}
		event := traceByOperation(t, events, TraceCreateOwnedFile)
		if event.Outcome != TraceReconciled || event.Next != checkpointmodel.PhaseActive {
			t.Fatalf("reconciled create trace=%+v", event)
		}
	})
}

func TestBeginNewPortObservationsSelectSafeCuts(t *testing.T) {
	tests := []struct {
		name        string
		observation func(t *testing.T) FinalObservation
		observeErr  error
		closeErr    error
	}{
		{
			name:       "presence observation error",
			observeErr: errors.New("public destination could not be observed"),
		},
		{
			name:        "invalid presence observation",
			observation: func(*testing.T) FinalObservation { return FinalObservation{} },
		},
		{
			name: "collision settlement close failure",
			observation: func(t *testing.T) FinalObservation {
				observation, err := ObserveFinal(FinalCollision)
				if err != nil {
					t.Fatal(err)
				}
				return observation
			},
			closeErr: errors.New("destination close failed"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newEngineFixture(t, 1)
			namespace := newFakePublicNamespace()
			var bound *fakeDestination
			directories := directoryAuthorityFunc(func(
				_ context.Context,
				claim outputsession.FileClaim,
			) (FileDestination, error) {
				bound = &fakeDestination{
					claimID: claim.ID(), target: claim.File().Target, namespace: namespace,
				}
				var destination FileDestination = bound
				if test.closeErr != nil {
					destination = &closeFailDestination{FileDestination: destination, err: test.closeErr}
				}
				observation := FinalObservation{}
				if test.observation != nil {
					observation = test.observation(t)
				}
				return &presenceOverrideDestination{
					FileDestination: destination, observation: observation, err: test.observeErr,
				}, nil
			})
			platform := newFakePlatform()
			repository := &fakeCheckpointRepository{}

			result, err := newRuntimeEngine(t, fixture, directories, platform, repository).BeginFile(
				context.Background(), fixture.claim,
			)
			if err == nil || result.Cut != outputsession.MutationNoChange || result.Settlement.Kind() != 0 {
				t.Fatalf("presence boundary cut=%v settlement=%v err=%v", result.Cut, result.Settlement.Kind(), err)
			}
			if bound == nil || !bound.closed || len(platform.objects) != 0 || repository.present {
				t.Fatalf("presence boundary close=%v objects=%d checkpoint=%v", bound != nil && bound.closed,
					len(platform.objects), repository.present)
			}
		})
	}
}

func TestCreateObservationContradictionsRetainUncertainObject(t *testing.T) {
	tests := []struct {
		name   string
		result func(*testing.T, *fakePlatform, context.Context, checkpointmodel.ObjectID, uint64) (OwnedFile, OwnedObservation, error)
	}{
		{
			name: "invalid observation",
			result: func(
				_ *testing.T,
				base *fakePlatform,
				ctx context.Context,
				object checkpointmodel.ObjectID,
				exactSize uint64,
			) (OwnedFile, OwnedObservation, error) {
				file, _, _ := base.CreateOwnedFile(ctx, object, exactSize)
				return file, OwnedObservation{}, errors.New("create observation was malformed")
			},
		},
		{
			name: "collision with writable handle",
			result: func(
				t *testing.T,
				base *fakePlatform,
				ctx context.Context,
				object checkpointmodel.ObjectID,
				exactSize uint64,
			) (OwnedFile, OwnedObservation, error) {
				file, _, _ := base.CreateOwnedFile(ctx, object, exactSize)
				observation, err := NewOwnedObservation(object, OwnedObjectCollision)
				if err != nil {
					t.Fatal(err)
				}
				return file, observation, nil
			},
		},
		{
			name: "ready without writable handle",
			result: func(
				t *testing.T,
				base *fakePlatform,
				ctx context.Context,
				object checkpointmodel.ObjectID,
				exactSize uint64,
			) (OwnedFile, OwnedObservation, error) {
				_, observation, _ := base.CreateOwnedFile(ctx, object, exactSize)
				if observation.Condition() != OwnedReady {
					t.Fatalf("fixture condition=%v", observation.Condition())
				}
				return nil, observation, nil
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newEngineFixture(t, 1)
			base := newFakePlatform()
			platform := &createOverridePlatform{fakePlatform: base}
			platform.create = func(
				ctx context.Context,
				object checkpointmodel.ObjectID,
				exactSize uint64,
			) (OwnedFile, OwnedObservation, error) {
				return test.result(t, base, ctx, object, exactSize)
			}
			repository := &fakeCheckpointRepository{}

			observation, err := newRuntimeEngine(
				t, fixture, &fakeDirectoryAuthority{namespace: newFakePublicNamespace()}, platform, repository,
			).BeginFile(context.Background(), fixture.claim)
			if err == nil || observation.Cut != outputsession.MutationAmbiguous {
				t.Fatalf("create contradiction cut=%v err=%v", observation.Cut, err)
			}
			if len(base.objects) != 1 || repository.present || len(base.retirement) != 0 {
				t.Fatalf("create contradiction objects=%d checkpoint=%v retirement=%v",
					len(base.objects), repository.present, base.retirement)
			}
		})
	}
}

func TestInitialQuarantineStoreFailureRetainsPartialCreationEvidence(t *testing.T) {
	fixture := newEngineFixture(t, 1)
	platform := newFakePlatform()
	platform.createCondition = OwnedAnchorMissing
	platform.createErr = errors.New("anchor creation was interrupted")
	repository := &fakeCheckpointRepository{}
	repository.hooks = append(repository.hooks, func(
		*fakeCheckpointRepository,
		*checkpointmodel.Record,
		checkpointmodel.Record,
	) (CheckpointObservation, error) {
		return MissingCheckpoint(), errors.New("quarantine checkpoint create failed")
	})

	observation, err := newTestEngine(
		t, fixture, &fakeDirectoryAuthority{namespace: newFakePublicNamespace()}, platform, repository,
	).BeginFile(context.Background(), fixture.claim)
	if err == nil || observation.Cut != outputsession.MutationAmbiguous || repository.present {
		t.Fatalf("quarantine store cut=%v checkpoint=%v err=%v", observation.Cut, repository.present, err)
	}
	state := platform.onlyState(t)
	state.mu.Lock()
	condition := state.condition
	state.mu.Unlock()
	if condition != OwnedAnchorMissing || len(platform.retirement) != 0 {
		t.Fatalf("partial creation evidence condition=%v retirement=%v", condition, platform.retirement)
	}
}

type failingReader struct {
	err error
}

func (reader *failingReader) Read([]byte) (int, error) { return 0, reader.err }

func TestInitialCandidateDurabilityFailuresRetainRecoveryEvidence(t *testing.T) {
	t.Run("owned object sync failure", func(t *testing.T) {
		fixture := newEngineFixture(t, 2)
		basePlatform := newFakePlatform()
		platform := &syncFailCreatePlatform{
			fakePlatform: basePlatform,
			err:          errors.New("stage sync failed after checkpoint create"),
		}
		repository := &fakeCheckpointRepository{}
		engine := newRuntimeEngine(
			t, fixture, &fakeDirectoryAuthority{namespace: newFakePublicNamespace()}, platform, repository,
		)

		observation, err := engine.BeginFile(context.Background(), fixture.claim)
		if err == nil || observation.Cut != outputsession.MutationAmbiguous {
			t.Fatalf("candidate sync cut=%v err=%v", observation.Cut, err)
		}
		record := repository.current(t)
		if record.Phase() != checkpointmodel.PhaseActive ||
			record.CommitState() != checkpointmodel.CommitCandidate {
			t.Fatalf("candidate evidence phase=%v commit=%v", record.Phase(), record.CommitState())
		}
		basePlatform.mu.Lock()
		retirement := append([]RetirementStep(nil), basePlatform.retirement...)
		basePlatform.mu.Unlock()
		if len(retirement) != 0 {
			t.Fatalf("candidate sync failure retired evidence: %v", retirement)
		}
	})

	t.Run("known unchanged promotion", func(t *testing.T) {
		fixture := newEngineFixture(t, 2)
		platform := newFakePlatform()
		repository := &fakeCheckpointRepository{}
		repository.hooks = append(repository.hooks,
			installObservedCheckpoint,
			func(
				repository *fakeCheckpointRepository,
				_ *checkpointmodel.Record,
				_ checkpointmodel.Record,
			) (CheckpointObservation, error) {
				observation, _ := ObservedCheckpoint(repository.record)
				return observation, errors.New("promotion stopped before replacement")
			},
		)

		observation, err := newTestEngine(
			t, fixture, &fakeDirectoryAuthority{namespace: newFakePublicNamespace()}, platform, repository,
		).BeginFile(context.Background(), fixture.claim)
		if err == nil || observation.Cut != outputsession.MutationAmbiguous {
			t.Fatalf("unchanged promotion cut=%v err=%v", observation.Cut, err)
		}
		record := repository.current(t)
		if record.CommitState() != checkpointmodel.CommitCandidate || len(platform.retirement) != 0 {
			t.Fatalf("unchanged promotion lost candidate: commit=%v retirement=%v", record.CommitState(), platform.retirement)
		}
	})

	t.Run("installed promotion with diagnostic", func(t *testing.T) {
		fixture := newEngineFixture(t, 2)
		platform := newFakePlatform()
		repository := &fakeCheckpointRepository{}
		repository.hooks = append(repository.hooks,
			installObservedCheckpoint,
			func(
				repository *fakeCheckpointRepository,
				_ *checkpointmodel.Record,
				next checkpointmodel.Record,
			) (CheckpointObservation, error) {
				observation, _ := installObservedCheckpoint(repository, nil, next)
				return observation, errors.New("promotion installed before close diagnostic")
			},
		)
		engine := newTestEngine(
			t, fixture, &fakeDirectoryAuthority{namespace: newFakePublicNamespace()}, platform, repository,
		)
		var events []TraceEvent
		engine.trace = TraceSinkFunc(func(event TraceEvent) { events = append(events, event) })

		observation, err := engine.BeginFile(context.Background(), fixture.claim)
		if err != nil || observation.Cut != outputsession.MutationStable || observation.Transaction == nil {
			t.Fatalf("installed promotion cut=%v transaction=%T err=%v", observation.Cut, observation.Transaction, err)
		}
		event := traceByOperation(t, events, TraceCheckpoint)
		if event.Outcome != TraceReconciled {
			t.Fatalf("installed promotion trace=%+v", event)
		}
	})
}
