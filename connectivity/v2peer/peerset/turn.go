package peerset

import (
	"context"
	"io"
	"time"

	"github.com/windshare/windshare/core/session/turngrant"

	"github.com/windshare/windshare/connectivity/turnsession"
	"github.com/windshare/windshare/connectivity/v2signal"
	diag "github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/peerslot"
	p "github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/lanes"
)

const grantResponseTimeout = 8 * time.Second
const renewalLead = 30 * time.Second

var turnBackoff = [...]time.Duration{2 * time.Second, 10 * time.Second, 30 * time.Second}

type turnRuntime interface {
	ContentLaneOwner
	SetTURNControlHandler(func(context.Context, []byte) error)
	SendTURNControl(context.Context, []byte) error
	AttachedLanes() int
	LaneDone(session.LaneIdentity) (<-chan struct{}, bool)
}
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

type turnService struct {
	current, next *turnLeg
	retries       int
	due           time.Time
}
type requestState struct {
	mode    p.TURNMode
	service string
	until   time.Time
}
type turnManager struct {
	owner         *ReceiverSession
	trace         turngrant.Trace
	negotiate     func(context.Context, v2signal.Binding, p.TURNGrant) (Attempt, error)
	runtime       turnRuntime
	pool          *peerslot.Pool
	paths         [peerslot.Count]v2signal.PeerPathID
	in            chan p.TURNGrants
	serial        uint64
	wait          map[uint64]requestState
	services      map[string]*turnService
	policy        turnsession.Demand
	asked         p.TURNMode
	disabled      bool
	retries       int
	due, settling time.Time
}

func newTURNManager(s *ReceiverSession, r turnRuntime) *turnManager {
	id := receiverSessionID(s.config.Lanes)
	s.owner.mu.Lock()
	pool := s.owner.slotPoolLocked(id)
	s.owner.mu.Unlock()
	config := s.config
	m := &turnManager{
		owner: s, trace: turngrant.Trace{Sink: config.Trace, Session: id}, runtime: r, pool: pool,
		in: make(chan p.TURNGrants, 16), wait: make(map[uint64]requestState), services: make(map[string]*turnService),
		negotiate: func(ctx context.Context, binding v2signal.Binding, grant p.TURNGrant) (Attempt, error) {
			return config.Factory.StartTURNBinding(ctx, config.Signaling, config.Lanes, binding, grant)
		},
	}
	r.SetTURNControlHandler(func(ctx context.Context, b []byte) error {
		g, err := p.DecodeTURNGrants(b)
		if err != nil {
			return err
		}
		select {
		case m.in <- g:
		default:
		}
		return nil
	})
	return m
}
func (m *turnManager) settlingAt(now time.Time) bool {
	return m != nil && now.Before(m.settling)
}

func (m *turnManager) close() { m.runtime.SetTURNControlHandler(nil); m.reset() }
func (m *turnManager) reset() {
	for _, s := range m.services {
		s.next.close()
		s.current.close()
	}
	clear(m.services)
	clear(m.wait)
	m.asked = 0
	m.retries = 0
	m.due = time.Time{}
	m.policy = turnsession.Demand{}
}
func (m *turnManager) step(now time.Time, active bool, d lanes.SchedulingDemand) {
	if !active {
		if len(m.services)+len(m.wait) > 0 || m.asked != 0 {
			m.reset()
		}
		return
	}
	for {
		select {
		case g := <-m.in:
			m.accept(now, g)
		default:
			goto drained
		}
	}
drained:
	for id, r := range m.wait {
		if !now.Before(r.until) {
			delete(m.wait, id)
			m.retry(now, r)
		}
	}
	mode := m.policy.Observe(now, d)
	if !m.disabled && mode > m.asked && m.retries <= len(turnBackoff) && !now.Before(m.due) && !m.pending("") {
		m.request(now, mode, "")
	}
	for id, service := range m.services {
		m.maintainService(now, id, service, d.UnfinishedBytes > 0 || d.NetworkWaitingBytes > 0)
	}
}

func (m *turnManager) maintainService(now time.Time, id string, s *turnService, networkDemand bool) {
	if s.next != nil {
		if signalled(s.next.ready) && !signalled(s.next.done) {
			s.current.close()
			s.current, s.next = s.next, nil
			s.retries = 0
			m.settling = now.Add(2 * time.Second)
		} else if signalled(s.next.done) {
			s.next.close()
			s.next = nil
			m.retry(now, requestState{service: id})
		}
	}
	l := s.current
	if l != nil && !l.counted && signalled(l.ready) {
		l.counted = true
		s.retries = 0
		m.settling = now.Add(2 * time.Second)
	}
	if l == nil || s.next != nil {
		return
	}
	if l.stop != nil && signalled(l.done) {
		l.retire()
		m.settling = now.Add(2 * time.Second)
		m.retry(now, requestState{service: id})
	}
	if l.stop != nil && !signalled(l.ready) {
		return
	}
	due := l.stop == nil || (!l.refresh.IsZero() && !now.Before(l.refresh))
	if due && networkDemand && !m.disabled && s.retries <= len(turnBackoff) && !now.Before(s.due) && !m.pending(id) {
		m.request(now, p.TURNAll, id)
	}
}

func (m *turnManager) pending(id string) bool {
	for _, r := range m.wait {
		if r.service == id {
			return true
		}
	}
	return false
}
func (m *turnManager) retry(now time.Time, r requestState) {
	retries, due := &m.retries, &m.due
	if r.service != "" {
		s := m.services[r.service]
		if s == nil {
			return
		}
		retries, due = &s.retries, &s.due
	}
	if *retries < len(turnBackoff) {
		*due = now.Add(turnBackoff[*retries])
	}
	*retries++
}
func (m *turnManager) request(now time.Time, mode p.TURNMode, service string) {
	m.serial++
	d := p.TURNDemand{RequestID: m.serial, Mode: mode}
	if service != "" {
		d.ServiceIDs = []string{service}
		m.trace.Event("turn_refresh_requested", service, diag.Fields{"request_id": diag.Uint(d.RequestID)})
	}
	body, _ := p.EncodeTURNDemand(d)
	m.wait[d.RequestID] = requestState{mode, service, now.Add(grantResponseTimeout)}
	m.trace.Event("turn_demand_sent", service, diag.Fields{"request_id": diag.Uint(d.RequestID), "mode": diag.Uint(uint64(mode))})
	m.owner.work.Go(func() {
		ctx, cancel := context.WithTimeout(m.owner.ctx, grantResponseTimeout)
		defer cancel()
		_ = m.runtime.SendTURNControl(ctx, body)
	})
}
func (m *turnManager) accept(now time.Time, g p.TURNGrants) {
	r, ok := m.wait[g.RequestID]
	if !ok {
		return
	}
	delete(m.wait, g.RequestID)
	m.trace.Event("turn_grants_received", r.service, diag.Fields{"request_id": diag.Uint(g.RequestID), "status": diag.Uint(uint64(g.Status)), "has_fallback": diag.Bool(g.HasFallback)})
	if g.Status == p.TURNUnconfigured {
		m.disabled = true
		return
	}
	if g.Status == p.TURNRetryLater {
		m.retry(now, r)
		return
	}
	if r.service == "" {
		m.asked = max(m.asked, r.mode)
		m.retries = 0
	} else if len(g.Grants) == 0 {
		m.retry(now, r)
	}
	for _, grant := range g.Grants {
		id := grant.ServiceID
		s := m.services[id]
		if r.service != "" && r.service != id {
			continue
		}
		if s == nil {
			if r.service != "" || len(m.services) >= p.MaxGrantsPerMessage {
				continue
			}
			s = &turnService{current: &turnLeg{}}
			m.services[id] = s
		} else if r.service == "" {
			continue
		}
		target := s.current
		if target.stop != nil && signalled(target.ready) && m.runtime.AttachedLanes() == 1 {
			slot, err := m.pool.Acquire()
			if err != nil {
				m.retry(now, r)
				continue
			}
			s.next = &turnLeg{slot: slot}
			target = s.next
		} else {
			target.retire()
		}
		if target.slot == nil {
			slot, err := m.pool.Acquire()
			if err != nil {
				m.retry(now, requestState{service: id})
				continue
			}
			target.slot = slot
		}
		m.start(now, target, grant)
	}
}
func (m *turnManager) start(now time.Time, l *turnLeg, g p.TURNGrant) {
	ctx, cancel := context.WithCancel(m.owner.ctx)
	l.stop, l.ready, l.done = cancel, make(chan struct{}), make(chan struct{})
	l.lane = session.LaneIdentity{}
	l.counted = false
	l.refresh = time.Time{}
	if g.ValidForMS > 0 {
		ttl := time.Duration(min(g.ValidForMS, uint64(86400000))) * time.Millisecond
		l.refresh = now.Add(ttl - min(ttl/4, renewalLead))
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
		if !ok {
			return
		}
		defer m.runtime.DetachLane(lane)
		done, ok := m.runtime.LaneDone(lane)
		if !ok {
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
