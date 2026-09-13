package v2endpoint

import (
	"sync"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

const (
	receiverWindowFrames = 16
	// A retired session may still own the sole physical write after its queued
	// reservations are reclaimed. Keep one maximum-size write outside the pool.
	receiverCreditCapacity = min(MaximumForwardQueueFrames, (MaximumForwardQueueBytes-MaximumV2WebSocketMessageSize)/MaximumV2WebSocketMessageSize)
)

type receiverAllocation struct {
	source   *connection
	reserved int
}

// receiverCreditPool reserves destination slots before a receiver may write.
// Credits, queued frames and the current physical write share this one bound;
// returning a slot may grant it to a different waiting session.
type receiverCreditPool struct {
	mu           sync.Mutex
	entries      map[v2.RelaySessionID]*receiverAllocation
	order        []v2.RelaySessionID
	cursor       int
	reserved     int
	windowFrames int
}

func (destination *connection) reserveReceiverCredit(id v2.RelaySessionID, source *connection) bool {
	destination.sessionMu.Lock()
	defer destination.sessionMu.Unlock()
	if _, current := destination.sessions[id]; !current || destination.closed.Load() {
		return false
	}
	return destination.receiverCredits.add(id, source)
}

func (pool *receiverCreditPool) add(id v2.RelaySessionID, source *connection) bool {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.entries == nil {
		pool.entries = make(map[v2.RelaySessionID]*receiverAllocation)
	}
	if existing := pool.entries[id]; existing != nil {
		return existing.source == source && !source.closed.Load()
	}
	// Byte and frame grants are orthogonal. Destination memory is reserved at
	// maximum frame size, so unused byte allowance cannot oversubscribe it.
	if !source.addReceiverCredit(id, 0, v2.SenderWindowBytes) {
		return false
	}
	pool.entries[id] = &receiverAllocation{source: source}
	pool.order = append(pool.order, id)
	pool.grantLocked()
	return true
}

func (pool *receiverCreditPool) complete(id v2.RelaySessionID, size int) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	entry := pool.entries[id]
	if entry == nil || entry.reserved == 0 {
		return
	}
	entry.reserved--
	pool.reserved--
	entry.source.addReceiverCredit(id, 0, uint32(size))
	pool.grantLocked()
}

func (pool *receiverCreditPool) remove(id v2.RelaySessionID) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	entry := pool.entries[id]
	if entry == nil {
		return
	}
	pool.reserved -= entry.reserved
	delete(pool.entries, id)
	for index, candidate := range pool.order {
		if candidate == id {
			pool.order = append(pool.order[:index], pool.order[index+1:]...)
			if pool.cursor > index {
				pool.cursor--
			}
			break
		}
	}
	pool.grantLocked()
}

func (pool *receiverCreditPool) grantLocked() {
	windowFrames := pool.windowFrames
	if windowFrames == 0 {
		windowFrames = receiverWindowFrames
	}
	for pool.reserved < receiverCreditCapacity && len(pool.order) > 0 {
		granted := false
		for range len(pool.order) {
			if pool.cursor >= len(pool.order) {
				pool.cursor = 0
			}
			id := pool.order[pool.cursor]
			pool.cursor++
			entry := pool.entries[id]
			if entry.reserved >= windowFrames || entry.source.closed.Load() {
				continue
			}
			if !entry.source.addReceiverCredit(id, 1, 0) {
				continue
			}
			entry.reserved++
			pool.reserved++
			granted = true
			break
		}
		if !granted {
			return
		}
	}
}

func (peer *connection) addReceiverCredit(id v2.RelaySessionID, frames, bytes uint32) bool {
	peer.sessionMu.Lock()
	defer peer.sessionMu.Unlock()
	window := peer.windows[id]
	if peer.closed.Load() || window == nil {
		return false
	}
	window.pendingFrames += frames
	window.pendingBytes += bytes
	select {
	case peer.wake <- struct{}{}:
	default:
	}
	return true
}
