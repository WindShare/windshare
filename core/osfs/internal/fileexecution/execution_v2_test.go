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
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type executionFixture struct {
	intent      transfer.ReceiveIntent
	ownership   checkpointmodel.Ownership
	session     transfer.OutputSessionID
	file        transfer.MaterializationFile
	destination transfer.OutputDestinationPath
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
	sourcePath, err := ordinaryoutput.NewSourceCatalogPath("file.bin")
	if err != nil {
		t.Fatal(err)
	}
	destinationPath, err := transfer.NewOutputDestinationPath("file.bin")
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
	scope, err := transfer.NewDirectoryAdmissionScope(intent)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := transfer.NewDirectoryAdmissionWithSecret(
		bytes.Repeat([]byte{0x25}, 32),
		scope,
		transfer.AuthenticatedSourceDirectory{
			DirectoryID: root, Generation: generation,
			SourcePath: ordinaryoutput.EmptySourceCatalogPath(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	projector, err := transfer.OrdinaryOutputArtifactPathProjector(intent)
	if err != nil {
		t.Fatal(err)
	}
	file, err := transfer.NewMaterializationFile(
		projector, sourcePath, descriptor, session, admission, transfer.MaterializedDirectoryClaim{},
	)
	if err != nil {
		t.Fatal(err)
	}
	return executionFixture{
		intent: intent, ownership: ownership, session: session,
		file: file, destination: destinationPath,
	}
}

type memoryCheckpointRepository struct {
	record        checkpointmodel.Record
	present       bool
	decision      checkpointmodel.CheckpointLineageDecision
	stores        []checkpointmodel.Record
	lookupErr     error
	storeErr      error
	installErrors []error
}

func (repository *memoryCheckpointRepository) Lookup(
	context.Context,
	CheckpointKey,
) (CheckpointResolution, error) {
	if repository.lookupErr != nil {
		return CheckpointResolution{}, repository.lookupErr
	}
	if repository.decision.Valid() && repository.decision != checkpointmodel.CheckpointLineageDecisionExact {
		return ResolveCheckpoint(repository.decision, checkpointmodel.Record{})
	}
	if !repository.present {
		return ResolveCheckpoint(checkpointmodel.CheckpointLineageDecisionAbsent, checkpointmodel.Record{})
	}
	return ResolveCheckpoint(checkpointmodel.CheckpointLineageDecisionExact, repository.record)
}

func (repository *memoryCheckpointRepository) InstallInitial(
	_ context.Context,
	_ CheckpointKey,
	next checkpointmodel.Record,
) (InitialCheckpointObservation, error) {
	if len(repository.installErrors) != 0 {
		err := repository.installErrors[0]
		repository.installErrors = repository.installErrors[1:]
		return InitialCheckpointObservation{}, err
	}
	if repository.storeErr != nil {
		if repository.present {
			resolution, _ := ResolveCheckpoint(checkpointmodel.CheckpointLineageDecisionExact, repository.record)
			observed, _ := ObserveInitialCheckpoint(resolution, false)
			return observed, repository.storeErr
		}
		return InitialCheckpointObservation{}, repository.storeErr
	}
	if repository.decision.Valid() && repository.decision != checkpointmodel.CheckpointLineageDecisionAbsent {
		resolution, _ := ResolveCheckpoint(repository.decision, checkpointmodel.Record{})
		return ObserveInitialCheckpoint(resolution, false)
	}
	if repository.present {
		resolution, _ := ResolveCheckpoint(checkpointmodel.CheckpointLineageDecisionExact, repository.record)
		return ObserveInitialCheckpoint(resolution, false)
	}
	repository.record, repository.present = next, true
	repository.stores = append(repository.stores, next)
	resolution, err := ResolveCheckpoint(checkpointmodel.CheckpointLineageDecisionExact, next)
	if err != nil {
		return InitialCheckpointObservation{}, err
	}
	return ObserveInitialCheckpoint(resolution, true)
}

func (repository *memoryCheckpointRepository) Replace(
	_ context.Context,
	previous checkpointmodel.Record,
	next checkpointmodel.Record,
) (CheckpointObservation, error) {
	if repository.storeErr != nil {
		if repository.present {
			observed, _ := ObservedCheckpoint(repository.record)
			return observed, repository.storeErr
		}
		return MissingCheckpoint(), repository.storeErr
	}
	if !repository.present || !recordEqual(repository.record, previous) {
		if !repository.present {
			return MissingCheckpoint(), nil
		}
		observed, _ := ObservedCheckpoint(repository.record)
		return observed, nil
	}
	repository.record = next
	repository.stores = append(repository.stores, next)
	return ObservedCheckpoint(next)
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
	closeErr    error
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
	if file.metadataErr != nil {
		return file.metadataErr
	}
	file.data.modified = value
	return nil
}
func (file *memoryOwnedFile) MetadataMatches(size uint64, modified catalog.ModifiedTime) (bool, error) {
	return uint64(len(file.data.bytes)) == size && file.data.modified == modified, file.metadataErr
}
func (file *memoryOwnedFile) Close() error {
	file.closed = true
	return file.closeErr
}

type memoryPlatform struct {
	objects          map[checkpointmodel.ObjectID]*memoryOwnedData
	createCollisions int
	openCondition    OwnedCondition
	observeCalls     int
	createCalls      int
	openCalls        int
	retirements      []RetirementStep
	retirementErr    error
}

func newMemoryPlatform() *memoryPlatform {
	return &memoryPlatform{objects: make(map[checkpointmodel.ObjectID]*memoryOwnedData)}
}

func (platform *memoryPlatform) ObserveOwnedObject(
	_ context.Context,
	object checkpointmodel.ObjectID,
	_ uint64,
) (OwnedObservation, error) {
	platform.observeCalls++
	condition := OwnedAbsent
	if platform.createCollisions > 0 {
		platform.createCollisions--
		condition = OwnedObjectCollision
	} else if platform.objects[object] != nil {
		condition = OwnedReady
	}
	observation, _ := NewOwnedObservation(object, condition)
	return observation, nil
}

func (platform *memoryPlatform) CreateOwnedFile(
	_ context.Context,
	_ FileDestination,
	object checkpointmodel.ObjectID,
	size uint64,
) (OwnedFile, OwnedObservation, error) {
	platform.createCalls++
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
	platform.openCalls++
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
	closeErr   error
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
	return destination.closeErr
}

type memoryDirectoryAuthority struct {
	destination *memoryDestination
	err         error
}

func (authority *memoryDirectoryAuthority) BindFile(
	context.Context,
	transfer.MaterializationFile,
	transfer.OutputDestinationPath,
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
	destination := &memoryDestination{target: fixture.file.Target(), final: FinalAbsent}
	var traces []TraceEvent
	engine := newFixtureEngine(t, fixture, repository, platform, destination, TraceSinkFunc(func(event TraceEvent) {
		traces = append(traces, event)
	}))

	start, err := engine.BeginFile(context.Background(), fixture.file, fixture.destination)
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
	if len(platform.retirements) != 0 {
		t.Fatalf("published witness was retired before operation settlement: %v", platform.retirements)
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
	destination := &memoryDestination{target: fixture.file.Target(), final: FinalAbsent}
	engine := newFixtureEngine(t, fixture, repository, platform, destination, nil)
	start, err := engine.BeginFile(context.Background(), fixture.file, fixture.destination)
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
	reopened, err := reopenedEngine.BeginFile(context.Background(), fixture.file, fixture.destination)
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
	if err != nil || retired.Kind() != transfer.FileFailed {
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
	destination := &memoryDestination{target: fixture.file.Target(), final: FinalAbsent}
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
	start, err := engine.BeginFile(context.Background(), fixture.file, fixture.destination)
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
			destination := &memoryDestination{target: fixture.file.Target(), final: final}
			engine := newFixtureEngine(t, fixture, repository, platform, destination, nil)
			start, err := engine.BeginFile(context.Background(), fixture.file, fixture.destination)
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
	destination := &memoryDestination{target: fixture.file.Target(), final: FinalAbsent}
	engine := newFixtureEngine(t, fixture, repository, platform, destination, nil)
	start, err := engine.BeginFile(context.Background(), fixture.file, fixture.destination)
	if err != nil {
		t.Fatal(err)
	}
	transaction, _, _ := start.Transaction()
	if err := transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	destination.publish = FinalUnsafe
	settlement, err := transaction.Commit(context.Background())
	if err != nil || settlement.Kind() != transfer.FileItemBlocked {
		t.Fatalf("unknown publication settlement = (%d, %v)", settlement.Kind(), err)
	}
	reference, reason, blocked := settlement.ItemBlock()
	if !blocked || reference.IsZero() || reason != transfer.ItemBlockPublicationAmbiguous ||
		repository.record.Phase() != checkpointmodel.PhaseQuarantined ||
		repository.record.QuarantineReason() != checkpointmodel.QuarantineFinalUnsafe {
		t.Fatalf("unknown publication evidence = (blocked %t reason %d phase %d quarantine %d)",
			blocked, reason, repository.record.Phase(), repository.record.QuarantineReason())
	}
}

func TestFileTransactionRejectsOverlapsBoundsAndClosedReuse(t *testing.T) {
	fixture := newExecutionFixture(t, 4)
	repository := &memoryCheckpointRepository{}
	platform := newMemoryPlatform()
	destination := &memoryDestination{target: fixture.file.Target(), final: FinalAbsent}
	engine := newFixtureEngine(t, fixture, repository, platform, destination, nil)
	start, err := engine.BeginFile(context.Background(), fixture.file, fixture.destination)
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

func TestBeginExistingMissingPartialKeepsAuthorityAndBlocksItem(t *testing.T) {
	fixture := newExecutionFixture(t, 4)
	repository := &memoryCheckpointRepository{}
	platform := newMemoryPlatform()
	destination := &memoryDestination{target: fixture.file.Target(), final: FinalAbsent}
	engine := newFixtureEngine(t, fixture, repository, platform, destination, nil)
	key, err := engine.checkpointKey(fixture.file)
	if err != nil {
		t.Fatal(err)
	}
	oldObject, _ := checkpointmodel.ObjectIDFromBytes(bytes.Repeat([]byte{0x71}, transfer.OwnedObjectIdentityBytes))
	candidate, _ := newInitialRecord(key, oldObject)
	oldRecord, _ := checkpointmodel.PromoteInitialCandidate(candidate)
	repository.record, repository.present = oldRecord, true
	platform.openCondition = OwnedStageMissing

	start, err := engine.BeginFile(context.Background(), fixture.file, fixture.destination)
	if err != nil {
		t.Fatal(err)
	}
	settlement, ok := start.ImmediateSettlement()
	if !ok || settlement.Kind() != transfer.FileItemBlocked {
		t.Fatalf("missing partial settlement = (%t, %d)", ok, settlement.Kind())
	}
	if _, reason, blocked := settlement.ItemBlock(); !blocked || reason != transfer.ItemBlockOwnedObjectUnknown {
		t.Fatalf("missing partial block = (%d, %t)", reason, blocked)
	}
	if repository.record.RecordID() != oldRecord.RecordID() {
		t.Fatal("missing partial replaced its durable authority")
	}
	if _, found := platform.objects[oldObject]; found {
		t.Fatal("recovery manufactured ownership of the missing old object")
	}
}

func TestCheckpointConflictsBlockBeforeDestinationOrObjectObservation(t *testing.T) {
	fixture := newExecutionFixture(t, 4)
	for name, test := range map[string]struct {
		decision checkpointmodel.CheckpointLineageDecision
		reason   transfer.ItemBlockReason
	}{
		"revision":  {checkpointmodel.CheckpointLineageDecisionRevisionConflict, transfer.ItemBlockRevisionConflict},
		"ownership": {checkpointmodel.CheckpointLineageDecisionOwnershipConflict, transfer.ItemBlockOwnedObjectUnknown},
		"invalid":   {checkpointmodel.CheckpointLineageDecisionInvalid, transfer.ItemBlockCheckpointInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			repository := &memoryCheckpointRepository{decision: test.decision}
			platform := newMemoryPlatform()
			destination := &memoryDestination{target: fixture.file.Target(), final: FinalAbsent}
			var traces []TraceEvent
			engine := newFixtureEngine(t, fixture, repository, platform, destination, TraceSinkFunc(func(event TraceEvent) {
				traces = append(traces, event)
			}))
			start, err := engine.BeginFile(context.Background(), fixture.file, fixture.destination)
			settlement, immediate := start.ImmediateSettlement()
			_, reason, blocked := settlement.ItemBlock()
			if err != nil || !immediate || !blocked || reason != test.reason {
				t.Fatalf("conflict settlement = (%t, %t, %d, %v)", immediate, blocked, reason, err)
			}
			if platform.observeCalls != 0 || platform.createCalls != 0 || platform.openCalls != 0 {
				t.Fatalf("conflict touched owned state: observe=%d create=%d open=%d",
					platform.observeCalls, platform.createCalls, platform.openCalls)
			}
			if len(traces) != 1 || traces[0].Decision != test.decision || traces[0].Operation != TraceCheckpoint {
				t.Fatalf("conflict trace = %+v", traces)
			}
		})
	}
}

func TestInitialCandidateRecreatesSameObjectAfterPreObjectCrash(t *testing.T) {
	fixture := newExecutionFixture(t, 4)
	repository := &memoryCheckpointRepository{}
	platform := newMemoryPlatform()
	destination := &memoryDestination{target: fixture.file.Target(), final: FinalAbsent}
	engine := newFixtureEngine(t, fixture, repository, platform, destination, nil)
	key, err := engine.checkpointKey(fixture.file)
	if err != nil {
		t.Fatal(err)
	}
	object, _ := checkpointmodel.ObjectIDFromBytes(bytes.Repeat([]byte{0x71}, transfer.OwnedObjectIdentityBytes))
	candidate, err := newInitialRecord(key, object)
	if err != nil {
		t.Fatal(err)
	}
	repository.record, repository.present = candidate, true
	platform.openCondition = OwnedAbsent

	start, err := engine.BeginFile(context.Background(), fixture.file, fixture.destination)
	transaction, _, hasTransaction := start.Transaction()
	if err != nil || !hasTransaction {
		t.Fatalf("candidate recovery = (transaction=%t, %v)", hasTransaction, err)
	}
	if transaction.Binding().ObjectIdentity().Bytes()[0] != 0x71 || platform.objects[object] == nil ||
		repository.record.RecordID() != candidate.RecordID() ||
		repository.record.CommitState() != checkpointmodel.CommitVerified {
		t.Fatalf("candidate authority changed: object=%x record=%+v", transaction.Binding().ObjectIdentity().Bytes(), repository.record)
	}
}

func TestRecoveryReducerCoversClosedOwnershipOutcomes(t *testing.T) {
	fixture := newExecutionFixture(t, 4)
	engine := newFixtureEngine(
		t, fixture, &memoryCheckpointRepository{}, newMemoryPlatform(),
		&memoryDestination{target: fixture.file.Target(), final: FinalAbsent}, nil,
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
		"active":               {active, owned(OwnedReady), final(FinalAbsent), RecoveryOpenActive},
		"paused":               {paused, owned(OwnedReady), final(FinalAbsent), RecoveryActivate},
		"publishing-retry":     {publishing, owned(OwnedReady), final(FinalAbsent), RecoveryRetryPublication},
		"publishing-collision": {publishing, owned(OwnedReady), final(FinalCollision), RecoveryReturnCollision},
		"publishing-complete":  {publishing, owned(OwnedReady), final(FinalOwnedExact), RecoveryCompletePublication},
		"published":            {published, owned(OwnedStageMissing), final(FinalOwnedExact), RecoveryReturnPublished},
		"retired":              {retired, owned(OwnedAbsent), final(FinalAbsent), RecoveryReturnRetired},
		"quarantined":          {quarantined, owned(OwnedReady), final(FinalAbsent), RecoveryReturnQuarantined},
		"unknown":              {active, owned(OwnedReady), final(FinalUnsafe), RecoveryInstallQuarantine},
		"anchor-missing":       {active, owned(OwnedAnchorMissing), final(FinalAbsent), RecoveryReturnOwnershipBlocked},
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
	destination := &memoryDestination{target: fixture.file.Target(), final: FinalAbsent}
	engine := newFixtureEngine(t, fixture, repository, platform, destination, nil)
	if _, err := engine.BeginFile(context.Background(), fixture.file, fixture.destination); !errors.Is(err, ErrObjectAllocation) {
		t.Fatalf("allocation exhaustion error = %v", err)
	}
	if repository.present {
		t.Fatal("allocation exhaustion installed a checkpoint")
	}
}

func TestEngineHandlesProspectiveCheckpointAdmissionWithoutObjectMutation(t *testing.T) {
	fixture := newExecutionFixture(t, 1)

	t.Run("claimed proposal retries a new object", func(t *testing.T) {
		repository := &memoryCheckpointRepository{installErrors: []error{ErrCheckpointObjectClaimed}}
		platform := newMemoryPlatform()
		destination := &memoryDestination{target: fixture.file.Target(), final: FinalAbsent}
		engine := newFixtureEngine(t, fixture, repository, platform, destination, nil)
		start, err := engine.BeginFile(context.Background(), fixture.file, fixture.destination)
		if err != nil {
			t.Fatal(err)
		}
		transaction, _, ok := start.Transaction()
		if !ok {
			t.Fatal("retry did not start a file transaction")
		}
		want, err := checkpointmodel.ObjectIDFromBytes(bytes.Repeat(
			[]byte{0x02}, transfer.OwnedObjectIdentityBytes,
		))
		if err != nil {
			t.Fatal(err)
		}
		if repository.record.OwnedObjectID() != want || platform.observeCalls != 2 ||
			platform.createCalls != 1 || len(platform.objects) != 1 {
			t.Fatalf("proposal retry = (object=%x observe=%d create=%d objects=%d)",
				repository.record.OwnedObjectID(), platform.observeCalls, platform.createCalls, len(platform.objects))
		}
		if transaction.Binding().ObjectIdentity().Bytes()[0] != 0x02 {
			t.Fatalf("transaction used rejected proposal: %x", transaction.Binding().ObjectIdentity().Bytes())
		}
	})

	t.Run("capacity failure creates no object", func(t *testing.T) {
		repository := &memoryCheckpointRepository{installErrors: []error{ErrCheckpointRecordCapacity}}
		platform := newMemoryPlatform()
		destination := &memoryDestination{target: fixture.file.Target(), final: FinalAbsent}
		engine := newFixtureEngine(t, fixture, repository, platform, destination, nil)
		if _, err := engine.BeginFile(context.Background(), fixture.file, fixture.destination); !errors.Is(err, ErrCheckpointRecordCapacity) {
			t.Fatalf("capacity error = %v", err)
		}
		if repository.present || platform.createCalls != 0 || len(platform.objects) != 0 {
			t.Fatalf("capacity failure mutated authority: present=%t create=%d objects=%d",
				repository.present, platform.createCalls, len(platform.objects))
		}
	})
}

func TestBeginExistingReducesTerminalAndRecoveryCuts(t *testing.T) {
	fixture := newExecutionFixture(t, 4)
	seedRepository := &memoryCheckpointRepository{}
	seedPlatform := newMemoryPlatform()
	seedDestination := &memoryDestination{target: fixture.file.Target(), final: FinalAbsent}
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
		"complete-publication":  {publishing, OwnedReady, FinalOwnedExact, transfer.FilePublished},
		"publication-collision": {publishing, OwnedReady, FinalCollision, transfer.FileCollision},
		"published":             {published, OwnedStageMissing, FinalOwnedExact, transfer.FilePublished},
		"retired":               {retired, OwnedAbsent, FinalAbsent, transfer.FileFailed},
		"quarantined":           {quarantined, OwnedReady, FinalAbsent, transfer.FileItemBlocked},
	} {
		t.Run(name, func(t *testing.T) {
			repository := &memoryCheckpointRepository{record: test.record, present: true}
			platform := newMemoryPlatform()
			platform.objects[object] = &memoryOwnedData{bytes: make([]byte, 4)}
			platform.openCondition = test.owned
			destination := &memoryDestination{target: fixture.file.Target(), final: test.final}
			engine := newFixtureEngine(t, fixture, repository, platform, destination, nil)
			start, err := engine.BeginFile(context.Background(), fixture.file, fixture.destination)
			if err != nil {
				t.Fatal(err)
			}
			settlement, ok := start.ImmediateSettlement()
			if !ok || settlement.Kind() != test.want {
				t.Fatalf("settlement = (%d, %t), want %d", settlement.Kind(), ok, test.want)
			}
			if test.want == transfer.FileCollision {
				if repository.record.Phase() != checkpointmodel.PhasePaused ||
					repository.record.QuarantineReason() != 0 {
					t.Fatalf("publishing collision record = (phase %d reason %d)",
						repository.record.Phase(), repository.record.QuarantineReason())
				}
				destination.final = FinalAbsent
				retry, retryErr := engine.BeginFile(context.Background(), fixture.file, fixture.destination)
				if retryErr != nil {
					t.Fatal(retryErr)
				}
				transaction, durable, hasTransaction := retry.Transaction()
				if !hasTransaction || !transfer.RangesCoverFile(4, durable.Ranges()) {
					t.Fatalf("publishing collision retry = (transaction %t ranges %v)",
						hasTransaction, durable.Ranges().Ranges())
				}
				retriedSettlement, retryErr := transaction.Commit(context.Background())
				if retryErr != nil || retriedSettlement.Kind() != transfer.FilePublished {
					t.Fatalf("publishing collision retry commit = (%d, %v)",
						retriedSettlement.Kind(), retryErr)
				}
			}
		})
	}
}

func TestExecutionValueObjectsAndFaultBoundariesAreClosed(t *testing.T) {
	fixture := newExecutionFixture(t, 2)
	engine := newFixtureEngine(
		t, fixture, &memoryCheckpointRepository{}, newMemoryPlatform(),
		&memoryDestination{target: fixture.file.Target(), final: FinalAbsent}, nil,
	)
	key, err := engine.checkpointKey(fixture.file)
	if err != nil {
		t.Fatal(err)
	}
	if key.OperationID() != fixture.intent.OperationID() ||
		key.ReceiveIntentDigest() != fixture.intent.Digest() ||
		key.MaterializationBindingDigest() != fixture.intent.BindingDigest() ||
		key.FileID() != fixture.file.Descriptor().FileID() ||
		key.FileRevision() != fixture.file.Descriptor().FileRevision() ||
		key.CanonicalPath() != fixture.file.ArtifactPath().String() || key.ExactSize() != 2 ||
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
	expectation, err := NewFinalExpectation(identity, 2)
	if err != nil || expectation.ObjectIdentity() != identity || expectation.ExactSize() != 2 {
		t.Fatalf("final expectation = %+v, %v", expectation, err)
	}
	if _, err := NewFinalExpectation(transfer.OwnedObjectID{}, 2); !errors.Is(err, ErrInvalidObservation) {
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
