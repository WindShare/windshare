package socketauthority

import (
	"sync"
	"time"
)

type iceOwnership uint8

const (
	iceUnclaimed iceOwnership = iota
	iceActive
)

// unavailableLocked is checked again after every unlocked wait: cancellation
// does not preserve a lease or network generation's right to start new work.
func (l *Lease) unavailableLocked() error {
	if l.released || l.authority.closing != nil || l.entry.closing != nil {
		return ErrClosed
	}
	if l.entry.key.generation <= l.authority.retiredThrough {
		return ErrRetired
	}
	return nil
}

// Claim transfers exclusive ICE ownership for this path lease. The returned
// release must run after PeerConnection.Close completes, before replacement.
func (l *Lease) Claim() (*Mux, func(), error) {
	if l == nil {
		return nil, nil, ErrInvalid
	}
	a := l.authority
	a.mu.Lock()
	if err := l.unavailableLocked(); err != nil {
		a.mu.Unlock()
		return nil, nil, err
	}
	if l.entry.owner != iceUnclaimed {
		a.mu.Unlock()
		return nil, nil, ErrActive
	}
	l.entry.owner = iceActive
	a.mu.Unlock()
	started := time.Now()
	l.observe(Event{Kind: SocketHandoffStarted, Result: "pending"})
	l.observe(Event{Kind: SocketHandoffFinished, Duration: time.Since(started), Result: socketResult(nil)})
	var once sync.Once
	return l.entry.mux, func() {
		once.Do(func() {
			a.mu.Lock()
			l.entry.owner = iceUnclaimed
			a.mu.Unlock()
		})
	}, nil
}
