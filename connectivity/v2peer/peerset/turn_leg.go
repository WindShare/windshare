package peerset

import (
	"context"
	"io"
	"time"

	"github.com/windshare/windshare/connectivity/v2signal"
	diag "github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/peerslot"
	p "github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/turngrant"
)

const maximumGrantLifetime = 24 * time.Hour

type turnLeg struct {
	counted     bool
	slot        *peerslot.Lease
	stop        context.CancelFunc
	done, ready chan struct{}
	lane        session.LaneIdentity
	refresh     time.Time
}

func (l *turnLeg) retire() {
	if l.stop != nil {
		l.stop()
		<-l.done
		l.stop = nil
	}
}
func (l *turnLeg) close() {
	if l != nil {
		l.retire()
		l.slot.Close()
		l.slot = nil
	}
}
func signalled(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func (m *turnManager) start(now time.Time, l *turnLeg, g p.TURNGrant) {
	ctx, cancel := context.WithCancel(m.owner.ctx)
	l.stop, l.ready, l.done = cancel, make(chan struct{}), make(chan struct{})
	l.lane = session.LaneIdentity{}
	l.counted = false
	l.refresh = time.Time{}
	if g.ValidForMS > 0 {
		ttl := time.Duration(min(g.ValidForMS, uint64(maximumGrantLifetime.Milliseconds()))) * time.Millisecond
		l.refresh = now.Add(ttl - turngrant.RenewalLead(ttl))
	}
	ordinal, err := l.slot.Next()
	if err != nil {
		close(l.done)
		return
	}
	path := &m.paths[ordinal.Slot]
	var attempt v2signal.AttemptID
	m.owner.owner.mu.Lock()
	if *path == (v2signal.PeerPathID{}) {
		_, err = io.ReadFull(m.owner.owner.config.Random, path[:])
	}
	if err == nil {
		_, err = io.ReadFull(m.owner.owner.config.Random, attempt[:])
	}
	m.owner.owner.mu.Unlock()
	if err != nil {
		close(l.done)
		return
	}
	binding := v2signal.Binding{Purpose: v2signal.TURNRelay, PeerPathID: *path, PeerSlot: ordinal.Slot, AttemptID: attempt, AttemptSequence: ordinal.Sequence}
	go func() {
		defer close(l.done)
		reason := "negotiation_failed"
		defer func() {
			m.tracePath("turn_path_closed", g, binding, l.lane, reason)
		}()
		a, err := m.negotiate(ctx, binding, g)
		if err != nil {
			return
		}
		defer a.Close()
		select {
		case <-a.Done():
		case <-a.Ready():
		case <-ctx.Done():
		}
		// Join the single negotiation before checking for a racing authenticated attachment.
		if ctx.Err() != nil {
			_ = a.Close()
		}
		lane, ok := a.Lane()
		if ok {
			defer m.runtime.DetachLane(lane)
		}
		if ctx.Err() != nil {
			reason = "owner_closed"
			return
		}
		if !ok {
			return
		}
		done, usable := m.runtime.UsableLaneDone(lane)
		if !usable || signalled(done) {
			reason = "lane_lost"
			return
		}
		if ctx.Err() != nil {
			reason = "owner_closed"
			return
		}
		l.lane = lane
		close(l.ready)
		m.tracePath("turn_path_admitted", g, binding, lane, "")
		select {
		case <-ctx.Done():
			reason = "owner_closed"
		case <-done:
			reason = "lane_lost"
		}
	}()
}

func (m *turnManager) tracePath(event string, grant p.TURNGrant, binding v2signal.Binding, lane session.LaneIdentity, reason string) {
	if !m.trace.Sink.Enabled() {
		return
	}
	fields := diag.Fields{"grant_id": diag.Identity(grant.GrantID[:]), "peer_path_id": diag.Identity(binding.PeerPathID[:]), "attempt_sequence": diag.Uint(binding.AttemptSequence)}
	if lane.ID != 0 {
		fields["lane_id"] = diag.Uint(uint64(lane.ID))
		fields["lane_epoch"] = diag.Uint(uint64(lane.Epoch))
	}
	if reason != "" {
		fields["reason"] = diag.Text(reason)
	}
	m.trace.Event(event, grant.ServiceID, fields)
}
