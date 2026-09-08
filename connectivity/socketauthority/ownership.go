package socketauthority

import (
	"sync"
	"time"
)

type iceOwnership uint8

const (
	iceUnclaimed iceOwnership = iota
	iceClaiming
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

// Claim transfers exclusive keepalive ownership to one ICE agent. The returned
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
	// Reserve before unlocking so another claimant cannot enter the gap between
	// stopping background discovery and handing the same socket to ICE.
	l.entry.owner = iceClaiming
	idle := l.entry.idle
	if idle != nil {
		idle.cancel()
	}
	a.mu.Unlock()
	started := time.Now()
	l.observe(Event{Kind: SocketHandoffStarted, Result: "pending"})
	if idle != nil {
		<-idle.done
	}
	a.mu.Lock()
	l.entry.idle = nil
	err := l.unavailableLocked()
	if err != nil {
		l.entry.owner = iceUnclaimed
	} else {
		l.entry.owner = iceActive
	}
	a.mu.Unlock()
	l.observe(Event{Kind: SocketHandoffFinished, Duration: time.Since(started), Result: socketResult(err)})
	if err != nil {
		return nil, nil, err
	}
	var once sync.Once
	return l.entry.mux, func() {
		once.Do(func() {
			a.mu.Lock()
			l.entry.owner = iceUnclaimed
			a.mu.Unlock()
		})
	}, nil
}
