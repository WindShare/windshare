package liveshare

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/windshare/windshare/core/catalog"
)

const (
	liveCatalogRegistryName = "windshare-live-catalog-v1"
	liveCatalogRootPrefix   = "share-"
	liveCatalogOwnerName    = "owner.lock"
	liveCatalogRegistryLock = "registry.lock"
)

func (operation CatalogStorageOperation) String() string {
	switch operation {
	case CatalogStorageCreating:
		return "creating"
	case CatalogStorageCreated:
		return "created"
	case CatalogStorageRecovering:
		return "recovering"
	case CatalogStorageRecovered:
		return "recovered"
	case CatalogStorageBudgetRejected:
		return "budget-rejected"
	case CatalogStorageCleaning:
		return "cleaning"
	case CatalogStorageCleaned:
		return "cleaned"
	default:
		return "unknown"
	}
}

type CatalogStorageOperation uint8

const (
	CatalogStorageCreating CatalogStorageOperation = iota + 1
	CatalogStorageCreated
	CatalogStorageRecovering
	CatalogStorageRecovered
	CatalogStorageBudgetRejected
	CatalogStorageCleaning
	CatalogStorageCleaned
)

type CatalogStorageCause uint8

const (
	CatalogStorageCauseNone CatalogStorageCause = iota
	CatalogStorageCauseCanceled
	CatalogStorageCauseDeadlineExceeded
	CatalogStorageCauseBudgetExceeded
	CatalogStorageCauseUnexpected
)

func (cause CatalogStorageCause) String() string {
	switch cause {
	case CatalogStorageCauseNone:
		return "none"
	case CatalogStorageCauseCanceled:
		return "canceled"
	case CatalogStorageCauseDeadlineExceeded:
		return "deadline-exceeded"
	case CatalogStorageCauseBudgetExceeded:
		return "budget-exceeded"
	case CatalogStorageCauseUnexpected:
		return "unexpected"
	default:
		return "unknown"
	}
}

// CatalogStorageTrace contains only closed facts. In particular it never
// retains provider errors, whose text may contain filesystem paths or relay
// credentials owned by an embedding application.
type CatalogStorageTrace struct {
	Operation          CatalogStorageOperation
	ShareInstance      catalog.ShareInstance
	RecoveredUsage     catalog.ResourceUsage
	LegacyRootsRemoved uint64
	Cause              CatalogStorageCause
}

type CatalogStorageTracer interface {
	TraceCatalogStorage(CatalogStorageTrace)
}

type CatalogStorageTraceFunc func(CatalogStorageTrace)

func (function CatalogStorageTraceFunc) TraceCatalogStorage(event CatalogStorageTrace) {
	if function != nil {
		function(event)
	}
}

type CatalogStorageFactory interface {
	Create(context.Context, catalog.ShareInstance) (catalog.CatalogBackend, error)
}

type CatalogStorageFactoryFunc func(context.Context, catalog.ShareInstance) (catalog.CatalogBackend, error)

func (function CatalogStorageFactoryFunc) Create(
	ctx context.Context,
	share catalog.ShareInstance,
) (catalog.CatalogBackend, error) {
	if function == nil {
		return nil, errors.New("live catalog storage factory is nil")
	}
	return function(ctx, share)
}

type fileCatalogStorageFactory struct {
	registry string
	tracer   CatalogStorageTracer
}

func productionCatalogStorageFactory(tracer CatalogStorageTracer) CatalogStorageFactory {
	return &fileCatalogStorageFactory{
		registry: filepath.Join(os.TempDir(), liveCatalogRegistryName),
		tracer:   tracer,
	}
}

func (factory *fileCatalogStorageFactory) Create(
	ctx context.Context,
	share catalog.ShareInstance,
) (result catalog.CatalogBackend, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if share.IsZero() {
		return nil, errors.New("live catalog storage requires a share identity")
	}
	if err := os.MkdirAll(factory.registry, 0o700); err != nil {
		return nil, fmt.Errorf("create live catalog registry: %w", err)
	}
	registryFile, err := openLockedCatalogRegistry(factory.registry)
	if err != nil {
		return nil, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, unlockAndCloseCatalogFile(registryFile))
	}()
	factory.trace(CatalogStorageTrace{
		Operation: CatalogStorageCleaning, ShareInstance: share,
	})
	removed, cleanupErr := factory.cleanAbandonedRoots(ctx)
	factory.trace(CatalogStorageTrace{
		Operation: CatalogStorageCleaned, ShareInstance: share,
		LegacyRootsRemoved: removed, Cause: catalogStorageCause(cleanupErr),
	})
	if cleanupErr != nil {
		return nil, cleanupErr
	}
	root, err := os.MkdirTemp(factory.registry, liveCatalogRootPrefix)
	if err != nil {
		return nil, fmt.Errorf("create private live catalog root: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			resultErr = errors.Join(resultErr, removeCatalogRoot(root))
		}
	}()
	owner, err := os.OpenFile(filepath.Join(root, liveCatalogOwnerName), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create live catalog owner lock: %w", err)
	}
	if err := lockCatalogFile(owner, false); err != nil {
		_ = owner.Close()
		return nil, fmt.Errorf("lock live catalog root: %w", err)
	}
	backend, err := catalog.NewFileCatalogBackend(catalog.FileCatalogBackendConfig{
		Root: filepath.Join(root, "catalog"), ShareInstance: share,
	})
	if err != nil {
		_ = unlockAndCloseCatalogFile(owner)
		return nil, err
	}
	committed = true
	return &ownedCatalogBackend{
		CatalogBackend: backend, registry: factory.registry, root: root, owner: owner,
		share: share, tracer: factory.tracer,
	}, nil
}

func openLockedCatalogRegistry(registry string) (*os.File, error) {
	registryFile, err := os.OpenFile(
		filepath.Join(registry, liveCatalogRegistryLock),
		os.O_CREATE|os.O_RDWR,
		0o600,
	)
	if err != nil {
		return nil, fmt.Errorf("open live catalog registry lock: %w", err)
	}
	if err := lockCatalogFile(registryFile, false); err != nil {
		return nil, errors.Join(
			fmt.Errorf("lock live catalog registry: %w", err),
			registryFile.Close(),
		)
	}
	return registryFile, nil
}

func (factory *fileCatalogStorageFactory) cleanAbandonedRoots(ctx context.Context) (uint64, error) {
	entries, err := os.ReadDir(factory.registry)
	if err != nil {
		return 0, fmt.Errorf("list live catalog registry: %w", err)
	}
	var removed uint64
	var cleanupErr error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return removed, errors.Join(cleanupErr, err)
		}
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), liveCatalogRootPrefix) {
			continue
		}
		root := filepath.Join(factory.registry, entry.Name())
		owner, openErr := os.OpenFile(filepath.Join(root, liveCatalogOwnerName), os.O_RDWR, 0)
		if errors.Is(openErr, os.ErrNotExist) {
			if removeErr := removeCatalogRoot(root); removeErr != nil {
				cleanupErr = errors.Join(cleanupErr, removeErr)
			} else {
				removed++
			}
			continue
		}
		if openErr != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("open abandoned live catalog owner: %w", openErr))
			continue
		}
		locked, lockErr := tryLockCatalogFile(owner)
		if lockErr != nil {
			_ = owner.Close()
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("inspect live catalog owner: %w", lockErr))
			continue
		}
		if !locked {
			_ = owner.Close()
			continue
		}
		closeErr := unlockAndCloseCatalogFile(owner)
		removeErr := removeCatalogRoot(root)
		if err := errors.Join(closeErr, removeErr); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		} else {
			removed++
		}
	}
	return removed, cleanupErr
}

func removeCatalogRoot(root string) error {
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("remove private live catalog root: %w", err)
	}
	return nil
}

func (factory *fileCatalogStorageFactory) trace(event CatalogStorageTrace) {
	traceCatalogStorage(factory.tracer, event)
}

type ownedCatalogBackend struct {
	catalog.CatalogBackend
	registry string
	root     string
	owner    *os.File
	share    catalog.ShareInstance
	tracer   CatalogStorageTracer

	closeOnce sync.Once
	closeErr  error
}

func (backend *ownedCatalogBackend) CatalogSpillRoot() string {
	if durable, ok := backend.CatalogBackend.(interface{ CatalogSpillRoot() string }); ok {
		return durable.CatalogSpillRoot()
	}
	return filepath.Join(backend.root, "sort")
}

func (backend *ownedCatalogBackend) Recover(ctx context.Context) (catalog.ResourceUsage, error) {
	backend.trace(CatalogStorageTrace{Operation: CatalogStorageRecovering, ShareInstance: backend.share})
	usage, err := backend.CatalogBackend.Recover(ctx)
	backend.trace(CatalogStorageTrace{
		Operation: CatalogStorageRecovered, ShareInstance: backend.share,
		RecoveredUsage: usage, Cause: catalogStorageCause(err),
	})
	return usage, err
}

func (backend *ownedCatalogBackend) Close() error   { return backend.destroy() }
func (backend *ownedCatalogBackend) Destroy() error { return backend.destroy() }

func (backend *ownedCatalogBackend) destroy() error {
	backend.closeOnce.Do(func() {
		backend.trace(CatalogStorageTrace{Operation: CatalogStorageCleaning, ShareInstance: backend.share})
		closeErr := backend.CatalogBackend.Close()
		registryFile, registryErr := openLockedCatalogRegistry(backend.registry)
		if registryErr != nil {
			// Releasing ownership makes the root recoverable by the next registry
			// sweep; deleting without the registry lock could race that sweep.
			backend.closeErr = errors.Join(closeErr, registryErr, unlockAndCloseCatalogFile(backend.owner))
		} else {
			ownerErr := unlockAndCloseCatalogFile(backend.owner)
			removeErr := removeCatalogRoot(backend.root)
			registryCloseErr := unlockAndCloseCatalogFile(registryFile)
			backend.closeErr = errors.Join(
				closeErr,
				ownerErr,
				removeErr,
				registryCloseErr,
			)
		}
		backend.trace(CatalogStorageTrace{
			Operation: CatalogStorageCleaned, ShareInstance: backend.share,
			Cause: catalogStorageCause(backend.closeErr),
		})
	})
	return backend.closeErr
}

func (backend *ownedCatalogBackend) trace(event CatalogStorageTrace) {
	traceCatalogStorage(backend.tracer, event)
}

type observedCatalogBackend struct {
	catalog.CatalogBackend
	share  catalog.ShareInstance
	tracer CatalogStorageTracer

	closeOnce sync.Once
	closeErr  error
}

func (backend *observedCatalogBackend) Recover(ctx context.Context) (catalog.ResourceUsage, error) {
	traceCatalogStorage(backend.tracer, CatalogStorageTrace{
		Operation: CatalogStorageRecovering, ShareInstance: backend.share,
	})
	usage, err := backend.CatalogBackend.Recover(ctx)
	traceCatalogStorage(backend.tracer, CatalogStorageTrace{
		Operation: CatalogStorageRecovered, ShareInstance: backend.share,
		RecoveredUsage: usage, Cause: catalogStorageCause(err),
	})
	return usage, err
}

func (backend *observedCatalogBackend) Close() error   { return backend.destroy() }
func (backend *observedCatalogBackend) Destroy() error { return backend.destroy() }

func (backend *observedCatalogBackend) destroy() error {
	backend.closeOnce.Do(func() {
		traceCatalogStorage(backend.tracer, CatalogStorageTrace{
			Operation: CatalogStorageCleaning, ShareInstance: backend.share,
		})
		if destroyable, ok := backend.CatalogBackend.(interface{ Destroy() error }); ok {
			backend.closeErr = destroyable.Destroy()
		} else {
			backend.closeErr = backend.CatalogBackend.Close()
		}
		traceCatalogStorage(backend.tracer, CatalogStorageTrace{
			Operation: CatalogStorageCleaned, ShareInstance: backend.share,
			Cause: catalogStorageCause(backend.closeErr),
		})
	})
	return backend.closeErr
}

func traceCatalogStorage(tracer CatalogStorageTracer, event CatalogStorageTrace) {
	if tracer == nil {
		return
	}
	// Storage diagnostics must never gain authority over catalog lifecycle.
	defer func() { _ = recover() }()
	tracer.TraceCatalogStorage(event)
}
