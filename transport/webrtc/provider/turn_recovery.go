package provider

import (
	"net"
	"strconv"
	"sync"

	"github.com/pion/stun/v4"
	"github.com/pion/turn/v5"
	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/core/session/turncontrol"
)

const maximumTURNBindingObservations = 512

type turnBindingKey struct{ allocation, peer string }

type turnRecovery struct {
	mu               sync.Mutex
	selected         turnBindingKey
	issues           map[turnBindingKey]turn.BindingEvent
	notified, closed bool
}

func (c *Connection) observeTURNBinding(event turn.BindingEvent) {
	if c.request.Observe != nil {
		observation := c.event("turn_binding", strconv.Itoa(int(event.Status)))
		observation.TURNBinding = &event
		c.request.Observe(observation)
	}
	r := &c.turnRecovery
	r.mu.Lock()
	key := turnBindingKey{event.RelayedAddress, event.PeerAddress}
	switch event.Status {
	case turn.BindingActive:
		delete(r.issues, key)
	case turn.BindingMismatch, turn.BindingExhausted:
		if r.issues == nil {
			r.issues = make(map[turnBindingKey]turn.BindingEvent)
		}
		if len(r.issues) < maximumTURNBindingObservations || key == r.selected {
			r.issues[key] = event
		}
	}
	reason := r.recoveryLocked()
	r.mu.Unlock()
	c.notifyTURNRecovery(reason)
}

func (c *Connection) selectTURNPair(pair *pion.ICECandidatePair) {
	if pair == nil {
		return
	}
	r := &c.turnRecovery
	r.mu.Lock()
	r.selected = turnBindingKey{
		net.JoinHostPort(pair.Local.Address, strconv.Itoa(int(pair.Local.Port))),
		net.JoinHostPort(pair.Remote.Address, strconv.Itoa(int(pair.Remote.Port))),
	}
	reason := r.recoveryLocked()
	r.mu.Unlock()
	c.notifyTURNRecovery(reason)
}

func (r *turnRecovery) recoveryLocked() turncontrol.RecoveryReason {
	if r.closed || r.notified || r.selected.allocation == "" {
		return 0
	}
	for key, issue := range r.issues {
		// A lost allocation affects every peer. Other binding failures matter
		// only when ICE actually selects the affected peer on that allocation.
		if key != r.selected && (key.allocation != r.selected.allocation || issue.Code != int(stun.CodeAllocMismatch)) {
			continue
		}
		r.notified = true
		if issue.Status == turn.BindingMismatch {
			return turncontrol.AllocationStateMismatch
		}
		return turncontrol.ChannelUnavailable
	}
	return 0
}

func (c *Connection) notifyTURNRecovery(reason turncontrol.RecoveryReason) {
	if reason != 0 && c.request.OnTURNRecovery != nil {
		c.request.OnTURNRecovery(reason)
	}
}
