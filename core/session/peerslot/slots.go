// Package peerslot owns bounded negotiation identities for one receiver session.
package peerslot

import (
	"errors"
	"sync"
)

// Count bounds session replay metadata independently of physical path churn.
const Count = 4

var (
	ErrCapacity = errors.New("peer negotiation slots are occupied")
	ErrClosed   = errors.New("peer negotiation slot is closed")
	ErrSequence = errors.New("peer negotiation sequence is exhausted")
)

type Attempt struct {
	Slot     uint8
	Sequence uint64
}

type state struct {
	sequence uint64
	owner    *Lease
}

// Pool retains sequence watermarks for the session, including while slots are
// vacant. Replacing a physical path must not reset its replay protection.
type Pool struct {
	mu    sync.Mutex
	slots [Count]state
}

type Lease struct {
	pool *Pool
	slot uint8
}

func (p *Pool) Acquire() (*Lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for index := range p.slots {
		if p.slots[index].owner == nil && p.slots[index].sequence != ^uint64(0) {
			lease := &Lease{pool: p, slot: uint8(index)}
			p.slots[index].owner = lease
			return lease, nil
		}
	}
	return nil, ErrCapacity
}

func (l *Lease) Next() (Attempt, error) {
	if l == nil || l.pool == nil {
		return Attempt{}, ErrClosed
	}
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	slot := &l.pool.slots[l.slot]
	if slot.owner != l {
		return Attempt{}, ErrClosed
	}
	if slot.sequence == ^uint64(0) {
		return Attempt{}, ErrSequence
	}
	slot.sequence++
	return Attempt{Slot: l.slot, Sequence: slot.sequence}, nil
}

// Close follows the path's joined teardown, so a delayed producer cannot spend
// sequence numbers belonging to its successor.
func (l *Lease) Close() {
	if l == nil || l.pool == nil {
		return
	}
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	if l.pool.slots[l.slot].owner == l {
		l.pool.slots[l.slot].owner = nil
	}
}
