package liveshare

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestFileCatalogStorageFactoryPreservesActiveRootsAndCleansAbandonedRoots(t *testing.T) {
	registry := t.TempDir()
	share := catalogStorageTestShare(1)
	var traces []CatalogStorageTrace
	factory := &fileCatalogStorageFactory{
		registry: registry,
		tracer: CatalogStorageTraceFunc(func(event CatalogStorageTrace) {
			traces = append(traces, event)
		}),
	}

	first, err := factory.Create(context.Background(), share)
	if err != nil {
		t.Fatal(err)
	}
	ownedFirst, ok := first.(*ownedCatalogBackend)
	if !ok {
		t.Fatalf("production storage = %T, want lifecycle-owned file backend", first)
	}
	if _, err := os.Stat(ownedFirst.root); err != nil {
		t.Fatalf("stat active catalog root: %v", err)
	}

	abandoned := filepath.Join(registry, liveCatalogRootPrefix+"abandoned")
	if err := os.MkdirAll(abandoned, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(abandoned, liveCatalogOwnerName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := factory.Create(context.Background(), catalogStorageTestShare(2))
	if err != nil {
		t.Fatal(err)
	}
	ownedSecond := second.(*ownedCatalogBackend)
	if _, err := os.Stat(ownedFirst.root); err != nil {
		t.Fatalf("a concurrent active catalog root was removed: %v", err)
	}
	if _, err := os.Stat(abandoned); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandoned catalog root survived cleanup: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ownedSecond.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("closed catalog root survived: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ownedFirst.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("closed active catalog root survived: %v", err)
	}

	var observedLegacyCleanup bool
	for _, event := range traces {
		if event.Operation == CatalogStorageCleaned && event.LegacyRootsRemoved == 1 && event.Cause == CatalogStorageCauseNone {
			observedLegacyCleanup = true
		}
	}
	if !observedLegacyCleanup {
		t.Fatalf("catalog storage traces did not record abandoned-root cleanup: %#v", traces)
	}
}

func TestFileCatalogStorageCloseLeavesRecoverableRootWhenRegistryLockIsUnavailable(t *testing.T) {
	registry := t.TempDir()
	share := catalogStorageTestShare(3)
	var traces []CatalogStorageTrace
	factory := &fileCatalogStorageFactory{
		registry: registry,
		tracer: CatalogStorageTraceFunc(func(event CatalogStorageTrace) {
			traces = append(traces, event)
		}),
	}
	created, err := factory.Create(context.Background(), share)
	if err != nil {
		t.Fatal(err)
	}
	owned := created.(*ownedCatalogBackend)
	lockPath := filepath.Join(registry, liveCatalogRegistryLock)
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(lockPath, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := owned.Close(); err == nil {
		t.Fatal("catalog close ignored unavailable registry ownership")
	}
	if _, err := os.Stat(owned.root); err != nil {
		t.Fatalf("failed close did not preserve a recoverable catalog root: %v", err)
	}
	last := traces[len(traces)-1]
	if last.Operation != CatalogStorageCleaned || last.ShareInstance != share || last.Cause != CatalogStorageCauseUnexpected {
		t.Fatalf("failed close trace = %#v", last)
	}

	if err := os.RemoveAll(lockPath); err != nil {
		t.Fatal(err)
	}
	replacement, err := factory.Create(context.Background(), catalogStorageTestShare(4))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(owned.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("next registry sweep did not remove the recoverable root: %v", err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestObservedCatalogBackendTracesRecoveryAndCleanupFailures(t *testing.T) {
	share := catalogStorageTestShare(9)
	recoverFailure := errors.New("recover failure")
	closeFailure := errors.New("close failure")
	backend := &catalogStorageFaultBackend{
		CatalogBackend: catalog.NewMemoryCatalogBackend(),
		recoverErr:     recoverFailure,
		closeErr:       closeFailure,
	}
	var traces []CatalogStorageTrace
	observed := &observedCatalogBackend{
		CatalogBackend: backend,
		share:          share,
		tracer: CatalogStorageTraceFunc(func(event CatalogStorageTrace) {
			traces = append(traces, event)
		}),
	}
	if _, err := observed.Recover(context.Background()); !errors.Is(err, recoverFailure) {
		t.Fatalf("recover error = %v", err)
	}
	if err := observed.Close(); !errors.Is(err, closeFailure) {
		t.Fatalf("close error = %v", err)
	}
	if len(traces) != 4 || traces[1].Operation != CatalogStorageRecovered || traces[1].Cause != CatalogStorageCauseUnexpected ||
		traces[3].Operation != CatalogStorageCleaned || traces[3].Cause != CatalogStorageCauseUnexpected {
		t.Fatalf("storage lifecycle traces = %#v", traces)
	}
}

func TestCatalogStorageTracerPanicCannotInterruptSenderLifecycle(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "selected.bin")
	if err := os.WriteFile(filename, []byte("catalog tracer authority"), 0o600); err != nil {
		t.Fatal(err)
	}
	sender, err := PrepareSender(context.Background(), SenderConfig{
		Paths: []string{filename}, Relays: []string{"ws://127.0.0.1:8484"}, ChunkSize: catalog.MinChunkSize,
		CatalogTracer: CatalogStorageTraceFunc(func(CatalogStorageTrace) {
			panic("catalog diagnostics must remain observational")
		}),
	})
	if err != nil {
		t.Fatalf("catalog tracer panic interrupted preparation: %v", err)
	}
	if err := sender.Close(); err != nil {
		t.Fatalf("catalog tracer panic interrupted cleanup: %v", err)
	}
}

func TestPrepareSenderCatalogTracesUnusableStorageRoot(t *testing.T) {
	harness := newSenderPreparationHarness(t)
	registry := filepath.Join(t.TempDir(), "catalog-registry")
	if err := os.WriteFile(registry, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	var traces []CatalogStorageTrace
	harness.config.CatalogStorage = &fileCatalogStorageFactory{registry: registry}
	harness.config.CatalogTracer = CatalogStorageTraceFunc(func(event CatalogStorageTrace) {
		traces = append(traces, event)
	})

	_, err := prepareSenderCatalog(
		context.Background(),
		harness.config,
		harness.random,
		harness.sender,
		harness.authority,
		nil,
		harness.dependencies,
	)
	if err == nil {
		t.Fatal("unusable catalog registry was accepted")
	}
	if len(traces) != 2 || traces[0].Operation != CatalogStorageCreating ||
		traces[1].Operation != CatalogStorageCreated || traces[1].Cause != CatalogStorageCauseUnexpected {
		t.Fatalf("catalog creation traces = %#v", traces)
	}
	for _, event := range traces {
		if event.ShareInstance != harness.authority.shareInstance {
			t.Fatalf("catalog creation trace lost share identity: %#v", event)
		}
	}
}

func TestPrepareSenderCatalogTracesCreationRecoveryAndBudgetRejection(t *testing.T) {
	harness := newSenderPreparationHarness(t)
	backend := &catalogStorageUsageBackend{
		CatalogBackend: catalog.NewMemoryCatalogBackend(),
		usage: catalog.ResourceUsage{
			MemoryBytes: catalog.DefaultShareCatalogMemory + 1,
		},
	}
	var traces []CatalogStorageTrace
	harness.config.CatalogStorage = CatalogStorageFactoryFunc(func(
		context.Context,
		catalog.ShareInstance,
	) (catalog.CatalogBackend, error) {
		return backend, nil
	})
	harness.config.CatalogTracer = CatalogStorageTraceFunc(func(event CatalogStorageTrace) {
		traces = append(traces, event)
	})

	_, err := prepareSenderCatalog(
		context.Background(),
		harness.config,
		harness.random,
		harness.sender,
		harness.authority,
		nil,
		harness.dependencies,
	)
	if !errors.Is(err, catalog.ErrBudgetExceeded) {
		t.Fatalf("recovered catalog budget error = %v", err)
	}
	if !backend.closed {
		t.Fatal("budget-rejected catalog backend retained ownership")
	}
	want := []CatalogStorageOperation{
		CatalogStorageCreating,
		CatalogStorageCreated,
		CatalogStorageRecovering,
		CatalogStorageRecovered,
		CatalogStorageBudgetRejected,
		CatalogStorageCleaning,
		CatalogStorageCleaned,
	}
	got := make([]CatalogStorageOperation, 0, len(traces))
	for _, event := range traces {
		if event.ShareInstance != harness.authority.shareInstance {
			t.Fatalf("trace lost share identity: %#v", event)
		}
		got = append(got, event.Operation)
	}
	if len(got) != len(want) {
		t.Fatalf("catalog storage trace operations = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("catalog storage trace operations = %v, want %v", got, want)
		}
	}
	if traces[3].Cause != CatalogStorageCauseNone ||
		traces[4].Cause != CatalogStorageCauseBudgetExceeded {
		t.Fatalf("budget failure causes = recovered %v, rejected %v", traces[3].Cause, traces[4].Cause)
	}
}

func TestCatalogStorageContractsRejectInvalidFactoriesAndAdmission(t *testing.T) {
	wantNames := []string{
		"creating", "created", "recovering", "recovered", "budget-rejected", "cleaning", "cleaned",
	}
	var gotNames []string
	for operation := CatalogStorageCreating; operation <= CatalogStorageCleaned; operation++ {
		gotNames = append(gotNames, operation.String())
	}
	if !slices.Equal(gotNames, wantNames) || CatalogStorageOperation(0).String() != "unknown" {
		t.Fatalf("storage operation names = %v", gotNames)
	}
	CatalogStorageTraceFunc(nil).TraceCatalogStorage(CatalogStorageTrace{})
	traceCatalogStorage(nil, CatalogStorageTrace{})
	if _, err := CatalogStorageFactoryFunc(nil).Create(context.Background(), catalogStorageTestShare(20)); err == nil {
		t.Fatal("nil catalog storage factory was accepted")
	}

	production := productionCatalogStorageFactory(nil)
	fileFactory, ok := production.(*fileCatalogStorageFactory)
	if !ok ||
		fileFactory.registry != filepath.Join(os.TempDir(), liveCatalogRegistryName) ||
		fileFactory.tracer != nil {
		t.Fatalf("production factory = %#v", production)
	}
	factory := &fileCatalogStorageFactory{registry: t.TempDir()}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := factory.Create(cancelled, catalogStorageTestShare(21)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled catalog creation = %v", err)
	}
	if _, err := factory.Create(context.Background(), catalog.ShareInstance{}); err == nil {
		t.Fatal("zero-share catalog creation was accepted")
	}
}

func TestFileCatalogStorageRecoverySpillAndDestroyShareOneLifecycle(t *testing.T) {
	registry := t.TempDir()
	share := catalogStorageTestShare(22)
	var traces []CatalogStorageTrace
	factory := &fileCatalogStorageFactory{
		registry: registry,
		tracer: CatalogStorageTraceFunc(func(event CatalogStorageTrace) {
			traces = append(traces, event)
		}),
	}
	created, err := factory.Create(context.Background(), share)
	if err != nil {
		t.Fatal(err)
	}
	owned := created.(*ownedCatalogBackend)
	usage, err := owned.Recover(context.Background())
	if err != nil || usage != (catalog.ResourceUsage{}) {
		t.Fatalf("empty catalog recovery = %#v, err %v", usage, err)
	}
	spillRoot := owned.CatalogSpillRoot()
	if filepath.Dir(spillRoot) != filepath.Join(owned.root, "catalog") {
		t.Fatalf("spill root %q escaped catalog lifecycle %q", spillRoot, owned.root)
	}
	if err := owned.Destroy(); err != nil {
		t.Fatal(err)
	}
	if err := owned.Close(); err != nil {
		t.Fatalf("idempotent owned close = %v", err)
	}
	if _, err := os.Stat(owned.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destroyed catalog root survived: %v", err)
	}
	want := []CatalogStorageOperation{
		CatalogStorageCleaning, CatalogStorageCleaned,
		CatalogStorageRecovering, CatalogStorageRecovered,
		CatalogStorageCleaning, CatalogStorageCleaned,
	}
	got := make([]CatalogStorageOperation, 0, len(traces))
	for _, event := range traces {
		if event.ShareInstance != share {
			t.Fatalf("trace lost share identity: %#v", event)
		}
		got = append(got, event.Operation)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("storage lifecycle traces = %v, want %v", got, want)
	}

	fallback := &ownedCatalogBackend{
		CatalogBackend: catalog.NewMemoryCatalogBackend(),
		root:           filepath.Join(t.TempDir(), "fallback-root"),
	}
	if got := fallback.CatalogSpillRoot(); got != filepath.Join(fallback.root, "sort") {
		t.Fatalf("fallback spill root = %q", got)
	}
}

func TestCatalogStorageCleanupHandlesMissingOwnersAndReportsBrokenOwners(t *testing.T) {
	registry := t.TempDir()
	factory := &fileCatalogStorageFactory{registry: registry}
	missingOwner := filepath.Join(registry, liveCatalogRootPrefix+"missing-owner")
	if err := os.MkdirAll(missingOwner, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(registry, "unrelated-file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(registry, "unrelated-directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	removed, err := factory.cleanAbandonedRoots(context.Background())
	if err != nil || removed != 1 {
		t.Fatalf("missing-owner cleanup = removed %d, err %v", removed, err)
	}
	if _, err := os.Stat(missingOwner); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ownerless root survived cleanup: %v", err)
	}

	brokenRoot := filepath.Join(registry, liveCatalogRootPrefix+"broken-owner")
	if err := os.MkdirAll(filepath.Join(brokenRoot, liveCatalogOwnerName), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := factory.cleanAbandonedRoots(context.Background()); err == nil {
		t.Fatal("broken owner entry was silently ignored")
	}
	if _, err := factory.Create(context.Background(), catalogStorageTestShare(24)); err == nil {
		t.Fatal("catalog creation continued after abandoned-root inspection failed")
	}
	if err := os.RemoveAll(brokenRoot); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := factory.cleanAbandonedRoots(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled cleanup = %v", err)
	}
	missingRegistry := &fileCatalogStorageFactory{registry: filepath.Join(t.TempDir(), "missing")}
	if _, err := missingRegistry.cleanAbandonedRoots(context.Background()); err == nil {
		t.Fatal("missing catalog registry was accepted")
	}
	blockedRegistry := t.TempDir()
	if err := os.Mkdir(filepath.Join(blockedRegistry, liveCatalogRegistryLock), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := (&fileCatalogStorageFactory{registry: blockedRegistry}).Create(
		context.Background(), catalogStorageTestShare(25),
	); err == nil {
		t.Fatal("directory-shaped registry lock was accepted")
	}

	closed, err := os.Create(filepath.Join(t.TempDir(), "closed.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if locked, err := tryLockCatalogFile(closed); err == nil || locked {
		t.Fatalf("closed owner lock = locked %v, err %v", locked, err)
	}
	if err := unlockAndCloseCatalogFile(nil); err != nil {
		t.Fatalf("nil owner unlock = %v", err)
	}
	if err := removeCatalogRoot(string([]byte{0})); err == nil {
		t.Fatal("invalid catalog root was accepted")
	}
}

func TestObservedCatalogBackendPrefersDestructiveLifecycle(t *testing.T) {
	backend := &catalogStorageDestroyBackend{CatalogBackend: catalog.NewMemoryCatalogBackend()}
	observed := &observedCatalogBackend{
		CatalogBackend: backend,
		share:          catalogStorageTestShare(23),
	}
	if err := observed.Destroy(); err != nil {
		t.Fatal(err)
	}
	if err := observed.Close(); err != nil {
		t.Fatalf("idempotent observed close = %v", err)
	}
	if !backend.destroyed {
		t.Fatal("observed backend downgraded Destroy to Close")
	}
}

type catalogStorageFaultBackend struct {
	catalog.CatalogBackend
	recoverErr error
	closeErr   error
}

func (backend *catalogStorageFaultBackend) Recover(context.Context) (catalog.ResourceUsage, error) {
	return catalog.ResourceUsage{}, backend.recoverErr
}

func (backend *catalogStorageFaultBackend) Close() error { return backend.closeErr }

type catalogStorageUsageBackend struct {
	catalog.CatalogBackend
	usage  catalog.ResourceUsage
	closed bool
}

type catalogStorageDestroyBackend struct {
	catalog.CatalogBackend
	destroyed bool
}

func (backend *catalogStorageDestroyBackend) Destroy() error {
	backend.destroyed = true
	return backend.CatalogBackend.Close()
}

func (backend *catalogStorageUsageBackend) Recover(context.Context) (catalog.ResourceUsage, error) {
	return backend.usage, nil
}

func (backend *catalogStorageUsageBackend) Close() error {
	backend.closed = true
	return backend.CatalogBackend.Close()
}

func catalogStorageTestShare(seed byte) catalog.ShareInstance {
	var share catalog.ShareInstance
	for index := range share {
		share[index] = seed + byte(index)
	}
	return share
}
