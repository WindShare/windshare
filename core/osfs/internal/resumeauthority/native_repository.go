package resumeauthority

import (
	"context"
	"errors"
	"sync"
)

// NativeRepository composes the private leased store with the independent
// guarded public-path observer. The closer normally owns the native platform;
// its lifetime is transferred to the returned inventory.
type NativeRepository struct {
	store    Repository
	observer PublicationObserver
	closer   func() error

	mu     sync.Mutex
	listed bool
}

func NewNativeRepository(
	store Repository,
	observer PublicationObserver,
	closer func() error,
) (*NativeRepository, error) {
	if store == nil || observer == nil || closer == nil {
		return nil, ErrInvalidContract
	}
	return &NativeRepository{store: store, observer: observer, closer: closer}, nil
}

func (repository *NativeRepository) ListResumeState(
	ctx context.Context,
) (PinnedInventory, error) {
	if repository == nil {
		return nil, ErrInvalidContract
	}
	repository.mu.Lock()
	if repository.listed || repository.store == nil || repository.observer == nil || repository.closer == nil {
		repository.mu.Unlock()
		return nil, ErrInvalidContract
	}
	repository.listed = true
	store, observer, closer := repository.store, repository.observer, repository.closer
	repository.store, repository.observer, repository.closer = nil, nil, nil
	repository.mu.Unlock()
	if ctx == nil {
		return nil, errors.Join(ErrInvalidContract, closer())
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, closer())
	}

	pinned, err := store.ListResumeState(ctx)
	if err != nil {
		return nil, errors.Join(err, closer())
	}
	if pinned == nil {
		return nil, errors.Join(ErrInvalidContract, closer())
	}
	inventory := &nativeInventory{pinned: pinned, observer: observer, closer: closer}
	inventory.cond = sync.NewCond(&inventory.mu)
	return inventory, nil
}

type nativeInventory struct {
	mu       sync.Mutex
	cond     *sync.Cond
	pinned   PinnedInventory
	observer PublicationObserver
	closer   func() error
	closing  bool
	closed   bool
	closeErr error
}

func (inventory *nativeInventory) Entries() []ListedState {
	if inventory == nil {
		return nil
	}
	inventory.mu.Lock()
	defer inventory.mu.Unlock()
	if inventory.closing || inventory.closed || inventory.pinned == nil {
		return nil
	}
	return inventory.pinned.Entries()
}

func (inventory *nativeInventory) Acquire(
	ctx context.Context,
	index int,
) (LeasedRepository, error) {
	if inventory == nil {
		return nil, ErrInvalidContract
	}
	inventory.mu.Lock()
	if inventory.closing || inventory.closed || inventory.pinned == nil || inventory.observer == nil {
		inventory.mu.Unlock()
		return nil, ErrInventoryClosed
	}
	pinned, observer := inventory.pinned, inventory.observer
	inventory.mu.Unlock()

	leased, err := pinned.Acquire(ctx, index)
	if err != nil {
		return nil, err
	}
	if leased == nil {
		return nil, ErrInvalidContract
	}
	provider, ok := leased.(PinnedCheckpointProvider)
	if !ok || provider == nil {
		return nil, errors.Join(ErrInvalidContract, leased.Close())
	}
	return &nativeLeasedRepository{
		LeasedRepository: leased,
		provider:         provider,
		observer:         observer,
	}, nil
}

func (inventory *nativeInventory) Close() error {
	if inventory == nil {
		return nil
	}
	inventory.mu.Lock()
	for inventory.closing && !inventory.closed {
		inventory.cond.Wait()
	}
	if inventory.closed {
		err := inventory.closeErr
		inventory.mu.Unlock()
		return err
	}
	inventory.closing = true
	pinned, closer := inventory.pinned, inventory.closer
	inventory.pinned, inventory.observer, inventory.closer = nil, nil, nil
	inventory.mu.Unlock()

	var pinnedErr, platformErr error
	if pinned != nil {
		pinnedErr = pinned.Close()
	}
	// Store pins close before the platform so a failed close cannot shorten the
	// native root lifetime while checkpoint capabilities still exist.
	if closer != nil {
		platformErr = closer()
	}
	closeErr := errors.Join(pinnedErr, platformErr)
	inventory.mu.Lock()
	inventory.closeErr = closeErr
	inventory.closed = true
	inventory.closing = false
	inventory.cond.Broadcast()
	inventory.mu.Unlock()
	return closeErr
}

type nativeLeasedRepository struct {
	LeasedRepository
	provider PinnedCheckpointProvider
	observer PublicationObserver
}

func (repository *nativeLeasedRepository) PinPublications(
	ctx context.Context,
	snapshot RepositorySnapshot,
) (_ []PinnedPublication, resultErr error) {
	if repository == nil || repository.provider == nil || repository.observer == nil || ctx == nil {
		return nil, ErrInvalidContract
	}
	checkpoints := snapshot.Checkpoints()
	publications := make([]PinnedPublication, 0, len(checkpoints))
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, closePinnedPublications(publications))
		}
	}()
	for _, checkpoint := range checkpoints {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pinned, ok := repository.provider.PinnedCheckpoint(checkpoint.RecordID())
		if !ok || pinned == nil {
			return nil, ErrInvalidContract
		}
		pinnedRecord := pinned.Record()
		if pinnedRecord.RecordID() != checkpoint.RecordID() ||
			pinnedRecord.Checksum() != checkpoint.Record().Checksum() {
			return nil, ErrInvalidContract
		}
		publication, err := repository.observer.PinPublication(ctx, pinned)
		if err != nil {
			return nil, err
		}
		if publication == nil || publication.Observation().RecordID() != checkpoint.RecordID() {
			if publication != nil {
				err = publication.Close()
			}
			return nil, errors.Join(ErrInvalidContract, err)
		}
		publications = append(publications, publication)
	}
	return publications, nil
}

var _ Repository = (*NativeRepository)(nil)
var _ PinnedInventory = (*nativeInventory)(nil)
var _ publicationRepository = (*nativeLeasedRepository)(nil)
