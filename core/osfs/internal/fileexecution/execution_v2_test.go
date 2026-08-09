package fileexecution

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type executionFixture struct {
	intent    transfer.ReceiveIntent
	ownership checkpointmodel.Ownership
	session   transfer.OutputSessionID
	file      transfer.MaterializationFile
}

func newExecutionFixture(t *testing.T, exactSize uint64) executionFixture {
	t.Helper()
	var share catalog.ShareInstance
	var root catalog.DirectoryID
	var generation catalog.DirectoryGeneration
	var fileID catalog.FileID
	var revision content.FileRevision
	share[0], root[0], generation[0], fileID[0], revision[0] = 0x11, 0x12, 0x13, 0x14, 0x15
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := transfer.NewSelectionSpec(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	artifact := receivecontract.NewCatalogRootDirectoryTree()
	operation, err := receivecontract.OperationIDFromBytes(bytes.Repeat([]byte{0x21}, receivecontract.StableIdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	reservationID, err := receivecontract.DestinationReservationIDFromBytes(bytes.Repeat([]byte{0x22}, receivecontract.StableIdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	authority, err := receivecontract.AuthorityRefFromBytes(bytes.Repeat([]byte{0x23}, receivecontract.AuthorityRefBytes))
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := receivecontract.NewNativeContainerRootReservation(operation, reservationID, artifact, authority)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := receivecontract.NewDirectTreePlan(artifact, reservation)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.NewReceiveIntent(selection, artifact, plan)
	if err != nil {
		t.Fatal(err)
	}
	ownership, err := checkpointmodel.NewOwnership(checkpointmodel.OwnershipSpec{
		Materializer:        checkpointmodel.MaterializerNativeTree,
		Certification:       checkpointmodel.CertificationWindowsNTFSProcessRestart,
		AuthorityRef:        authority.Bytes(),
		RootOpenDisposition: checkpointmodel.CallerProvidedContainer,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := transfer.OutputSessionIDFromBytes(bytes.Repeat([]byte{0x24}, transfer.OutputSessionIdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	geometry, err := content.NewFileGeometry(exactSize, catalog.MinChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(share, fileID, revision, geometry, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	locator, err := transfer.NewPathMaterializationLocator("file.bin")
	if err != nil {
		t.Fatal(err)
	}
	target, err := transfer.NewFileMaterializationTarget(session, descriptor, locator)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := transfer.NewDirectoryAdmissionScope(intent)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := transfer.NewDirectoryAdmissionWithSecret(
		bytes.Repeat([]byte{0x25}, 32),
		scope,
		transfer.MaterializationDirectory{DirectoryID: root, Generation: generation},
	)
	if err != nil {
		t.Fatal(err)
	}
	return executionFixture{
		intent: intent, ownership: ownership, session: session,
		file: transfer.MaterializationFile{
			Path: "file.bin", ExpectedSize: exactSize, Descriptor: descriptor,
			Target: target, ParentAdmission: admission,
		},
	}
}

type memoryCheckpointRepository struct {
	record    checkpointmodel.Record
	present   bool
	stores    []checkpointmodel.Record
	lookupErr error
	storeErr  error
}

func (repository *memoryCheckpointRepository) Lookup(
	context.Context,
	CheckpointKey,
) (checkpointmodel.Record, bool, error) {
	return repository.record, repository.present, repository.lookupErr
}

func (repository *memoryCheckpointRepository) Store(
	_ context.Context,
	previous *checkpointmodel.Record,
	next checkpointmodel.Record,
) (CheckpointObservation, error) {
	if repository.storeErr != nil {
		if repository.present {
			observed, _ := ObservedCheckpoint(repository.record)
			return observed, repository.storeErr
		}
		return MissingCheckpoint(), repository.storeErr
	}
	if previous == nil {
		if repository.present {
			observed, _ := ObservedCheckpoint(repository.record)
			return observed, nil
		}
	} else if !repository.present || !recordEqual(repository.record, *previous) {
		if !repository.present {
			return MissingCheckpoint(), nil
		}
		observed, _ := ObservedCheckpoint(repository.record)
		return observed, nil
	}
	repository.record, repository.present = next, true
	repository.stores = append(repository.stores, next)
	observed, err := ObservedCheckpoint(next)
	return observed, err
}

type memoryOwnedData struct {
	bytes    []byte
	modified catalog.ModifiedTime
}

type memoryOwnedFile struct {
	object      checkpointmodel.ObjectID
	data        *memoryOwnedData
	closed      bool
	writeErr    error
	syncErr     error
	metadataErr error
}

func (file *memoryOwnedFile) ObjectID() checkpointmodel.ObjectID { return file.object }
func (file *memoryOwnedFile) WriteAt(value []byte, offset int64) (int, error) {
	if file.writeErr != nil {
		return 0, file.writeErr
	}
	if offset < 0 || offset+int64(len(value)) > int64(len(file.data.bytes)) {
		return 0, io.ErrShortWrite
	}
	return copy(file.data.bytes[offset:], value), nil
}
func (file *memoryOwnedFile) Sync() error { return file.syncErr }
func (file *memoryOwnedFile) SetModifiedTime(value catalog.ModifiedTime) error {
	file.data.modified = value
	return nil
}
func (file *memoryOwnedFile) MetadataMatches(size uint64, modified catalog.ModifiedTime) (bool, error) {
	return uint64(len(file.data.bytes)) == size && file.data.modified == modified, file.metadataErr
}
func (file *memoryOwnedFile) Close() error {
	file.closed = true
	return nil
}

type memoryPlatform struct {
	objects          map[checkpointmodel.ObjectID]*memoryOwnedData
	createCollisions int
	openCondition    OwnedCondition
	retirements      []RetirementStep
	retirementErr    error
}

func newMemoryPlatform() *memoryPlatform {
	return &memoryPlatform{objects: make(map[checkpointmodel.ObjectID]*memoryOwnedData)}
}

func (platform *memoryPlatform) CreateOwnedFile(
	_ context.Context,
	object checkpointmodel.ObjectID,
	size uint64,
) (OwnedFile, OwnedObservation, error) {
	if platform.createCollisions > 0 {
		platform.createCollisions--
		observation, _ := NewOwnedObservation(object, OwnedObjectCollision)
		return nil, observation, nil
	}
	data := &memoryOwnedData{bytes: make([]byte, size)}
	platform.objects[object] = data
	observation, _ := NewOwnedObservation(object, OwnedReady)
	return &memoryOwnedFile{object: object, data: data}, observation, nil
}

func (platform *memoryPlatform) OpenOwnedFile(
	_ context.Context,
	object checkpointmodel.ObjectID,
	_ uint64,
	_ bool,
) (OwnedFile, OwnedObservation, error) {
	condition := platform.openCondition
	if condition == 0 {
		condition = OwnedReady
	}
	observation, _ := NewOwnedObservation(object, condition)
	if condition != OwnedReady {
		return nil, observation, nil
	}
	data := platform.objects[object]
	if data == nil {
		return nil, observation, nil
	}
	return &memoryOwnedFile{object: object, data: data}, observation, nil
}

func (platform *memoryPlatform) ApplyRetirement(
	_ context.Context,
	object checkpointmodel.ObjectID,
	step RetirementStep,
) (OwnedObservation, error) {
	platform.retirements = append(platform.retirements, step)
	condition := OwnedStageMissing
	if step == RetirementRemoveAnchor || step == RetirementSyncAnchorNamespace {
		condition = OwnedAbsent
	}
	observation, _ := NewOwnedObservation(object, condition)
	return observation, platform.retirementErr
}

type memoryDestination struct {
	target     transfer.FileMaterializationTarget
	final      FinalCondition
	publish    FinalCondition
	observeErr error
	publishErr error
	syncErr    error
	closed     bool
}

func (destination *memoryDestination) Target() transfer.FileMaterializationTarget {
	return destination.target
}
func (destination *memoryDestination) ObserveFinal(context.Context, FinalExpectation) (FinalObservation, error) {
	observation, _ := ObserveFinal(destination.final)
	return observation, destination.observeErr
}
func (destination *memoryDestination) ObserveFinalPresence(context.Context) (FinalObservation, error) {
	observation, _ := ObserveFinal(destination.final)
	return observation, destination.observeErr
}
func (destination *memoryDestination) PublishNoReplace(
	context.Context,
	OwnedFile,
	FinalExpectation,
) (FinalObservation, error) {
	condition := destination.publish
	if condition == 0 {
		condition = FinalOwnedExact
	}
	destination.final = condition
	observation, _ := ObserveFinal(condition)
	return observation, destination.publishErr
}
func (destination *memoryDestination) SyncFinalParent(context.Context) error {
	return destination.syncErr
}
func (destination *memoryDestination) Close() error {
	destination.closed = true
	return nil
}

type memoryDirectoryAuthority struct {
	destination *memoryDestination
	err         error
}

func (authority *memoryDirectoryAuthority) BindFile(
	context.Context,
	transfer.MaterializationFile,
) (FileDestination, error) {
	if authority.err != nil {
		return nil, authority.err
	}
	authority.destination.closed = false
	return authority.destination, nil
}

func newFixtureEngine(
	t *testing.T,
	fixture executionFixture,
	repository *memoryCheckpointRepository,
	platform *memoryPlatform,
	destination *memoryDestination,
	trace TraceSink,
) *Engine {
	t.Helper()
	random := make([]byte, transfer.OwnedObjectIdentityBytes*MaximumObjectAllocationAttempts)
	for block := range MaximumObjectAllocationAttempts {
		for index := range transfer.OwnedObjectIdentityBytes {
			random[block*transfer.OwnedObjectIdentityBytes+index] = byte(block + 1)
		}
	}
	engine, err := New(Config{
		Intent: fixture.intent, Ownership: fixture.ownership, SessionID: fixture.session,
		Directories: &memoryDirectoryAuthority{destination: destination},
		Platform:    platform, Checkpoints: repository, Random: bytes.NewReader(random), Trace: trace,
	})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func TestDirectTreeFileCheckpointIsTheOnlyRangeAuthority(t *testing.T) {
	fixture := newExecutionFixture(t, 8)
	repository := &memoryCheckpointRepository{}
	platform := newMemoryPlatform()
	destination := &memoryDestination{target: fixture.file.Target, final: FinalAbsent}
	var traces []TraceEvent
	engine := newFixtureEngine(t, fixture, repository, platform, destination, TraceSinkFunc(func(event TraceEvent) {
		traces = append(traces, event)
	}))

	start, err := engine.BeginFile(context.Background(), fixture.file)
	if err != nil {
		t.Fatal(err)
	}
	transaction, durable, ok := start.Transaction()
	if !ok || !durable.Ranges().IsEmpty() {
		t.Fatal("new file did not start with an empty durable checkpoint")
	}
	if err := transaction.WriteRange(context.Background(), 4, []byte("efgh")); err != nil {
		t.Fatal(err)
	}
	if err := transaction.WriteRange(context.Background(), 0, []byte("abcd")); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := transaction.Checkpoint(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !transfer.RangesCoverFile(8, checkpoint.Ranges()) || len(repository.stores) != 4 {
		t.Fatalf("checkpoint = %#v, store cuts = %d", checkpoint.Ranges().Ranges(), len(repository.stores))
	}
	settlement, err := transaction.Commit(context.Background())
	if err != nil || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("commit = (%d, %v)", settlement.Kind(), err)
	}
	if repository.record.Phase() != checkpointmodel.PhasePublished ||
		len(repository.record.VerifiedRanges()) != 1 ||
		repository.record.VerifiedRanges()[0] != (checkpointmodel.Range{Offset: 0, End: 8}) {
		t.Fatalf("published checkpoint = phase %d ranges %#v", repository.record.Phase(), repository.record.VerifiedRanges())
	}
	wantRetirement := []RetirementStep{RetirementRemoveStage, RetirementSyncStageNamespace}
	if !slicesEqual(platform.retirements, wantRetirement) {
		t.Fatalf("published cleanup = %v, want %v", platform.retirements, wantRetirement)
	}
	if len(traces) == 0 {
		t.Fatal("execution emitted no structured milestones")
	}
	for index, event := range traces {
		if event.OperationID != fixture.intent.OperationID() || event.IntentDigest != fixture.intent.Digest() ||
			event.SessionID != fixture.session || event.Sequence != uint64(index+1) {
			t.Fatalf("trace %d lost stable execution context: %+v", index, event)
		}
	}
}

func TestPausedCheckpointReopensAndRetiresInOwnershipOrder(t *testing.T) {
	fixture := newExecutionFixture(t, 8)
	repository := &memoryCheckpointRepository{}
	platform := newMemoryPlatform()
	destination := &memoryDestination{target: fixture.file.Target, final: FinalAbsent}
	engine := newFixtureEngine(t, fixture, repository, platform, destination, nil)
	start, err := engine.BeginFile(context.Background(), fixture.file)
	if err != nil {
		t.Fatal(err)
	}
	transaction, _, _ := start.Transaction()
	if err := transaction.WriteRange(context.Background(), 0, []byte("abcd")); err != nil {
		t.Fatal(err)
	}
	paused, err := transaction.Pause(context.Background(), transfer.FilePauseInterrupted)
	if err != nil || paused.Kind() != transfer.FilePaused || repository.record.Phase() != checkpointmodel.PhasePaused {
		t.Fatalf("pause = (%d, %v), phase %d", paused.Kind(), err, repository.record.Phase())
	}

	reopenedEngine := newFixtureEngine(t, fixture, repository, platform, destination, nil)
	reopened, err := reopenedEngine.BeginFile(context.Background(), fixture.file)
	if err != nil {
		t.Fatal(err)
	}
	reopenedTransaction, durable, ok := reopened.Transaction()
	if !ok || durable.Ranges().Ranges()[0] != (content.Range{Offset: 0, End: 4}) {
		t.Fatalf("reopened durable ranges = %#v", durable.Ranges().Ranges())
	}
	retired, err := reopenedTransaction.Retire(
		context.Background(), transfer.FileRetireIsolatedPermanentSourceFailure,
	)
	if err != nil || retired.Kind() != transfer.FileRetired {
		t.Fatalf("retire = (%d, %v)", retired.Kind(), err)
	}
	want := []RetirementStep{
		RetirementRemoveStage, RetirementSyncStageNamespace,
		RetirementRemoveAnchor, RetirementSyncAnchorNamespace,
	}
	if !slicesEqual(platform.retirements, want) {
		t.Fatalf("retirement order = %v, want %v", platform.retirements, want)
	}
}

func TestLaterGenerationCandidateReopensAtItsDurableRangeCut(t *testing.T) {
	fixture := newExecutionFixture(t, 8)
	repository := &memoryCheckpointRepository{}
	platform := newMemoryPlatform()
	destination := &memoryDestination{target: fixture.file.Target, final: FinalAbsent}
	seedEngine := newFixtureEngine(t, fixture, repository, platform, destination, nil)
	key, err := seedEngine.checkpointKey(fixture.file)
	if err != nil {
		t.Fatal(err)
	}
	object, err := checkpointmodel.ObjectIDFromBytes(
		bytes.Repeat([]byte{0x70}, transfer.OwnedObjectIdentityBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := newInitialRecord(key, object)
	if err != nil {
		t.Fatal(err)
	}
	active, err := checkpointmodel.PromoteInitialCandidate(initial)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := checkpointmodel.AdvanceGeneration(
		active, []checkpointmodel.Range{{Offset: 0, End: 4}},
		checkpointmodel.PhaseActive, checkpointmodel.CommitCandidate,
	)
	if err != nil {
		t.Fatal(err)
	}
	repository.record, repository.present = candidate, true
	platform.objects[object] = &memoryOwnedData{bytes: make([]byte, 8)}

	engine := newFixtureEngine(t, fixture, repository, platform, destination, nil)
	start, err := engine.BeginFile(context.Background(), fixture.file)
	if err != nil {
		t.Fatal(err)
	}
	_, durable, ok := start.Transaction()
	if !ok || durable.Ranges().Ranges()[0] != (content.Range{Offset: 0, End: 4}) {
		t.Fatalf("candidate durable ranges = %#v", durable.Ranges().Ranges())
	}
	if repository.record.CommitState() != checkpointmodel.CommitVerified ||
		repository.record.CheckpointGeneration() != candidate.CheckpointGeneration() {
		t.Fatalf(
			"candidate promotion = commit %d generation %d",
			repository.record.CommitState(), repository.record.CheckpointGeneration(),
		)
	}
}

func TestUnknownOwnershipNeverBecomesHaveState(t *testing.T) {
	fixture := newExecutionFixture(t, 4)
	for name, final := range map[string]FinalCondition{
		"unsafe": FinalUnsafe,
	} {
		t.Run(name, func(t *testing.T) {
			repository := &memoryCheckpointRepository{}
			platform := newMemoryPlatform()
			destination := &memoryDestination{target: fixture.file.Target, final: final}
			engine := newFixtureEngine(t, fixture, repository, platform, destination, nil)
			start, err := engine.BeginFile(context.Background(), fixture.file)
			if !errors.Is(err, ErrTargetOwnershipUnknown) {
				t.Fatalf("unknown target error = %v", err)
			}
			if _, _, ok := start.Transaction(); ok || repository.present {
				t.Fatal("unknown target ownership fabricated resumable state")
			}
		})
	}

	repository := &memoryCheckpointRepository{}
	platform := newMemoryPlatform()
	destination := &memoryDestination{target: fixture.file.Target, final: FinalAbsent}
	engine := newFixtureEngine(t, fixture, repository, platform, destination, nil)
	start, err := engine.BeginFile(context.Background(), fixture.file)
	if err != nil {
		t.Fatal(err)
	}
	transaction, _, _ := start.Transaction()
	if err := transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	destination.publish = FinalUnsafe
	if _, err := transaction.Commit(context.Background()); !errors.Is(err, ErrPublicationAmbiguous) ||
		!errors.Is(err, ErrTargetOwnershipUnknown) {
		t.Fatalf("unknown publication error = %v", err)
	}
	if repository.record.Phase() != checkpointmodel.PhasePublishing {
		t.Fatalf("unknown publication phase = %d", repository.record.Phase())
	}
}

func TestFileTransactionRejectsOverlapsBoundsAndClosedReuse(t *testing.T) {
	fixture := newExecutionFixture(t, 4)
	repository := &memoryCheckpointRepository{}
	platform := newMemoryPlatform()
	destination := &memoryDestination{target: fixture.file.Target, final: FinalAbsent}
	engine := newFixtureEngine(t, fixture, repository, platform, destination, nil)
	start, err := engine.BeginFile(context.Background(), fixture.file)
	if err != nil {
		t.Fatal(err)
	}
	transaction, _, _ := start.Transaction()
	if err := transaction.WriteRange(context.Background(), 0, []byte("ab")); err != nil {
		t.Fatal(err)
	}
	if err := transaction.WriteRange(context.Background(), 1, []byte("x")); !errors.Is(err, ErrRangeOverlap) {
		t.Fatalf("overlap error = %v", err)
	}
	if err := transaction.WriteRange(context.Background(), 4, []byte("x")); !errors.Is(err, ErrRangeOutOfBounds) {
		t.Fatalf("bounds error = %v", err)
	}
	if _, err := transaction.Commit(context.Background()); !errors.Is(err, ErrIncompleteFile) {
		t.Fatalf("incomplete commit error = %v", err)
	}
	if _, err := transaction.Pause(context.Background(), transfer.FilePauseInterrupted); err != nil {
		t.Fatal(err)
	}
	if err := transaction.WriteRange(context.Background(), 2, []byte("cd")); !errors.Is(err, ErrTransactionClosed) {
		t.Fatalf("closed transaction error = %v", err)
	}
}

func TestRecoveryReducerCoversClosedOwnershipOutcomes(t *testing.T) {
	fixture := newExecutionFixture(t, 4)
	engine := newFixtureEngine(
		t, fixture, &memoryCheckpointRepository{}, newMemoryPlatform(),
		&memoryDestination{target: fixture.file.Target, final: FinalAbsent}, nil,
	)
	key, err := engine.checkpointKey(fixture.file)
	if err != nil {
		t.Fatal(err)
	}
	object, _ := checkpointmodel.ObjectIDFromBytes(bytes.Repeat([]byte{0x61}, transfer.OwnedObjectIdentityBytes))
	candidate, err := newInitialRecord(key, object)
	if err != nil {
		t.Fatal(err)
	}
	active, err := checkpointmodel.PromoteInitialCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	paused, _ := pauseRecord(active)
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
	publishing, _ := publishingRecord(fullActive)
	published, _ := publishedRecord(publishing)
	retired, _ := retiredRecord(active, checkpointmodel.RetirementIsolatedFailure)
	quarantined, _ := quarantineRecord(active, checkpointmodel.QuarantineAnchorMissing)

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
	for name, test := range map[string]struct {
		record checkpointmodel.Record
		owned  OwnedObservation
		final  FinalObservation
		want   RecoveryAction
	}{
		"active":              {active, owned(OwnedReady), final(FinalAbsent), RecoveryOpenActive},
		"paused":              {paused, owned(OwnedReady), final(FinalAbsent), RecoveryActivate},
		"publishing-retry":    {publishing, owned(OwnedReady), final(FinalAbsent), RecoveryRetryPublication},
		"publishing-blocked":  {publishing, owned(OwnedReady), final(FinalCollision), RecoveryPublishBlocked},
		"publishing-complete": {publishing, owned(OwnedReady), final(FinalOwnedExact), RecoveryCompletePublication},
		"published":           {published, owned(OwnedStageMissing), final(FinalOwnedExact), RecoveryReturnPublished},
		"retired":             {retired, owned(OwnedAbsent), final(FinalAbsent), RecoveryReturnRetired},
		"quarantined":         {quarantined, owned(OwnedReady), final(FinalAbsent), RecoveryReturnQuarantined},
		"unknown":             {active, owned(OwnedReady), final(FinalUnsafe), RecoveryNeedsAttention},
		"anchor-missing":      {active, owned(OwnedAnchorMissing), final(FinalAbsent), RecoveryInstallQuarantine},
	} {
		t.Run(name, func(t *testing.T) {
			decision, err := ReduceRecovery(test.record, test.owned, test.final)
			if err != nil || decision.Action() != test.want {
				t.Fatalf("decision = (%d, %v), want %d", decision.Action(), err, test.want)
			}
		})
	}
	if _, err := NewOwnedObservation(checkpointmodel.ObjectID{}, OwnedReady); !errors.Is(err, ErrInvalidObservation) {
		t.Fatalf("zero owned observation error = %v", err)
	}
	if _, err := ObserveFinal(0); !errors.Is(err, ErrInvalidObservation) {
		t.Fatalf("zero final observation error = %v", err)
	}
	if _, err := ReduceRecovery(active, OwnedObservation{}, final(FinalAbsent)); !errors.Is(err, ErrInvalidObservation) {
		t.Fatalf("invalid reducer observation error = %v", err)
	}
}

func TestEngineRejectsInvalidConfigurationAndExhaustedObjectAllocation(t *testing.T) {
	fixture := newExecutionFixture(t, 1)
	if _, err := New(Config{}); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("zero configuration error = %v", err)
	}
	repository := &memoryCheckpointRepository{}
	platform := newMemoryPlatform()
	platform.createCollisions = MaximumObjectAllocationAttempts
	destination := &memoryDestination{target: fixture.file.Target, final: FinalAbsent}
	engine := newFixtureEngine(t, fixture, repository, platform, destination, nil)
	if _, err := engine.BeginFile(context.Background(), fixture.file); !errors.Is(err, ErrObjectAllocation) {
		t.Fatalf("allocation exhaustion error = %v", err)
	}
	if repository.present {
		t.Fatal("allocation exhaustion installed a checkpoint")
	}
}

func TestBeginExistingReducesTerminalAndRecoveryCuts(t *testing.T) {
	fixture := newExecutionFixture(t, 4)
	seedRepository := &memoryCheckpointRepository{}
	seedPlatform := newMemoryPlatform()
	seedDestination := &memoryDestination{target: fixture.file.Target, final: FinalAbsent}
	seedEngine := newFixtureEngine(t, fixture, seedRepository, seedPlatform, seedDestination, nil)
	key, err := seedEngine.checkpointKey(fixture.file)
	if err != nil {
		t.Fatal(err)
	}
	object, _ := checkpointmodel.ObjectIDFromBytes(bytes.Repeat([]byte{0x71}, transfer.OwnedObjectIdentityBytes))
	candidate, _ := newInitialRecord(key, object)
	active, _ := checkpointmodel.PromoteInitialCandidate(candidate)
	fullCandidate, _ := checkpointmodel.AdvanceGeneration(
		active, []checkpointmodel.Range{{Offset: 0, End: 4}},
		checkpointmodel.PhaseActive, checkpointmodel.CommitCandidate,
	)
	fullActive, _ := checkpointmodel.Promote(
		fullCandidate, checkpointmodel.PhaseActive, checkpointmodel.CommitVerified,
	)
	publishing, _ := publishingRecord(fullActive)
	published, _ := publishedRecord(publishing)
	retired, _ := retiredRecord(active, checkpointmodel.RetirementInvalidatedRevision)
	quarantined, _ := quarantineRecord(active, checkpointmodel.QuarantineStageUnsafe)

	for name, test := range map[string]struct {
		record checkpointmodel.Record
		owned  OwnedCondition
		final  FinalCondition
		want   transfer.FileSettlementKind
	}{
		"complete-publication": {publishing, OwnedReady, FinalOwnedExact, transfer.FilePublished},
		"publication-blocked":  {publishing, OwnedReady, FinalCollision, transfer.FilePublishBlocked},
		"published":            {published, OwnedStageMissing, FinalOwnedExact, transfer.FilePublished},
		"retired":              {retired, OwnedAbsent, FinalAbsent, transfer.FileRetired},
		"quarantined":          {quarantined, OwnedReady, FinalAbsent, transfer.FileQuarantined},
		"install-quarantine":   {active, OwnedAnchorMissing, FinalAbsent, transfer.FileQuarantined},
	} {
		t.Run(name, func(t *testing.T) {
			repository := &memoryCheckpointRepository{record: test.record, present: true}
			platform := newMemoryPlatform()
			platform.objects[object] = &memoryOwnedData{bytes: make([]byte, 4)}
			platform.openCondition = test.owned
			destination := &memoryDestination{target: fixture.file.Target, final: test.final}
			engine := newFixtureEngine(t, fixture, repository, platform, destination, nil)
			start, err := engine.BeginFile(context.Background(), fixture.file)
			if err != nil {
				t.Fatal(err)
			}
			settlement, ok := start.ImmediateSettlement()
			if !ok || settlement.Kind() != test.want {
				t.Fatalf("settlement = (%d, %t), want %d", settlement.Kind(), ok, test.want)
			}
		})
	}
}

func TestExecutionValueObjectsAndFaultBoundariesAreClosed(t *testing.T) {
	fixture := newExecutionFixture(t, 2)
	engine := newFixtureEngine(
		t, fixture, &memoryCheckpointRepository{}, newMemoryPlatform(),
		&memoryDestination{target: fixture.file.Target, final: FinalAbsent}, nil,
	)
	key, err := engine.checkpointKey(fixture.file)
	if err != nil {
		t.Fatal(err)
	}
	if key.OperationID() != fixture.intent.OperationID() ||
		key.ReceiveIntentDigest() != fixture.intent.Digest() ||
		key.MaterializationBindingDigest() != fixture.intent.BindingDigest() ||
		key.FileID() != fixture.file.Descriptor.FileID() ||
		key.FileRevision() != fixture.file.Descriptor.FileRevision() ||
		key.CanonicalPath() != fixture.file.Path || key.ExactSize() != 2 ||
		key.MaterializerKind() != checkpointmodel.MaterializerNativeTree ||
		key.AuthorityRef() != fixture.ownership.AuthorityRef() {
		t.Fatal("checkpoint key accessors lost an immutable binding")
	}
	if _, present := MissingCheckpoint().Record(); present {
		t.Fatal("missing checkpoint projected a record")
	}
	object, _ := checkpointmodel.ObjectIDFromBytes(bytes.Repeat([]byte{0x81}, transfer.OwnedObjectIdentityBytes))
	owned, err := NewOwnedObservation(object, OwnedReady)
	if err != nil || owned.ObjectID() != object || owned.Condition() != OwnedReady {
		t.Fatalf("owned observation = %+v, %v", owned, err)
	}
	identity, _ := transfer.OwnedObjectIDFromBytes(object.Bytes())
	expectation, err := NewFinalExpectation(identity, 2, catalog.ModifiedTime{})
	if err != nil || expectation.ObjectIdentity() != identity || expectation.ExactSize() != 2 ||
		expectation.ModifiedTime().Present() {
		t.Fatalf("final expectation = %+v, %v", expectation, err)
	}
	if _, err := NewFinalExpectation(transfer.OwnedObjectID{}, 2, catalog.ModifiedTime{}); !errors.Is(err, ErrInvalidObservation) {
		t.Fatalf("invalid expectation error = %v", err)
	}
	if !errors.Is(checkpointBindingError(ErrCheckpointBinding), ErrCheckpointBinding) ||
		!errors.Is(checkpointInstallError(ErrCheckpointNotInstalled), ErrCheckpointNotInstalled) {
		t.Fatal("checkpoint faults lost their stable cause")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := collaboratorError(cancelled, errors.New("late failure")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled collaborator error = %v", err)
	}
	if collaboratorError(context.Background(), nil) != nil {
		t.Fatal("nil collaborator failure became an error")
	}
	for _, reason := range []checkpointmodel.QuarantineReason{
		checkpointmodel.QuarantinePublicationHistory,
		checkpointmodel.QuarantinePartialObjectCreation,
		checkpointmodel.QuarantineUpdateTemporary,
		checkpointmodel.QuarantineAnchorMissing,
	} {
		if transferQuarantineReason(reason) == 0 {
			t.Fatalf("quarantine reason %d has no transfer projection", reason)
		}
	}
}

func slicesEqual[T comparable](left, right []T) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
