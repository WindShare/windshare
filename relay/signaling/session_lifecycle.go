package signaling

import (
	"encoding/binary"
	"io"
	"sync"

	"github.com/windshare/windshare/relay/protocol"
)

type senderSessionState uint8

const (
	senderSessionNeverKnown senderSessionState = iota
	senderSessionActive
	senderSessionTerminal
)

const maxSessionSequence = ^uint64(0)

// senderSessionIDs encodes a connection-local monotonic sequence through a
// random mask. Calling next irrevocably retires that sequence before either pump
// lane is opened. This lets failed lane creation and candidates already reserved
// by detached diagnostics remain safe terminal IDs without per-ID tombstones.
// Session IDs are routing labels, not authorization capabilities; Hub.mu guards
// this constant-size namespace state.
type senderSessionIDs struct {
	mask        uint64
	highWater   uint64
	initialized bool
}

func (s *senderSessionIDs) next(random io.Reader) (protocol.SessionID, bool) {
	var id protocol.SessionID
	if !s.initialized {
		var maskBytes [protocol.SessionIDBytes]byte
		if _, err := io.ReadFull(random, maskBytes[:]); err != nil {
			return id, false
		}
		s.mask = binary.BigEndian.Uint64(maskBytes[:])
		s.initialized = true
	}
	if s.highWater == maxSessionSequence {
		return id, false
	}

	sequence := s.highWater + 1
	binary.BigEndian.PutUint64(id[:], sequence^s.mask)
	s.highWater = sequence
	return id, true
}

func (s *senderSessionIDs) recognizes(id protocol.SessionID) bool {
	if !s.initialized {
		return false
	}
	sequence := binary.BigEndian.Uint64(id[:]) ^ s.mask
	return sequence > 0 && sequence <= s.highWater
}

type unknownSessionObservation uint8

const (
	unknownSessionRepeated unknownSessionObservation = iota
	unknownSessionFirst
	unknownSessionLimitExceeded
)

// unknownSessionTracker charges diagnostic capacity per distinct never-issued
// ID, not per frame. Repeated frames can then be dropped without repeatedly
// materializing an ephemeral pump session. Hub.mu is the semantic namespace
// lock shared with candidate retirement; this private mutex also makes isolated
// inspection and tests race-safe.
type unknownSessionTracker struct {
	mu  sync.Mutex
	ids map[protocol.SessionID]struct{}
}

func (t *unknownSessionTracker) observe(id protocol.SessionID, limit int) unknownSessionObservation {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.ids[id]; exists {
		return unknownSessionRepeated
	}
	if limit <= 0 || len(t.ids) >= limit {
		return unknownSessionLimitExceeded
	}
	if t.ids == nil {
		t.ids = make(map[protocol.SessionID]struct{}, limit)
	}
	t.ids[id] = struct{}{}
	return unknownSessionFirst
}

// rollback releases a first-observation reservation when no diagnostic was
// established. Production callers also hold Hub.mu so issuance can only observe
// the reservation before cleanup or its absence after cleanup, never a gap.
func (t *unknownSessionTracker) rollback(id protocol.SessionID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.ids, id)
}

func (t *unknownSessionTracker) contains(id protocol.SessionID) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, exists := t.ids[id]
	return exists
}

func (t *unknownSessionTracker) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.ids)
}
