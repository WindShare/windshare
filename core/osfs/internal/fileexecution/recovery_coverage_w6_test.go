package fileexecution

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer"
)

func TestRecoveryReducerSeparatesDefiniteTamperFromUnknownOwnership(t *testing.T) {
	transaction, repository, _, _ := newCoverageTransaction(t, 4)
	active := repository.record
	object := active.OwnedObjectID()
	paused, err := pauseRecord(active)
	if err != nil {
		t.Fatal(err)
	}
	fullCandidate, err := checkpointmodel.AdvanceGeneration(
		active, []checkpointmodel.Range{{Offset: 0, End: 4}},
		checkpointmodel.PhaseActive, checkpointmodel.CommitCandidate,
	)
	if err != nil {
		t.Fatal(err)
	}
	fullActive, err := checkpointmodel.Promote(
		fullCandidate, checkpointmodel.PhaseActive, checkpointmodel.CommitVerified,
	)
	if err != nil {
		t.Fatal(err)
	}
	fullPaused, err := pauseRecord(fullActive)
	if err != nil {
		t.Fatal(err)
	}
	publishing, err := publishingRecord(fullActive)
	if err != nil {
		t.Fatal(err)
	}
	published, err := publishedRecord(publishing)
	if err != nil {
		t.Fatal(err)
	}
	retired, err := retiredRecord(active, checkpointmodel.RetirementInvalidatedRevision)
	if err != nil {
		t.Fatal(err)
	}

	owned := func(condition OwnedCondition) OwnedObservation {
		observation, err := NewOwnedObservation(object, condition)
		if err != nil {
			t.Fatal(err)
		}
		return observation
	}
	final := func(condition FinalCondition) FinalObservation {
		observation, err := ObserveFinal(condition)
		if err != nil {
			t.Fatal(err)
		}
		return observation
	}
	cases := []struct {
		name       string
		record     checkpointmodel.Record
		owned      OwnedCondition
		final      FinalCondition
		wantAction RecoveryAction
		wantReason checkpointmodel.QuarantineReason
	}{
		{"owned absent", active, OwnedAbsent, FinalAbsent, RecoveryInstallQuarantine, checkpointmodel.QuarantineAnchorMissing},
		{"anchor absent", active, OwnedAnchorMissing, FinalAbsent, RecoveryInstallQuarantine, checkpointmodel.QuarantineAnchorMissing},
		{"stage absent", active, OwnedStageMissing, FinalAbsent, RecoveryInstallQuarantine, checkpointmodel.QuarantineStageMissing},
		{"active collision", active, OwnedReady, FinalCollision, RecoveryInstallQuarantine, checkpointmodel.QuarantinePublicationHistory},
		{"paused incomplete collision", paused, OwnedReady, FinalCollision, RecoveryInstallQuarantine, checkpointmodel.QuarantinePublicationHistory},
		{"paused complete collision", fullPaused, OwnedReady, FinalCollision, RecoveryPublishBlocked, 0},
		{"active final exact", active, OwnedReady, FinalOwnedExact, RecoveryNeedsAttention, 0},
		{"active final mismatch", active, OwnedReady, FinalOwnedMetadataMismatch, RecoveryNeedsAttention, 0},
		{"publishing missing owned", publishing, OwnedStageMissing, FinalAbsent, RecoveryNeedsAttention, 0},
		{"publishing final unsafe", publishing, OwnedReady, FinalUnsafe, RecoveryNeedsAttention, 0},
		{"publishing final mismatch", publishing, OwnedReady, FinalOwnedMetadataMismatch, RecoveryNeedsAttention, 0},
		{"published mismatch", published, OwnedReady, FinalCollision, RecoveryNeedsAttention, 0},
		{"retired ownership unknown", retired, OwnedObjectCollision, FinalAbsent, RecoveryNeedsAttention, 0},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			decision, err := ReduceRecovery(test.record, owned(test.owned), final(test.final))
			if err != nil || decision.Action() != test.wantAction ||
				decision.QuarantineReason() != test.wantReason {
				t.Fatalf(
					"decision = (%d, %d, %v), want (%d, %d)",
					decision.Action(), decision.QuarantineReason(), err,
					test.wantAction, test.wantReason,
				)
			}
		})
	}
	if _, err := ReduceRecovery(fullCandidate, owned(OwnedReady), final(FinalAbsent)); !errors.Is(err, ErrCheckpointBinding) {
		t.Fatalf("candidate recovery error = %v", err)
	}
	if _, err := ReduceRecovery(checkpointmodel.Record{}, owned(OwnedReady), final(FinalAbsent)); !errors.Is(err, ErrCheckpointBinding) {
		t.Fatalf("zero record recovery error = %v", err)
	}
	if transaction.Binding().ObjectIdentity().IsZero() {
		t.Fatal("transaction fixture lost its owned identity")
	}

	for condition, reason := range map[OwnedCondition]checkpointmodel.QuarantineReason{
		OwnedAbsent:          checkpointmodel.QuarantineAnchorMissing,
		OwnedAnchorMissing:   checkpointmodel.QuarantineAnchorMissing,
		OwnedStageMissing:    checkpointmodel.QuarantineStageMissing,
		OwnedObjectCollision: checkpointmodel.QuarantineOutputObjectDuplicate,
		OwnedAnchorUnsafe:    checkpointmodel.QuarantineAnchorUnsafe,
		OwnedStageMismatch:   checkpointmodel.QuarantineStageMismatch,
		OwnedStageUnsafe:     checkpointmodel.QuarantineStageUnsafe,
		OwnedReady:           checkpointmodel.QuarantinePublicationHistory,
	} {
		if got := ownedQuarantineReason(condition); got != reason {
			t.Fatalf("owned condition %d quarantine = %d, want %d", condition, got, reason)
		}
	}
}

type coverageFaultOwnedFile struct {
	OwnedFile
	writeAt         func([]byte, int64) (int, error)
	sync            func() error
	setModifiedTime func(catalog.ModifiedTime) error
	metadataMatches func(uint64, catalog.ModifiedTime) (bool, error)
}

func (file *coverageFaultOwnedFile) WriteAt(data []byte, offset int64) (int, error) {
	if file.writeAt != nil {
		return file.writeAt(data, offset)
	}
	return file.OwnedFile.WriteAt(data, offset)
}

func (file *coverageFaultOwnedFile) Sync() error {
	if file.sync != nil {
		return file.sync()
	}
	return file.OwnedFile.Sync()
}

func (file *coverageFaultOwnedFile) SetModifiedTime(modified catalog.ModifiedTime) error {
	if file.setModifiedTime != nil {
		return file.setModifiedTime(modified)
	}
	return file.OwnedFile.SetModifiedTime(modified)
}

func (file *coverageFaultOwnedFile) MetadataMatches(
	exactSize uint64,
	modified catalog.ModifiedTime,
) (bool, error) {
	if file.metadataMatches != nil {
		return file.metadataMatches(exactSize, modified)
	}
	return file.OwnedFile.MetadataMatches(exactSize, modified)
}

func TestTransactionFaultBoundariesDoNotInventDurability(t *testing.T) {
	operationErr := errors.New("operation failed")

	t.Run("partial write is ambiguous", func(t *testing.T) {
		transaction, _, _, _ := newCoverageTransaction(t, 4)
		transaction.file = &coverageFaultOwnedFile{
			OwnedFile: transaction.file,
			writeAt:   func([]byte, int64) (int, error) { return 1, nil },
		}
		if err := transaction.WriteRange(context.Background(), 0, []byte("data")); !errors.Is(err, ErrPublicationAmbiguous) || !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("partial write error = %v", err)
		}
		if !transaction.pending.IsEmpty() {
			t.Fatal("ambiguous partial write became pending durable state")
		}
	})

	t.Run("zero write retains collaborator failure", func(t *testing.T) {
		transaction, _, _, _ := newCoverageTransaction(t, 4)
		transaction.file = &coverageFaultOwnedFile{
			OwnedFile: transaction.file,
			writeAt:   func([]byte, int64) (int, error) { return 0, operationErr },
		}
		if err := transaction.WriteRange(context.Background(), 0, []byte("data")); !errors.Is(err, operationErr) {
			t.Fatalf("zero write error = %v", err)
		}
	})

	t.Run("sync failure preserves prior checkpoint", func(t *testing.T) {
		transaction, repository, _, _ := newCoverageTransaction(t, 4)
		if err := transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
			t.Fatal(err)
		}
		transaction.file = &coverageFaultOwnedFile{
			OwnedFile: transaction.file, sync: func() error { return operationErr },
		}
		if _, err := transaction.Checkpoint(context.Background()); !errors.Is(err, operationErr) {
			t.Fatalf("sync error = %v", err)
		}
		if repository.record.CheckpointGeneration() != 0 {
			t.Fatal("failed sync advanced the durable checkpoint")
		}
	})

	for name, configure := range map[string]func(*Transaction, *memoryDestination){
		"modified time failure": func(transaction *Transaction, _ *memoryDestination) {
			transaction.file = &coverageFaultOwnedFile{
				OwnedFile:       transaction.file,
				setModifiedTime: func(catalog.ModifiedTime) error { return operationErr },
			}
		},
		"metadata mismatch": func(transaction *Transaction, _ *memoryDestination) {
			transaction.file = &coverageFaultOwnedFile{
				OwnedFile:       transaction.file,
				metadataMatches: func(uint64, catalog.ModifiedTime) (bool, error) { return false, nil },
			}
		},
		"metadata failure": func(transaction *Transaction, _ *memoryDestination) {
			transaction.file = &coverageFaultOwnedFile{
				OwnedFile:       transaction.file,
				metadataMatches: func(uint64, catalog.ModifiedTime) (bool, error) { return false, operationErr },
			}
		},
		"publication failure": func(_ *Transaction, destination *memoryDestination) {
			destination.publishErr = operationErr
		},
		"parent sync failure": func(_ *Transaction, destination *memoryDestination) {
			destination.syncErr = operationErr
		},
	} {
		t.Run(name, func(t *testing.T) {
			transaction, repository, _, destination := newCoverageTransaction(t, 4)
			if err := transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
				t.Fatal(err)
			}
			configure(transaction, destination)
			if _, err := transaction.Commit(context.Background()); err == nil {
				t.Fatal("faulted commit succeeded")
			}
			if repository.record.Phase() == checkpointmodel.PhasePublished {
				t.Fatal("faulted commit became published")
			}
		})
	}

	t.Run("retirement cleanup failure", func(t *testing.T) {
		transaction, _, platform, _ := newCoverageTransaction(t, 4)
		platform.retirementErr = operationErr
		if _, err := transaction.Retire(
			context.Background(), transfer.FileRetireInvalidatedRevision,
		); !errors.Is(err, operationErr) {
			t.Fatalf("retirement cleanup error = %v", err)
		}
	})

	transaction, _, _, _ := newCoverageTransaction(t, 1)
	if _, err := transaction.Pause(context.Background(), 0); !errors.Is(err, transfer.ErrInvalidOutputSettlement) {
		t.Fatalf("open pause reason error = %v", err)
	}
	if _, err := transaction.Retire(context.Background(), 0); !errors.Is(err, ErrRetirementUnauthorized) {
		t.Fatalf("open retirement reason error = %v", err)
	}
	var nilTransaction *Transaction
	if !nilTransaction.Binding().ObjectIdentity().IsZero() {
		t.Fatal("nil transaction exposed a binding")
	}
	if err := nilTransaction.WriteRange(context.Background(), 0, nil); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("nil write error = %v", err)
	}
	if _, err := nilTransaction.Checkpoint(context.Background()); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("nil checkpoint error = %v", err)
	}
	if _, err := nilTransaction.Commit(context.Background()); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("nil commit error = %v", err)
	}
}

func newCoverageTransaction(
	t *testing.T,
	exactSize uint64,
) (*Transaction, *memoryCheckpointRepository, *memoryPlatform, *memoryDestination) {
	t.Helper()
	fixture := newExecutionFixture(t, exactSize)
	repository := &memoryCheckpointRepository{}
	platform := newMemoryPlatform()
	destination := &memoryDestination{target: fixture.file.Target, final: FinalAbsent}
	engine := newFixtureEngine(t, fixture, repository, platform, destination, nil)
	start, err := engine.BeginFile(context.Background(), fixture.file)
	if err != nil {
		t.Fatal(err)
	}
	fileTransaction, _, ok := start.Transaction()
	if !ok {
		t.Fatal("fixture did not start a file transaction")
	}
	transaction, ok := fileTransaction.(*Transaction)
	if !ok {
		t.Fatalf("transaction = %T", fileTransaction)
	}
	return transaction, repository, platform, destination
}

func TestEngineRejectsCanceledAndUnboundCollaborators(t *testing.T) {
	fixture := newExecutionFixture(t, 1)
	repository := &memoryCheckpointRepository{lookupErr: errors.New("lookup failed")}
	platform := newMemoryPlatform()
	destination := &memoryDestination{target: fixture.file.Target, final: FinalAbsent}
	engine := newFixtureEngine(t, fixture, repository, platform, destination, nil)
	if _, err := engine.BeginFile(context.Background(), fixture.file); !errors.Is(err, repository.lookupErr) {
		t.Fatalf("lookup failure = %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := engine.BeginFile(canceled, fixture.file); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled begin = %v", err)
	}
	var nilEngine *Engine
	if _, err := nilEngine.BeginFile(context.Background(), fixture.file); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("nil engine begin = %v", err)
	}

	engine, err := New(Config{
		Intent: fixture.intent, Ownership: fixture.ownership, SessionID: fixture.session,
		Directories: &memoryDirectoryAuthority{err: errors.New("bind failed")},
		Platform:    platform, Checkpoints: &memoryCheckpointRepository{},
		Random: bytes.NewReader(bytes.Repeat([]byte{1}, transfer.OwnedObjectIdentityBytes)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.BeginFile(context.Background(), fixture.file); err == nil {
		t.Fatal("directory binding failure was ignored")
	}

	shortRandomEngine, err := New(Config{
		Intent: fixture.intent, Ownership: fixture.ownership, SessionID: fixture.session,
		Directories: &memoryDirectoryAuthority{destination: destination},
		Platform:    platform, Checkpoints: &memoryCheckpointRepository{}, Random: bytes.NewReader(nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := shortRandomEngine.BeginFile(context.Background(), fixture.file); err == nil {
		t.Fatal("short object identity source was ignored")
	}
}

type coverageCheckpointRepository struct {
	CheckpointRepository
	lookup func(context.Context, CheckpointKey) (checkpointmodel.Record, bool, error)
	store  func(context.Context, *checkpointmodel.Record, checkpointmodel.Record) (CheckpointObservation, error)
}

func (repository *coverageCheckpointRepository) Lookup(
	ctx context.Context,
	key CheckpointKey,
) (checkpointmodel.Record, bool, error) {
	if repository.lookup != nil {
		return repository.lookup(ctx, key)
	}
	return repository.CheckpointRepository.Lookup(ctx, key)
}

func (repository *coverageCheckpointRepository) Store(
	ctx context.Context,
	previous *checkpointmodel.Record,
	next checkpointmodel.Record,
) (CheckpointObservation, error) {
	if repository.store != nil {
		return repository.store(ctx, previous, next)
	}
	return repository.CheckpointRepository.Store(ctx, previous, next)
}

type coveragePlatform struct {
	Platform
	create func(context.Context, checkpointmodel.ObjectID, uint64) (OwnedFile, OwnedObservation, error)
	open   func(context.Context, checkpointmodel.ObjectID, uint64, bool) (OwnedFile, OwnedObservation, error)
}

func (platform *coveragePlatform) CreateOwnedFile(
	ctx context.Context,
	object checkpointmodel.ObjectID,
	exactSize uint64,
) (OwnedFile, OwnedObservation, error) {
	if platform.create != nil {
		return platform.create(ctx, object, exactSize)
	}
	return platform.Platform.CreateOwnedFile(ctx, object, exactSize)
}

func (platform *coveragePlatform) OpenOwnedFile(
	ctx context.Context,
	object checkpointmodel.ObjectID,
	exactSize uint64,
	writable bool,
) (OwnedFile, OwnedObservation, error) {
	if platform.open != nil {
		return platform.open(ctx, object, exactSize, writable)
	}
	return platform.Platform.OpenOwnedFile(ctx, object, exactSize, writable)
}

type coverageDirectoryAuthority struct {
	destination FileDestination
	err         error
}

func (authority *coverageDirectoryAuthority) BindFile(
	context.Context,
	transfer.MaterializationFile,
) (FileDestination, error) {
	return authority.destination, authority.err
}

func TestStoreRecordCrashCutUsesObservationAsTheOnlyInstallAuthority(t *testing.T) {
	transaction, repository, _, _ := newCoverageTransaction(t, 4)
	engine := transaction.engine
	previous := repository.record
	next, err := pauseRecord(previous)
	if err != nil {
		t.Fatal(err)
	}
	operationErr := errors.New("write outcome unknown")

	for name, test := range map[string]struct {
		previous    *checkpointmodel.Record
		observation CheckpointObservation
		operation   error
		wantChanged bool
		want        error
	}{
		"next observed despite operation error": {&previous, mustObservedCheckpoint(t, next), operationErr, true, nil},
		"previous remains":                      {&previous, mustObservedCheckpoint(t, previous), operationErr, false, ErrCheckpointNotInstalled},
		"create remains missing":                {nil, MissingCheckpoint(), operationErr, false, ErrCheckpointNotInstalled},
		"foreign observation":                   {&previous, mustObservedCheckpoint(t, mustForeignExecutionRecord(t, previous)), operationErr, false, ErrTargetOwnershipUnknown},
		"invalid observation":                   {&previous, CheckpointObservation{present: true}, operationErr, false, ErrInvalidObservation},
	} {
		t.Run(name, func(t *testing.T) {
			original := engine.checkpoints
			engine.checkpoints = &coverageCheckpointRepository{
				CheckpointRepository: original,
				store: func(context.Context, *checkpointmodel.Record, checkpointmodel.Record) (CheckpointObservation, error) {
					return test.observation, test.operation
				},
			}
			defer func() { engine.checkpoints = original }()
			changed, err := engine.storeRecord(context.Background(), test.previous, next)
			if changed != test.wantChanged || (test.want == nil && err != nil) ||
				test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("store decision = (%t, %v), want (%t, %v)", changed, err, test.wantChanged, test.want)
			}
		})
	}

	if _, err := engine.storeRecord(nil, &previous, next); !errors.Is(err, ErrCheckpointBinding) {
		t.Fatalf("nil store context error = %v", err)
	}
	foreignPrevious := mustForeignExecutionRecord(t, previous)
	if _, err := engine.storeRecord(context.Background(), &foreignPrevious, next); !errors.Is(err, ErrCheckpointBinding) {
		t.Fatalf("foreign previous error = %v", err)
	}
}

func mustObservedCheckpoint(t *testing.T, record checkpointmodel.Record) CheckpointObservation {
	t.Helper()
	observation, err := ObservedCheckpoint(record)
	if err != nil {
		t.Fatal(err)
	}
	return observation
}

func mustForeignExecutionRecord(t *testing.T, record checkpointmodel.Record) checkpointmodel.Record {
	t.Helper()
	spec := checkpointmodel.RecordSpec{
		OperationID: record.OperationID(), ReceiveIntentDigest: record.ReceiveIntentDigest(),
		MaterializationBindingDigest: record.MaterializationBindingDigest(),
		FileID:                       record.FileID(), FileRevision: record.FileRevision(), CanonicalPath: record.CanonicalPath(),
		ExactSize: record.ExactSize(), MaterializerKind: record.MaterializerKind(),
		AuthorityRef: record.AuthorityRef().Bytes(), OwnedObjectID: bytes.Repeat([]byte{0xf1}, transfer.OwnedObjectIdentityBytes),
		StateGeneration: record.StateGeneration(), CheckpointGeneration: record.CheckpointGeneration(),
		VerifiedRanges: record.VerifiedRanges(), Phase: record.Phase(), CommitState: record.CommitState(),
		QuarantineReason: record.QuarantineReason(), QuarantineOrigin: record.QuarantineOrigin(),
		RetirementReason: record.RetirementReason(),
	}
	foreign, err := checkpointmodel.NewRecord(spec)
	if err != nil {
		t.Fatal(err)
	}
	return foreign
}

func TestBeginNewRejectsPortViolationsAndAmbiguousOwnedCreation(t *testing.T) {
	fixture := newExecutionFixture(t, 4)
	newEngine := func() (*Engine, *memoryCheckpointRepository, *memoryPlatform, *memoryDestination) {
		repository := &memoryCheckpointRepository{}
		platform := newMemoryPlatform()
		destination := &memoryDestination{target: fixture.file.Target, final: FinalAbsent}
		return newFixtureEngine(t, fixture, repository, platform, destination, nil), repository, platform, destination
	}

	t.Run("nil destination", func(t *testing.T) {
		engine, _, _, _ := newEngine()
		engine.directories = &coverageDirectoryAuthority{}
		if _, err := engine.BeginFile(context.Background(), fixture.file); !errors.Is(err, ErrPortContract) {
			t.Fatalf("nil destination error = %v", err)
		}
	})

	t.Run("wrong destination target", func(t *testing.T) {
		engine, _, _, destination := newEngine()
		destination.target = transfer.FileMaterializationTarget{}
		if _, err := engine.BeginFile(context.Background(), fixture.file); !errors.Is(err, ErrPortContract) {
			t.Fatalf("wrong target error = %v", err)
		}
	})

	for name, final := range map[string]FinalCondition{
		"foreign collision":  FinalCollision,
		"owned collision":    FinalOwnedExact,
		"metadata collision": FinalOwnedMetadataMismatch,
	} {
		t.Run(name, func(t *testing.T) {
			engine, repository, _, destination := newEngine()
			destination.final = final
			start, err := engine.BeginFile(context.Background(), fixture.file)
			settlement, immediate := start.ImmediateSettlement()
			if err != nil || !immediate || settlement.Kind() != transfer.FileCollision || repository.present {
				t.Fatalf("collision start = (%d, %t, present=%t, %v)", settlement.Kind(), immediate, repository.present, err)
			}
		})
	}

	t.Run("invalid owned observation", func(t *testing.T) {
		engine, _, platform, _ := newEngine()
		engine.platform = &coveragePlatform{
			Platform: platform,
			create: func(context.Context, checkpointmodel.ObjectID, uint64) (OwnedFile, OwnedObservation, error) {
				return nil, OwnedObservation{}, nil
			},
		}
		if _, err := engine.BeginFile(context.Background(), fixture.file); !errors.Is(err, ErrInvalidObservation) {
			t.Fatalf("invalid owned observation error = %v", err)
		}
	})

	t.Run("collision cannot return a file", func(t *testing.T) {
		engine, _, platform, _ := newEngine()
		engine.platform = &coveragePlatform{
			Platform: platform,
			create: func(_ context.Context, object checkpointmodel.ObjectID, size uint64) (OwnedFile, OwnedObservation, error) {
				observation, _ := NewOwnedObservation(object, OwnedObjectCollision)
				return &memoryOwnedFile{object: object, data: &memoryOwnedData{bytes: make([]byte, size)}}, observation, nil
			},
		}
		if _, err := engine.BeginFile(context.Background(), fixture.file); !errors.Is(err, ErrPortContract) {
			t.Fatalf("collision file error = %v", err)
		}
	})

	t.Run("ready observation must carry exact object", func(t *testing.T) {
		engine, _, platform, _ := newEngine()
		foreign, err := checkpointmodel.ObjectIDFromBytes(bytes.Repeat([]byte{0xf2}, transfer.OwnedObjectIdentityBytes))
		if err != nil {
			t.Fatal(err)
		}
		engine.platform = &coveragePlatform{
			Platform: platform,
			create: func(_ context.Context, object checkpointmodel.ObjectID, size uint64) (OwnedFile, OwnedObservation, error) {
				observation, _ := NewOwnedObservation(object, OwnedReady)
				return &memoryOwnedFile{object: foreign, data: &memoryOwnedData{bytes: make([]byte, size)}}, observation, nil
			},
		}
		if _, err := engine.BeginFile(context.Background(), fixture.file); !errors.Is(err, ErrTargetOwnershipUnknown) {
			t.Fatalf("foreign owned object error = %v", err)
		}
	})

	t.Run("candidate store failure closes owned object", func(t *testing.T) {
		engine, repository, _, _ := newEngine()
		repository.storeErr = errors.New("checkpoint unavailable")
		if _, err := engine.BeginFile(context.Background(), fixture.file); !errors.Is(err, repository.storeErr) {
			t.Fatalf("candidate store error = %v", err)
		}
	})

	t.Run("owned sync failure cannot promote candidate", func(t *testing.T) {
		engine, _, platform, _ := newEngine()
		syncFailure := errors.New("owned sync failed")
		engine.platform = &coveragePlatform{
			Platform: platform,
			create: func(_ context.Context, object checkpointmodel.ObjectID, size uint64) (OwnedFile, OwnedObservation, error) {
				observation, _ := NewOwnedObservation(object, OwnedReady)
				return &memoryOwnedFile{object: object, data: &memoryOwnedData{bytes: make([]byte, size)}, syncErr: syncFailure}, observation, nil
			},
		}
		if _, err := engine.BeginFile(context.Background(), fixture.file); !errors.Is(err, syncFailure) {
			t.Fatalf("owned sync error = %v", err)
		}
	})
}

func TestBeginExistingRejectsUnverifiableOwnedAndFinalObservations(t *testing.T) {
	fixture := newExecutionFixture(t, 4)
	seedRepository := &memoryCheckpointRepository{}
	seedPlatform := newMemoryPlatform()
	seedDestination := &memoryDestination{target: fixture.file.Target, final: FinalAbsent}
	seedEngine := newFixtureEngine(t, fixture, seedRepository, seedPlatform, seedDestination, nil)
	key, err := seedEngine.checkpointKey(fixture.file)
	if err != nil {
		t.Fatal(err)
	}
	object, err := checkpointmodel.ObjectIDFromBytes(bytes.Repeat([]byte{0xe1}, transfer.OwnedObjectIdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := newInitialRecord(key, object)
	if err != nil {
		t.Fatal(err)
	}
	active, err := checkpointmodel.PromoteInitialCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}

	newExistingEngine := func(record checkpointmodel.Record) (*Engine, *memoryPlatform, *memoryDestination) {
		repository := &memoryCheckpointRepository{record: record, present: true}
		platform := newMemoryPlatform()
		platform.objects[object] = &memoryOwnedData{bytes: make([]byte, 4)}
		destination := &memoryDestination{target: fixture.file.Target, final: FinalAbsent}
		return newFixtureEngine(t, fixture, repository, platform, destination, nil), platform, destination
	}

	t.Run("ready without file", func(t *testing.T) {
		engine, platform, _ := newExistingEngine(active)
		delete(platform.objects, object)
		if _, err := engine.BeginFile(context.Background(), fixture.file); !errors.Is(err, ErrInvalidObservation) {
			t.Fatalf("ready-without-file error = %v", err)
		}
	})

	t.Run("nonready with file", func(t *testing.T) {
		engine, platform, _ := newExistingEngine(active)
		engine.platform = &coveragePlatform{
			Platform: platform,
			open: func(_ context.Context, object checkpointmodel.ObjectID, size uint64, _ bool) (OwnedFile, OwnedObservation, error) {
				observation, _ := NewOwnedObservation(object, OwnedStageMissing)
				return &memoryOwnedFile{object: object, data: &memoryOwnedData{bytes: make([]byte, size)}}, observation, nil
			},
		}
		if _, err := engine.BeginFile(context.Background(), fixture.file); !errors.Is(err, ErrInvalidObservation) {
			t.Fatalf("nonready-with-file error = %v", err)
		}
	})

	t.Run("final observation failure", func(t *testing.T) {
		engine, _, destination := newExistingEngine(active)
		destination.observeErr = errors.New("final observation failed")
		if _, err := engine.BeginFile(context.Background(), fixture.file); !errors.Is(err, destination.observeErr) {
			t.Fatalf("final observation error = %v", err)
		}
	})

	t.Run("candidate needs exact owned witness", func(t *testing.T) {
		engine, platform, _ := newExistingEngine(candidate)
		platform.openCondition = OwnedStageMissing
		if _, err := engine.BeginFile(context.Background(), fixture.file); !errors.Is(err, ErrTargetOwnershipUnknown) {
			t.Fatalf("candidate witness error = %v", err)
		}
	})

	t.Run("candidate sync failure remains unverified", func(t *testing.T) {
		engine, platform, _ := newExistingEngine(candidate)
		syncFailure := errors.New("candidate sync failed")
		engine.platform = &coveragePlatform{
			Platform: platform,
			open: func(_ context.Context, object checkpointmodel.ObjectID, size uint64, _ bool) (OwnedFile, OwnedObservation, error) {
				observation, _ := NewOwnedObservation(object, OwnedReady)
				return &memoryOwnedFile{object: object, data: &memoryOwnedData{bytes: make([]byte, size)}, syncErr: syncFailure}, observation, nil
			},
		}
		if _, err := engine.BeginFile(context.Background(), fixture.file); !errors.Is(err, syncFailure) {
			t.Fatalf("candidate sync error = %v", err)
		}
	})
}

func recordForRecoveryPhaseCoverage(
	t *testing.T,
	record checkpointmodel.Record,
	phase checkpointmodel.Phase,
	commit checkpointmodel.CommitState,
	generation uint64,
) checkpointmodel.Record {
	t.Helper()
	spec := checkpointmodel.RecordSpec{
		OperationID: record.OperationID(), ReceiveIntentDigest: record.ReceiveIntentDigest(),
		MaterializationBindingDigest: record.MaterializationBindingDigest(),
		FileID:                       record.FileID(), FileRevision: record.FileRevision(), CanonicalPath: record.CanonicalPath(),
		ExactSize: record.ExactSize(), MaterializerKind: record.MaterializerKind(),
		AuthorityRef: record.AuthorityRef().Bytes(), OwnedObjectID: record.OwnedObjectID().Bytes(),
		StateGeneration: generation, CheckpointGeneration: record.CheckpointGeneration(),
		VerifiedRanges: record.VerifiedRanges(), Phase: phase, CommitState: commit,
	}
	rebuilt, err := checkpointmodel.NewRecord(spec)
	if err != nil {
		t.Fatal(err)
	}
	return rebuilt
}

func TestRecoveryReducerRejectsUnreachablePhasesAndIncompletePublication(t *testing.T) {
	_, repository, _, _ := newCoverageTransaction(t, 4)
	active := repository.record
	object := active.OwnedObjectID()
	owned := func(condition OwnedCondition) OwnedObservation {
		observation, err := NewOwnedObservation(object, condition)
		if err != nil {
			t.Fatal(err)
		}
		return observation
	}
	final := func(condition FinalCondition) FinalObservation {
		observation, err := ObserveFinal(condition)
		if err != nil {
			t.Fatal(err)
		}
		return observation
	}

	reserved := recordForRecoveryPhaseCoverage(
		t, active, checkpointmodel.PhaseReserved, checkpointmodel.CommitCandidate, 1,
	)
	if _, err := ReduceRecovery(reserved, owned(OwnedReady), final(FinalAbsent)); !errors.Is(err, ErrCheckpointBinding) {
		t.Fatalf("reserved recovery error = %v", err)
	}
	decision, err := ReduceRecovery(active, owned(OwnedObjectCollision), final(FinalAbsent))
	if err != nil || decision.Action() != RecoveryNeedsAttention {
		t.Fatalf("ambiguous owned recovery = (%d, %v)", decision.Action(), err)
	}
	publishing, err := publishingRecord(active)
	if err != nil {
		t.Fatal(err)
	}
	decision, err = ReduceRecovery(publishing, owned(OwnedReady), final(FinalAbsent))
	if err != nil || decision.Action() != RecoveryNeedsAttention {
		t.Fatalf("incomplete publication recovery = (%d, %v)", decision.Action(), err)
	}
	quarantined, err := quarantineRecord(active, checkpointmodel.QuarantineAnchorUnsafe)
	if err != nil {
		t.Fatal(err)
	}
	decision, err = ReduceRecovery(quarantined, OwnedObservation{}, FinalObservation{})
	if err != nil || decision.Action() != RecoveryReturnQuarantined {
		t.Fatalf("quarantined recovery = (%d, %v)", decision.Action(), err)
	}
}

func TestRecordTransitionsRejectGenerationAndQuarantineAuthorityGaps(t *testing.T) {
	_, repository, _, _ := newCoverageTransaction(t, 4)
	active := repository.record
	if _, err := newInitialRecord(CheckpointKey{}, checkpointmodel.ObjectID{}); !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("invalid initial record error = %v", err)
	}
	if _, err := nextStateGeneration(checkpointmodel.Record{}); !errors.Is(err, checkpointmodel.ErrRecordGeneration) {
		t.Fatalf("invalid generation error = %v", err)
	}
	exhausted := recordForRecoveryPhaseCoverage(
		t, active, checkpointmodel.PhaseActive, checkpointmodel.CommitVerified, math.MaxUint64,
	)
	if _, err := nextStateGeneration(exhausted); !errors.Is(err, checkpointmodel.ErrRecordGeneration) {
		t.Fatalf("exhausted generation error = %v", err)
	}
	if _, err := transitionRecord(exhausted, checkpointmodel.PhasePaused, checkpointmodel.CommitVerified, 0, 0, 0); !errors.Is(err, checkpointmodel.ErrRecordGeneration) {
		t.Fatalf("exhausted transition error = %v", err)
	}
	if restored, err := activateRecord(active); err != nil || !recordEqual(restored, active) {
		t.Fatalf("active idempotence = (%t, %v)", recordEqual(restored, active), err)
	}
	if _, err := quarantineRecord(active, 0); !errors.Is(err, checkpointmodel.ErrRecordGeneration) {
		t.Fatalf("open quarantine reason error = %v", err)
	}

	for phase, want := range map[checkpointmodel.Phase]checkpointmodel.QuarantineOrigin{
		checkpointmodel.PhaseReserved:    checkpointmodel.QuarantineOriginReserved,
		checkpointmodel.PhaseActive:      checkpointmodel.QuarantineOriginWitnessed,
		checkpointmodel.PhasePaused:      checkpointmodel.QuarantineOriginWitnessed,
		checkpointmodel.PhasePublishing:  checkpointmodel.QuarantineOriginPublishing,
		checkpointmodel.PhasePublished:   checkpointmodel.QuarantineOriginPublished,
		checkpointmodel.PhaseRetired:     checkpointmodel.QuarantineOriginRetiring,
		checkpointmodel.PhaseQuarantined: 0,
	} {
		if got := quarantineOrigin(phase); got != want {
			t.Fatalf("phase %d quarantine origin = %d, want %d", phase, got, want)
		}
	}
}
