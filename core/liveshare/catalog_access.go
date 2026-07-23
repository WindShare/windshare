package liveshare

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

const rootPrefetchBudgetName = "live-share-root-prefetch"

type catalogDirectoryStore interface {
	ListChildren(
		context.Context,
		catalog.DirectoryID,
		*catalog.BudgetAccount,
		catalog.ScanOptions,
		catalog.DirectoryScanner,
	) (catalog.CommittedDirectory, error)
}

// senderCatalogAccess keeps user-driven catalog work authoritative over optional
// root warming. A receiver that arrives during prefetch cancels that waiter and
// joins or restarts the same store-owned scan with its own session budget.
type senderCatalogAccess struct {
	share   catalog.ShareInstance
	store   *catalog.CatalogStore
	listing catalogDirectoryStore
	scanner catalog.DirectoryScanner
	roots   []catalog.DirectoryID
	budget  *catalog.BudgetAccount

	sessionSequence atomic.Uint64
	wake            chan struct{}
	prefetchWG      sync.WaitGroup

	mu              sync.Mutex
	demands         uint64
	nextAttempt     uint64
	activeAttempt   uint64
	activeCancel    context.CancelFunc
	lifetimeCancel  context.CancelFunc
	prefetchStarted bool
	prefetchStopped bool
	closed          bool
}

func newSenderCatalogAccess(
	share catalog.ShareInstance,
	store *catalog.CatalogStore,
	scanner catalog.DirectoryScanner,
	selected []catalog.NodeRecord,
) (*senderCatalogAccess, error) {
	if share.IsZero() || store == nil || scanner == nil {
		return nil, errors.New("live share catalog access requires share, store, and scanner")
	}
	budget, err := catalog.NewBudgetAccount(rootPrefetchBudgetName, catalog.DefaultSessionBudgetLimits())
	if err != nil {
		return nil, err
	}
	roots := make([]catalog.DirectoryID, 0, len(selected))
	for _, record := range selected {
		if directory, ok := record.DirectoryID(); ok {
			roots = append(roots, directory)
		}
	}
	return &senderCatalogAccess{
		share: share, store: store, listing: store, scanner: scanner, roots: roots, budget: budget,
		wake: make(chan struct{}, 1),
	}, nil
}

func (access *senderCatalogAccess) NewSenderCatalogService() (*catalogflow.AddressedSenderService, error) {
	if access == nil {
		return nil, errors.New("live share catalog access is unavailable")
	}
	access.mu.Lock()
	closed := access.closed
	access.mu.Unlock()
	if closed {
		return nil, errors.New("live share catalog access is closed")
	}
	sequence := access.sessionSequence.Add(1)
	budget, err := catalog.NewBudgetAccount(
		fmt.Sprintf("live-share-catalog-session-%d", sequence),
		catalog.DefaultSessionBudgetLimits(),
	)
	if err != nil {
		return nil, err
	}
	source, err := catalogflow.NewCatalogStoreSource(catalogflow.CatalogStoreSourceConfig{
		ShareInstance: access.share,
		Store:         access.store,
		SessionBudget: budget,
		Scanner:       access.scanner,
	})
	if err != nil {
		return nil, err
	}
	return catalogflow.NewAddressedSenderService(
		access.share,
		receiverPriorityCatalogSource{access: access, source: source},
	)
}

func (access *senderCatalogAccess) StartRootPrefetch() {
	if access == nil {
		return
	}
	access.mu.Lock()
	if access.prefetchStarted || access.prefetchStopped || access.closed || len(access.roots) == 0 {
		access.mu.Unlock()
		return
	}
	lifetime, cancel := context.WithCancel(context.Background())
	access.prefetchStarted = true
	access.lifetimeCancel = cancel
	access.prefetchWG.Add(1)
	access.mu.Unlock()
	go access.runRootPrefetch(lifetime)
}

func (access *senderCatalogAccess) CancelRootPrefetch() {
	if access == nil {
		return
	}
	access.mu.Lock()
	if access.prefetchStopped {
		access.mu.Unlock()
		return
	}
	access.prefetchStopped = true
	lifetimeCancel, activeCancel := access.lifetimeCancel, access.activeCancel
	access.signalLocked()
	access.mu.Unlock()
	if lifetimeCancel != nil {
		lifetimeCancel()
	}
	if activeCancel != nil {
		activeCancel()
	}
}

func (access *senderCatalogAccess) Close() {
	if access == nil {
		return
	}
	access.mu.Lock()
	if access.closed {
		access.mu.Unlock()
		return
	}
	access.closed = true
	access.prefetchStopped = true
	lifetimeCancel, activeCancel := access.lifetimeCancel, access.activeCancel
	access.signalLocked()
	access.mu.Unlock()
	if lifetimeCancel != nil {
		lifetimeCancel()
	}
	if activeCancel != nil {
		activeCancel()
	}
	access.prefetchWG.Wait()
}

func (access *senderCatalogAccess) runRootPrefetch(lifetime context.Context) {
	defer access.prefetchWG.Done()
	for _, directory := range access.roots {
		for {
			attemptContext, attempt, err := access.beginPrefetchAttempt(lifetime)
			if err != nil {
				return
			}
			_, _ = access.listing.ListChildren(
				attemptContext,
				directory,
				access.budget,
				catalog.ScanOptions{},
				access.scanner,
			)
			interruptedByDemand := attemptContext.Err() != nil && lifetime.Err() == nil
			access.finishPrefetchAttempt(attempt)
			if lifetime.Err() != nil {
				return
			}
			if interruptedByDemand {
				continue
			}
			break
		}
	}
}

func (access *senderCatalogAccess) beginPrefetchAttempt(
	lifetime context.Context,
) (context.Context, uint64, error) {
	for {
		if err := lifetime.Err(); err != nil {
			return nil, 0, err
		}
		access.mu.Lock()
		if access.closed || access.prefetchStopped {
			access.mu.Unlock()
			return nil, 0, context.Canceled
		}
		if access.demands == 0 {
			access.nextAttempt++
			attempt := access.nextAttempt
			attemptContext, cancel := context.WithCancel(lifetime)
			access.activeAttempt = attempt
			access.activeCancel = cancel
			access.mu.Unlock()
			return attemptContext, attempt, nil
		}
		wake := access.wake
		access.mu.Unlock()
		select {
		case <-lifetime.Done():
			return nil, 0, lifetime.Err()
		case <-wake:
		}
	}
}

func (access *senderCatalogAccess) finishPrefetchAttempt(attempt uint64) {
	access.mu.Lock()
	var cancel context.CancelFunc
	if access.activeAttempt == attempt {
		cancel = access.activeCancel
		access.activeAttempt = 0
		access.activeCancel = nil
	}
	access.signalLocked()
	access.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (access *senderCatalogAccess) beginReceiverDemand() func() {
	access.mu.Lock()
	access.demands++
	activeCancel := access.activeCancel
	access.signalLocked()
	access.mu.Unlock()
	if activeCancel != nil {
		activeCancel()
	}
	return func() {
		access.mu.Lock()
		if access.demands > 0 {
			access.demands--
		}
		access.signalLocked()
		access.mu.Unlock()
	}
}

func (access *senderCatalogAccess) signalLocked() {
	select {
	case access.wake <- struct{}{}:
	default:
	}
}

type receiverPriorityCatalogSource struct {
	access *senderCatalogAccess
	source *catalogflow.CatalogStoreSource
}

func (source receiverPriorityCatalogSource) LoadPage(
	ctx context.Context,
	request catalogflow.ListRequest,
	progress catalog.ScanProgressObserver,
) (catalogflow.PageResult, error) {
	release := source.access.beginReceiverDemand()
	defer release()
	return source.source.LoadPage(ctx, request, progress)
}

type prefetchTerminalConnectivity struct {
	prefetch *senderCatalogAccess
	delegate sessionruntime.TerminalConnectivity
}

func (connectivity prefetchTerminalConnectivity) StopRecovery() {
	connectivity.prefetch.CancelRootPrefetch()
	connectivity.delegate.StopRecovery()
}

func (connectivity prefetchTerminalConnectivity) Cleanup(ctx context.Context) error {
	return connectivity.delegate.Cleanup(ctx)
}
