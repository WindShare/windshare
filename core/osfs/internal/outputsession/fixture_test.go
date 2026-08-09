package outputsession

import (
	"context"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type testIdentity interface {
	~[catalog.IdentityBytes]byte
}

func identity[T testIdentity](seed byte) T {
	var value T
	for index := range value {
		value[index] = seed + byte(index)
	}
	return value
}

type fakeDirectoryAuthority struct {
	mu                  sync.Mutex
	materializeCalls    []ClaimID
	finalizeCalls       []ClaimID
	materialize         func(context.Context, DirectoryClaim) (DirectoryMaterialization, error)
	finalize            func(context.Context, DirectoryClaim) (DirectoryFinalization, error)
	canonicalLocatorKey func(string) (string, error)
}

func (authority *fakeDirectoryAuthority) CanonicalLocatorKey(path string) (string, error) {
	if authority.canonicalLocatorKey != nil {
		return authority.canonicalLocatorKey(path)
	}
	if path == "" {
		return "locator:root", nil
	}
	return "locator:" + path, nil
}

func (authority *fakeDirectoryAuthority) MaterializeDirectory(
	ctx context.Context,
	claim DirectoryClaim,
) (DirectoryMaterialization, error) {
	authority.mu.Lock()
	authority.materializeCalls = append(authority.materializeCalls, claim.ID())
	operation := authority.materialize
	authority.mu.Unlock()
	if operation != nil {
		return operation(ctx, claim)
	}
	disposition := DirectoryAuthorityCreatedDescendant
	if claim.IsRoot() {
		disposition = DirectoryCallerProvidedRoot
	}
	return DirectoryMaterialization{Cut: MutationStable, Disposition: disposition}, nil
}

func (authority *fakeDirectoryAuthority) FinalizeDirectory(
	ctx context.Context,
	claim DirectoryClaim,
) (DirectoryFinalization, error) {
	authority.mu.Lock()
	authority.finalizeCalls = append(authority.finalizeCalls, claim.ID())
	operation := authority.finalize
	authority.mu.Unlock()
	if operation != nil {
		return operation(ctx, claim)
	}
	return FinalizedDirectory(), nil
}

func (authority *fakeDirectoryAuthority) counts() (int, int) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	return len(authority.materializeCalls), len(authority.finalizeCalls)
}

type fakeFileEngine struct {
	mu         sync.Mutex
	t          *testing.T
	beginCalls []ClaimID
	begin      func(context.Context, FileClaim) (FileBeginObservation, error)
	last       *fakeTransactionExecutor
}

func (engine *fakeFileEngine) BeginFile(
	ctx context.Context,
	claim FileClaim,
) (FileBeginObservation, error) {
	engine.mu.Lock()
	engine.beginCalls = append(engine.beginCalls, claim.ID())
	operation := engine.begin
	engine.mu.Unlock()
	if operation != nil {
		return operation(ctx, claim)
	}
	transaction := newFakeTransaction(engine.t, claim.File().Target)
	engine.mu.Lock()
	engine.last = transaction
	engine.mu.Unlock()
	return FileBeginObservation{
		Cut: MutationStable, Transaction: transaction, Durable: transaction.emptyCheckpoint,
	}, nil
}

func (engine *fakeFileEngine) transaction() *fakeTransactionExecutor {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	return engine.last
}

func testContext() context.Context {
	return context.Background()
}

type fakeTransactionExecutor struct {
	mu              sync.Mutex
	binding         transfer.MaterializedFileBinding
	emptyCheckpoint transfer.VerifiedDurableRanges
	fullCheckpoint  transfer.VerifiedDurableRanges
	write           func(context.Context, uint64, []byte) (MutationCut, error)
	checkpoint      func(context.Context) (transfer.VerifiedDurableRanges, MutationCut, error)
	commit          func(context.Context) (transfer.FileSettlement, MutationCut, error)
	pause           func(context.Context, transfer.FilePauseReason) (transfer.FileSettlement, MutationCut, error)
	retire          func(context.Context, transfer.FileRetireReason) (transfer.FileSettlement, MutationCut, error)
	writeCalls      int
	checkpointCalls int
	commitCalls     int
	pauseCalls      int
	retireCalls     int
}

func newFakeTransaction(t *testing.T, target transfer.FileMaterializationTarget) *fakeTransactionExecutor {
	t.Helper()
	rawIdentity := make([]byte, transfer.OwnedObjectIdentityBytes)
	for index := range rawIdentity {
		rawIdentity[index] = byte(0x80 + index)
	}
	objectIdentity, err := transfer.OwnedObjectIDFromBytes(rawIdentity)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := transfer.BindFileMaterializationTarget(target, objectIdentity)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := content.NewRangeSet(nil)
	if err != nil {
		t.Fatal(err)
	}
	emptyCheckpoint, err := transfer.VerifyDurableRanges(binding, 1, empty)
	if err != nil {
		t.Fatal(err)
	}
	fullRanges := empty
	if binding.ExactSize() != 0 {
		fullRanges, err = content.NewRangeSet([]content.Range{{Offset: 0, End: binding.ExactSize()}})
		if err != nil {
			t.Fatal(err)
		}
	}
	fullCheckpoint, err := transfer.VerifyDurableRanges(binding, 2, fullRanges)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeTransactionExecutor{
		binding: binding, emptyCheckpoint: emptyCheckpoint, fullCheckpoint: fullCheckpoint,
	}
}

func (transaction *fakeTransactionExecutor) Binding() transfer.MaterializedFileBinding {
	return transaction.binding
}

func (transaction *fakeTransactionExecutor) WriteRange(
	ctx context.Context,
	offset uint64,
	data []byte,
) (MutationCut, error) {
	transaction.mu.Lock()
	transaction.writeCalls++
	operation := transaction.write
	transaction.mu.Unlock()
	if operation != nil {
		return operation(ctx, offset, data)
	}
	return MutationStable, nil
}

func (transaction *fakeTransactionExecutor) Checkpoint(
	ctx context.Context,
) (transfer.VerifiedDurableRanges, MutationCut, error) {
	transaction.mu.Lock()
	transaction.checkpointCalls++
	operation := transaction.checkpoint
	transaction.mu.Unlock()
	if operation != nil {
		return operation(ctx)
	}
	return transaction.emptyCheckpoint, MutationStable, nil
}

func (transaction *fakeTransactionExecutor) Commit(
	ctx context.Context,
) (transfer.FileSettlement, MutationCut, error) {
	transaction.mu.Lock()
	transaction.commitCalls++
	operation := transaction.commit
	transaction.mu.Unlock()
	if operation != nil {
		return operation(ctx)
	}
	settlement, err := transfer.NewVerifiedFileSettlement(transfer.FilePublished, transaction.fullCheckpoint)
	return settlement, MutationStable, err
}

func (transaction *fakeTransactionExecutor) Pause(
	ctx context.Context,
	reason transfer.FilePauseReason,
) (transfer.FileSettlement, MutationCut, error) {
	transaction.mu.Lock()
	transaction.pauseCalls++
	operation := transaction.pause
	transaction.mu.Unlock()
	if operation != nil {
		return operation(ctx, reason)
	}
	settlement, err := transfer.NewVerifiedFileSettlement(transfer.FilePaused, transaction.emptyCheckpoint)
	return settlement, MutationStable, err
}

func (transaction *fakeTransactionExecutor) Retire(
	ctx context.Context,
	reason transfer.FileRetireReason,
) (transfer.FileSettlement, MutationCut, error) {
	transaction.mu.Lock()
	transaction.retireCalls++
	operation := transaction.retire
	transaction.mu.Unlock()
	if operation != nil {
		return operation(ctx, reason)
	}
	settlement, err := transfer.NewRetiredFileSettlement(transaction.binding)
	return settlement, MutationStable, err
}

type fakeResources struct {
	mu      sync.Mutex
	calls   int
	release func(context.Context) error
}

func (resources *fakeResources) ReleaseOutputSession(ctx context.Context) error {
	resources.mu.Lock()
	resources.calls++
	operation := resources.release
	resources.mu.Unlock()
	if operation != nil {
		return operation(ctx)
	}
	return nil
}

type testFixture struct {
	t             *testing.T
	session       *Session
	directories   *fakeDirectoryAuthority
	files         *fakeFileEngine
	resources     *fakeResources
	intent        transfer.ReceiveIntent
	sessionID     transfer.OutputSessionID
	rootDirectory transfer.MaterializationDirectory
}

func newTestFixture(t *testing.T, mutate func(*Config)) testFixture {
	t.Helper()
	share := identity[catalog.ShareInstance](1)
	root := identity[catalog.DirectoryID](21)
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := transfer.NewSelectionSpec(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	artifact := receivecontract.NewCatalogRootDirectoryTree()
	operationRaw := identity[receivecontract.OperationID](41)
	operation, err := receivecontract.OperationIDFromBytes(operationRaw[:])
	if err != nil {
		t.Fatal(err)
	}
	reservationRaw := identity[receivecontract.DestinationReservationID](51)
	reservationID, err := receivecontract.DestinationReservationIDFromBytes(reservationRaw[:])
	if err != nil {
		t.Fatal(err)
	}
	authorityRaw := make([]byte, receivecontract.AuthorityRefBytes)
	for index := range authorityRaw {
		authorityRaw[index] = byte(index + 1)
	}
	authority, err := receivecontract.AuthorityRefFromBytes(authorityRaw)
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
	sessionID := identity[transfer.OutputSessionID](61)
	directories := &fakeDirectoryAuthority{}
	files := &fakeFileEngine{t: t}
	resources := &fakeResources{}
	var secret [32]byte
	for index := range secret {
		secret[index] = byte(91 + index)
	}
	config := Config{
		Intent: intent, SessionID: sessionID,
		Capabilities: transfer.DirectTreeCapabilities{
			Durability:  transfer.DurabilityPowerLoss,
			RandomWrite: true, FileFailureIsolation: true, ModifiedTime: true,
		},
		ReceiptSecret: append([]byte(nil), secret[:]...),
		Locator:       directories, Directories: directories, Files: files, Resources: resources,
	}
	if mutate != nil {
		mutate(&config)
	}
	session, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return testFixture{
		t: t, session: session, directories: directories, files: files, resources: resources,
		intent: intent, sessionID: sessionID,
		rootDirectory: transfer.MaterializationDirectory{
			DirectoryID: root, Generation: identity[catalog.DirectoryGeneration](31), Path: "",
		},
	}
}

func (fixture testFixture) admitRoot(ctx context.Context) transfer.DirectoryAdmission {
	fixture.t.Helper()
	admission, err := fixture.session.AdmitDirectory(ctx, fixture.rootDirectory)
	if err != nil {
		fixture.t.Fatal(err)
	}
	return admission
}

func (fixture testFixture) childDirectory(
	parent transfer.DirectoryAdmission,
	seed byte,
	path string,
) transfer.MaterializationDirectory {
	return transfer.MaterializationDirectory{
		DirectoryID:     identity[catalog.DirectoryID](seed),
		Generation:      identity[catalog.DirectoryGeneration](seed + 1),
		ParentAdmission: parent,
		Path:            path,
	}
}

func (fixture testFixture) outputFile(
	parent transfer.DirectoryAdmission,
	seed byte,
	path string,
) transfer.MaterializationFile {
	fixture.t.Helper()
	geometry, err := content.NewFileGeometry(1, catalog.MinChunkSize)
	if err != nil {
		fixture.t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(
		fixture.intent.ShareInstance(), identity[catalog.FileID](seed), identity[content.FileRevision](seed+1),
		geometry, catalog.ModifiedTime{},
	)
	if err != nil {
		fixture.t.Fatal(err)
	}
	locator, err := transfer.NewPathMaterializationLocator(path)
	if err != nil {
		fixture.t.Fatal(err)
	}
	target, err := transfer.NewFileMaterializationTarget(fixture.sessionID, descriptor, locator)
	if err != nil {
		fixture.t.Fatal(err)
	}
	return transfer.MaterializationFile{
		Path: path, ExpectedSize: 1, Descriptor: descriptor, Target: target, ParentAdmission: parent,
	}
}
