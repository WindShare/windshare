package fileexecution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
)

type fixtureIdentity interface {
	~[catalog.IdentityBytes]byte
}

func fixtureID[T fixtureIdentity](seed byte) T {
	var value T
	for index := range value {
		value[index] = seed + byte(index)
	}
	return value
}

type claimPorts struct {
	mu    sync.Mutex
	claim outputsession.FileClaim
}

func (*claimPorts) CanonicalLocatorKey(path string) (string, error) {
	if path == "" {
		return "locator:root", nil
	}
	return "locator:" + path, nil
}

func (*claimPorts) MaterializeDirectory(
	context.Context,
	outputsession.DirectoryClaim,
) (outputsession.DirectoryMaterialization, error) {
	return outputsession.DirectoryMaterialization{
		Cut: outputsession.MutationStable, Disposition: outputsession.DirectoryCallerProvidedRoot,
	}, nil
}

func (*claimPorts) FinalizeDirectory(
	context.Context,
	outputsession.DirectoryClaim,
) (outputsession.DirectoryFinalization, error) {
	return outputsession.FinalizedDirectory(), nil
}

func (ports *claimPorts) BeginFile(
	_ context.Context,
	claim outputsession.FileClaim,
) (outputsession.FileBeginObservation, error) {
	ports.mu.Lock()
	ports.claim = claim
	ports.mu.Unlock()
	settlement, err := transfer.NewCollisionFileSettlement(claim.File().Target)
	return outputsession.FileBeginObservation{
		Cut: outputsession.MutationStable, Settlement: settlement,
	}, err
}

func (*claimPorts) ReleaseOutputSession(context.Context) error { return nil }

type engineFixture struct {
	intent    transfer.TransferIntent
	ownership checkpointmodel.Ownership
	sessionID transfer.OutputSessionID
	claim     outputsession.FileClaim
	file      transfer.OutputFile
}

func newEngineFixture(t *testing.T, exactSize uint64) engineFixture {
	t.Helper()
	share := fixtureID[catalog.ShareInstance](1)
	root := fixtureID[catalog.DirectoryID](21)
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.NewFilesystemTransferIntent(
		share, root, rules, filepath.Join(t.TempDir(), "output"),
		transfer.NativeFilesystemOutputBackendID, transfer.OutputNativeTree,
	)
	if err != nil {
		t.Fatal(err)
	}
	rootIdentity := bytes.Repeat([]byte{0x51}, sha256.Size)
	ownership, err := checkpointmodel.NewOwnership(checkpointmodel.OwnershipSpec{
		Backend:             intent.BackendID(),
		Certification:       checkpointmodel.CertificationWindowsNTFSProcessRestart,
		RootIdentity:        rootIdentity,
		RootOpenDisposition: checkpointmodel.CallerProvidedContainer,
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := fixtureID[transfer.OutputSessionID](61)
	geometry, err := content.NewFileGeometry(exactSize, catalog.MinChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(
		share, fixtureID[catalog.FileID](31), fixtureID[content.FileRevision](41),
		geometry, catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	path := "file.bin"
	locator, err := transfer.NewPathOutputLocator(path)
	if err != nil {
		t.Fatal(err)
	}
	target, err := transfer.NewOutputFileTarget(intent.BackendID(), sessionID, descriptor, locator)
	if err != nil {
		t.Fatal(err)
	}
	ports := &claimPorts{}
	secret := bytes.Repeat([]byte{0x91}, 32)
	session, err := outputsession.New(outputsession.Config{
		Intent: intent, SessionID: sessionID,
		Capabilities: transfer.OutputCapabilities{
			Durability: transfer.DurabilityPowerLoss, Mode: transfer.OutputNativeTree,
			RandomWrite: true, FileFailureIsolation: true, ModifiedTime: true,
		},
		ReceiptSecret: secret, Locator: ports, Directories: ports, Files: ports, Resources: ports,
	})
	if err != nil {
		t.Fatal(err)
	}
	admission, err := session.AdmitDirectory(context.Background(), transfer.OutputDirectory{
		DirectoryID: root, Generation: fixtureID[catalog.DirectoryGeneration](71), Path: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	file := transfer.OutputFile{
		Path: path, ExpectedSize: exactSize, Descriptor: descriptor,
		Target: target, ParentAdmission: admission,
	}
	if _, err := session.BeginFile(context.Background(), file); err != nil {
		t.Fatal(err)
	}
	ports.mu.Lock()
	claim := ports.claim
	ports.mu.Unlock()
	if claim.ID() == 0 {
		t.Fatal("output session did not produce an atomic file claim")
	}
	return engineFixture{
		intent: intent, ownership: ownership, sessionID: sessionID, claim: claim, file: file,
	}
}

type fakeDirectoryAuthority struct {
	mu         sync.Mutex
	namespace  *fakePublicNamespace
	claims     []outputsession.FileClaim
	bindErr    error
	wrongClaim bool
}

func (authority *fakeDirectoryAuthority) BindFile(
	_ context.Context,
	claim outputsession.FileClaim,
) (FileDestination, error) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	authority.claims = append(authority.claims, claim)
	if authority.bindErr != nil {
		return nil, authority.bindErr
	}
	claimID := claim.ID()
	if authority.wrongClaim {
		claimID++
	}
	return &fakeDestination{
		claimID: claimID, target: claim.File().Target, namespace: authority.namespace,
	}, nil
}

type fakePublicNamespace struct {
	mu                 sync.Mutex
	condition          FinalCondition
	publishObservation FinalCondition
	publishErr         error
	syncErr            error
	publishCalls       int
	syncCalls          int
	data               []byte
	modified           catalog.ModifiedTime
}

func newFakePublicNamespace() *fakePublicNamespace {
	return &fakePublicNamespace{condition: FinalAbsent}
}

type fakeDestination struct {
	claimID   outputsession.ClaimID
	target    transfer.OutputFileTarget
	namespace *fakePublicNamespace
	closed    bool
}

func (destination *fakeDestination) ClaimID() outputsession.ClaimID { return destination.claimID }
func (destination *fakeDestination) Target() transfer.OutputFileTarget {
	return destination.target
}

func (destination *fakeDestination) ObserveFinal(
	context.Context,
	FinalExpectation,
) (FinalObservation, error) {
	return destination.observe()
}

func (destination *fakeDestination) ObserveFinalPresence(context.Context) (FinalObservation, error) {
	return destination.observe()
}

func (destination *fakeDestination) observe() (FinalObservation, error) {
	destination.namespace.mu.Lock()
	defer destination.namespace.mu.Unlock()
	return ObserveFinal(destination.namespace.condition)
}

func (destination *fakeDestination) PublishNoReplace(
	_ context.Context,
	file OwnedFile,
	expectation FinalExpectation,
) (FinalObservation, error) {
	destination.namespace.mu.Lock()
	defer destination.namespace.mu.Unlock()
	destination.namespace.publishCalls++
	condition := destination.namespace.publishObservation
	if condition == 0 {
		condition = destination.namespace.condition
		if condition == FinalAbsent {
			condition = FinalOwnedExact
		}
	}
	if condition == FinalOwnedExact && destination.namespace.publishErr == nil {
		owned := file.(*fakeOwnedFile)
		owned.state.mu.Lock()
		destination.namespace.data = append([]byte(nil), owned.state.data...)
		destination.namespace.modified = owned.state.modified
		owned.state.mu.Unlock()
		destination.namespace.condition = FinalOwnedExact
	}
	observation, err := ObserveFinal(condition)
	return observation, errors.Join(err, destination.namespace.publishErr)
}

func (destination *fakeDestination) SyncFinalParent(context.Context) error {
	destination.namespace.mu.Lock()
	defer destination.namespace.mu.Unlock()
	destination.namespace.syncCalls++
	return destination.namespace.syncErr
}

func (destination *fakeDestination) Close() error {
	destination.closed = true
	return nil
}

type fakeOwnedState struct {
	mu        sync.Mutex
	object    checkpointmodel.ObjectID
	condition OwnedCondition
	data      []byte
	modified  catalog.ModifiedTime
	writeN    int
	writeErr  error
	syncErr   error
	setErr    error
	matchErr  error
}

type fakeOwnedFile struct {
	state  *fakeOwnedState
	closed bool
}

func (file *fakeOwnedFile) ObjectID() checkpointmodel.ObjectID { return file.state.object }

func (file *fakeOwnedFile) WriteAt(data []byte, offset int64) (int, error) {
	file.state.mu.Lock()
	defer file.state.mu.Unlock()
	count := len(data)
	if file.state.writeN >= 0 {
		count = file.state.writeN
		file.state.writeN = -1
	}
	if count > len(data) {
		count = len(data)
	}
	copy(file.state.data[int(offset):int(offset)+count], data[:count])
	err := file.state.writeErr
	file.state.writeErr = nil
	return count, err
}

func (file *fakeOwnedFile) Sync() error {
	file.state.mu.Lock()
	defer file.state.mu.Unlock()
	return file.state.syncErr
}

func (file *fakeOwnedFile) SetModifiedTime(modified catalog.ModifiedTime) error {
	file.state.mu.Lock()
	defer file.state.mu.Unlock()
	if file.state.setErr == nil {
		file.state.modified = modified
	}
	return file.state.setErr
}

func (file *fakeOwnedFile) MetadataMatches(size uint64, modified catalog.ModifiedTime) (bool, error) {
	file.state.mu.Lock()
	defer file.state.mu.Unlock()
	return uint64(len(file.state.data)) == size && file.state.modified == modified, file.state.matchErr
}

func (file *fakeOwnedFile) Close() error {
	file.closed = true
	return nil
}

type fakePlatform struct {
	mu              sync.Mutex
	objects         map[checkpointmodel.ObjectID]*fakeOwnedState
	createCondition OwnedCondition
	createErr       error
	openErr         error
	retirementErr   map[RetirementStep]error
	retirement      []RetirementStep
}

func newFakePlatform() *fakePlatform {
	return &fakePlatform{
		objects:       make(map[checkpointmodel.ObjectID]*fakeOwnedState),
		retirementErr: make(map[RetirementStep]error),
	}
}

func (platform *fakePlatform) CreateOwnedFile(
	_ context.Context,
	object checkpointmodel.ObjectID,
	exactSize uint64,
) (OwnedFile, OwnedObservation, error) {
	platform.mu.Lock()
	defer platform.mu.Unlock()
	if _, exists := platform.objects[object]; exists {
		observation, _ := NewOwnedObservation(object, OwnedObjectCollision)
		return nil, observation, platform.createErr
	}
	condition := platform.createCondition
	if condition == 0 {
		condition = OwnedReady
	}
	state := &fakeOwnedState{
		object: object, condition: condition, data: make([]byte, exactSize), writeN: -1,
	}
	platform.objects[object] = state
	observation, _ := NewOwnedObservation(object, condition)
	if condition != OwnedReady {
		return nil, observation, platform.createErr
	}
	return &fakeOwnedFile{state: state}, observation, platform.createErr
}

func (platform *fakePlatform) OpenOwnedFile(
	_ context.Context,
	object checkpointmodel.ObjectID,
	_ uint64,
	_ bool,
) (OwnedFile, OwnedObservation, error) {
	platform.mu.Lock()
	defer platform.mu.Unlock()
	state, exists := platform.objects[object]
	if !exists {
		observation, _ := NewOwnedObservation(object, OwnedAbsent)
		return nil, observation, platform.openErr
	}
	state.mu.Lock()
	condition := state.condition
	state.mu.Unlock()
	observation, _ := NewOwnedObservation(object, condition)
	if condition != OwnedReady {
		return nil, observation, platform.openErr
	}
	return &fakeOwnedFile{state: state}, observation, platform.openErr
}

func (platform *fakePlatform) ApplyRetirement(
	_ context.Context,
	object checkpointmodel.ObjectID,
	step RetirementStep,
) (OwnedObservation, error) {
	platform.mu.Lock()
	defer platform.mu.Unlock()
	platform.retirement = append(platform.retirement, step)
	state, exists := platform.objects[object]
	condition := OwnedAbsent
	if exists {
		state.mu.Lock()
		condition = state.condition
		if platform.retirementErr[step] == nil {
			switch step {
			case RetirementRemoveStage:
				if condition == OwnedReady {
					condition = OwnedStageMissing
				}
			case RetirementRemoveAnchor:
				if condition == OwnedStageMissing {
					condition = OwnedAbsent
				}
			}
			state.condition = condition
		}
		state.mu.Unlock()
	}
	observation, _ := NewOwnedObservation(object, condition)
	return observation, platform.retirementErr[step]
}

func (platform *fakePlatform) onlyState(t *testing.T) *fakeOwnedState {
	t.Helper()
	platform.mu.Lock()
	defer platform.mu.Unlock()
	if len(platform.objects) != 1 {
		t.Fatalf("got %d owned objects, want 1", len(platform.objects))
	}
	for _, state := range platform.objects {
		return state
	}
	return nil
}

type checkpointStoreHook func(
	*fakeCheckpointRepository,
	*checkpointmodel.Record,
	checkpointmodel.Record,
) (CheckpointObservation, error)

type fakeCheckpointRepository struct {
	mu        sync.Mutex
	present   bool
	record    checkpointmodel.Record
	stores    []checkpointmodel.Record
	hooks     []checkpointStoreHook
	lookupErr error
}

func (repository *fakeCheckpointRepository) Lookup(
	context.Context,
	CheckpointKey,
) (checkpointmodel.Record, bool, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	return repository.record, repository.present, repository.lookupErr
}

func (repository *fakeCheckpointRepository) Store(
	_ context.Context,
	previous *checkpointmodel.Record,
	next checkpointmodel.Record,
) (CheckpointObservation, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	repository.stores = append(repository.stores, next)
	if len(repository.hooks) > 0 {
		hook := repository.hooks[0]
		repository.hooks = repository.hooks[1:]
		return hook(repository, previous, next)
	}
	if previous == nil {
		if repository.present {
			observation, _ := ObservedCheckpoint(repository.record)
			return observation, errors.New("checkpoint already exists")
		}
	} else if !repository.present || !recordEqual(repository.record, *previous) {
		if !repository.present {
			return MissingCheckpoint(), errors.New("checkpoint disappeared")
		}
		observation, _ := ObservedCheckpoint(repository.record)
		return observation, errors.New("checkpoint changed")
	}
	repository.present = true
	repository.record = next
	observation, _ := ObservedCheckpoint(next)
	return observation, nil
}

func (repository *fakeCheckpointRepository) current(t *testing.T) checkpointmodel.Record {
	t.Helper()
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if !repository.present || !repository.record.Valid() {
		t.Fatal("checkpoint is absent")
	}
	return repository.record
}

func deterministicRandom(seed byte) io.Reader {
	data := make([]byte, transfer.OutputObjectIdentityBytes*MaximumObjectAllocationAttempts)
	for index := range data {
		data[index] = seed + byte(index%transfer.OutputObjectIdentityBytes)
	}
	return bytes.NewReader(data)
}

func newTestEngine(
	t *testing.T,
	fixture engineFixture,
	directories *fakeDirectoryAuthority,
	platform *fakePlatform,
	repository *fakeCheckpointRepository,
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

func beginTransaction(t *testing.T, engine *Engine, claim outputsession.FileClaim) *Transaction {
	t.Helper()
	observation, err := engine.BeginFile(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	}
	transaction, ok := observation.Transaction.(*Transaction)
	if !ok || observation.Cut != outputsession.MutationStable {
		t.Fatalf("unexpected begin observation: %#v", observation)
	}
	return transaction
}
