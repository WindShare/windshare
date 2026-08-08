package resumeauthority

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
)

const MaxResumeStateInventoryItems = checkpointmodel.MaxCheckpointRecordsPerIntent

var (
	ErrInventoryClosed          = errors.New("resume state inventory is closed")
	ErrReferenceConsumed        = errors.New("resume state reference is unavailable")
	ErrReferenceNotSerializable = errors.New("resume state reference cannot be serialized")
)

// Repository is the read-only entry point. The future native authority binds
// one certified root when constructing this port, so listing cannot accept an
// arbitrary internal path or acquire every intent lease eagerly.
type Repository interface {
	ListResumeState(context.Context) (PinnedInventory, error)
}

// PinnedInventory is the consumer-owned repository port. Implementations retain
// native pins internally and use the opaque ordinal only while this inventory
// is live; raw names, paths, and native identities never cross the boundary.
type PinnedInventory interface {
	Entries() []ListedState
	Acquire(context.Context, int) (LeasedRepository, error)
	Close() error
}

type inventoryState struct {
	mu       sync.Mutex
	cond     *sync.Cond
	pinned   PinnedInventory
	active   uint64
	closing  bool
	closed   bool
	closeErr error
}

type Inventory struct {
	state     *inventoryState
	summaries []Summary
}

// NewInventory takes ownership of pinned. A construction failure closes it so
// callers cannot leak filesystem handles while projecting an invalid adapter
// result.
func NewInventory(pinned PinnedInventory) (*Inventory, error) {
	if pinned == nil {
		return nil, ErrInvalidContract
	}
	entries := pinned.Entries()
	if len(entries) > MaxResumeStateInventoryItems {
		return nil, errors.Join(ErrInvalidContract, pinned.Close())
	}
	if err := validateListedStates(entries); err != nil {
		return nil, errors.Join(err, pinned.Close())
	}
	state := &inventoryState{pinned: pinned}
	state.cond = sync.NewCond(&state.mu)
	inventory := &Inventory{state: state, summaries: make([]Summary, len(entries))}
	for index, entry := range entries {
		capability := &referenceCapability{inventory: state, index: index}
		inventory.summaries[index] = Summary{
			state:     entry,
			reference: Reference{capability: capability},
		}
	}
	return inventory, nil
}

func (inventory *Inventory) Summaries() []Summary {
	if inventory == nil {
		return nil
	}
	return append([]Summary(nil), inventory.summaries...)
}

func (inventory *Inventory) Close() error {
	if inventory == nil || inventory.state == nil {
		return nil
	}
	state := inventory.state
	state.mu.Lock()
	for state.closing && !state.closed {
		state.cond.Wait()
	}
	if state.closed {
		err := state.closeErr
		state.mu.Unlock()
		return err
	}
	state.closing = true
	for state.active != 0 {
		state.cond.Wait()
	}
	pinned := state.pinned
	state.pinned = nil
	state.mu.Unlock()

	closeErr := pinned.Close()

	state.mu.Lock()
	state.closeErr = closeErr
	state.closed = true
	state.cond.Broadcast()
	state.mu.Unlock()
	return closeErr
}

// Reference is a live capability rather than an identifier. Copies share one
// consumption bit, so copying a Go value cannot manufacture extra authority.
type Reference struct {
	capability *referenceCapability
}

type referenceCapability struct {
	inventory *inventoryState
	index     int
	consumed  bool
}

func (Reference) MarshalJSON() ([]byte, error) {
	return nil, ErrReferenceNotSerializable
}

func (*Reference) UnmarshalJSON([]byte) error {
	return ErrReferenceNotSerializable
}

func (Reference) MarshalText() ([]byte, error) {
	return nil, ErrReferenceNotSerializable
}

func (*Reference) UnmarshalText([]byte) error {
	return ErrReferenceNotSerializable
}

func (Reference) GobEncode() ([]byte, error) {
	return nil, ErrReferenceNotSerializable
}

func (*Reference) GobDecode([]byte) error {
	return ErrReferenceNotSerializable
}

// The explicit use keeps encoding/json contract drift visible to static tools.
var _ json.Marshaler = Reference{}

type referenceClaim struct {
	state     *inventoryState
	pinned    PinnedInventory
	index     int
	acquireMu sync.Mutex
	acquired  bool
	once      sync.Once
}

func consumeReference(ctx context.Context, reference Reference) (*referenceClaim, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	capability := reference.capability
	if capability == nil || capability.inventory == nil {
		return nil, ErrReferenceConsumed
	}
	state := capability.inventory
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closing || state.closed || state.pinned == nil {
		return nil, ErrInventoryClosed
	}
	if capability.consumed {
		return nil, ErrReferenceConsumed
	}
	capability.consumed = true
	state.active++
	return &referenceClaim{
		state: state, pinned: state.pinned, index: capability.index,
	}, nil
}

func (claim *referenceClaim) Acquire(ctx context.Context) (LeasedRepository, error) {
	if claim == nil || claim.pinned == nil {
		return nil, ErrReferenceConsumed
	}
	claim.acquireMu.Lock()
	if claim.acquired {
		claim.acquireMu.Unlock()
		return nil, ErrReferenceConsumed
	}
	claim.acquired = true
	claim.acquireMu.Unlock()
	return claim.pinned.Acquire(ctx, claim.index)
}

func (claim *referenceClaim) Release() {
	if claim == nil || claim.state == nil {
		return
	}
	claim.once.Do(func() {
		claim.state.mu.Lock()
		claim.state.active--
		claim.state.cond.Broadcast()
		claim.state.mu.Unlock()
	})
}
