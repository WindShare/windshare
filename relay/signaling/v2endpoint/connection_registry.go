package v2endpoint

import (
	"context"
	"sync"

	"github.com/windshare/windshare/relay/signaling/v2route"
)

type connectionCompareResult string

const (
	connectionExactCurrent      connectionCompareResult = "exact_current"
	connectionAbsent            connectionCompareResult = "absent"
	connectionGenerationChanged connectionCompareResult = "generation_changed"
)

// connectionRegistry owns local connection lifetimes. Wire IDs index the
// current slot, while the opaque ConnectionRef is the only authority for
// lookup, detach, cancellation, and completion.
type connectionRegistry struct {
	mu sync.Mutex

	current map[v2route.ConnectionID]*connection
	live    map[v2route.ConnectionRef]*connection
	closed  bool
	drained chan struct{}
}

func newConnectionRegistry() *connectionRegistry {
	return &connectionRegistry{
		current: make(map[v2route.ConnectionID]*connection),
		live:    make(map[v2route.ConnectionRef]*connection),
		drained: make(chan struct{}),
	}
}

func (registry *connectionRegistry) add(peer *connection) bool {
	if registry == nil || peer == nil || !peer.ref.Valid() {
		return false
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	connectionID := peer.ref.ConnectionID()
	if registry.closed || registry.current[connectionID] != nil {
		return false
	}
	registry.current[connectionID] = peer
	registry.live[peer.ref] = peer
	return true
}

func (registry *connectionRegistry) resolve(
	reference v2route.ConnectionRef,
) (*connection, connectionCompareResult, uint64) {
	if registry == nil || !reference.Valid() {
		return nil, connectionAbsent, 0
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	current := registry.current[reference.ConnectionID()]
	if current == nil {
		return nil, connectionAbsent, 0
	}
	if current.ref != reference {
		return nil, connectionGenerationChanged, current.ref.LocalGeneration()
	}
	return current, connectionExactCurrent, current.ref.LocalGeneration()
}

func (registry *connectionRegistry) detach(reference v2route.ConnectionRef) connectionCompareResult {
	if registry == nil || !reference.Valid() {
		return connectionAbsent
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	connectionID := reference.ConnectionID()
	current := registry.current[connectionID]
	if current == nil {
		return connectionAbsent
	}
	if current.ref != reference {
		return connectionGenerationChanged
	}
	delete(registry.current, connectionID)
	return connectionExactCurrent
}

func (registry *connectionRegistry) complete(reference v2route.ConnectionRef) bool {
	if registry == nil || !reference.Valid() {
		return false
	}
	registry.mu.Lock()
	peer, exists := registry.live[reference]
	if !exists {
		registry.mu.Unlock()
		return false
	}
	delete(registry.live, reference)
	if registry.current[reference.ConnectionID()] == peer {
		delete(registry.current, reference.ConnectionID())
	}
	registry.closeDrainedLocked()
	registry.mu.Unlock()
	return true
}

func (registry *connectionRegistry) closeDrainedLocked() {
	if !registry.closed || len(registry.live) != 0 {
		return
	}
	select {
	case <-registry.drained:
	default:
		close(registry.drained)
	}
}

// close cancels every exact generation admitted before the close boundary and
// then joins those generations. Keeping detached-but-live peers in the snapshot
// prevents cleanup/apply windows from escaping shutdown accounting.
func (registry *connectionRegistry) close(ctx context.Context) error {
	if registry == nil {
		return ErrConfig
	}
	registry.mu.Lock()
	registry.closed = true
	peers := make([]*connection, 0, len(registry.live))
	for _, peer := range registry.live {
		peers = append(peers, peer)
	}
	registry.closeDrainedLocked()
	drained := registry.drained
	registry.mu.Unlock()
	for _, peer := range peers {
		peer.requestClose()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-drained:
		return nil
	}
}
