package peerset

import (
	"context"
	"time"

	"github.com/windshare/windshare/connectivity/turnsession"
	"github.com/windshare/windshare/connectivity/v2signal"
	diag "github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/peerslot"
	p "github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/turngrant"
	"github.com/windshare/windshare/core/transfer/lanes"
)

const (
	grantResponseTimeout = 8 * time.Second
	turnSettlingPeriod   = 2 * time.Second
)

var turnBackoff = [...]time.Duration{2 * time.Second, 10 * time.Second, 30 * time.Second}

type turnRuntime interface {
	DetachLane(session.LaneIdentity) bool
	SetTURNControlHandler(func(context.Context, []byte) error)
	SendTURNControl(context.Context, []byte) error
	UsableLaneDone(session.LaneIdentity) (<-chan struct{}, bool)
	RetireLaneIfCovered(session.LaneIdentity, *session.LaneIdentity) bool
}

type turnRetry struct {
	failures int
	due      time.Time
}

func (r *turnRetry) failed(now time.Time) {
	if r.failures < len(turnBackoff) {
		r.due = now.Add(turnBackoff[r.failures])
	}
	if !r.exhausted() {
		r.failures++
	}
}

func (r turnRetry) exhausted() bool          { return r.failures > len(turnBackoff) }
func (r turnRetry) ready(now time.Time) bool { return !r.exhausted() && !now.Before(r.due) }

type turnService struct {
	current, next *turnLeg
	retry         turnRetry
}

type fullTURNDemand struct {
	mode     p.TURNMode
	complete bool
	retry    turnRetry
}

// Escalation opens its own bounded budget. Once fallback was requested only
// renewed fallback pressure can spend that budget on another full request.
func (f fullTURNDemand) needed(now time.Time, mode p.TURNMode) bool {
	return mode > f.mode || (mode != 0 && mode == f.mode && !f.complete && f.retry.ready(now))
}

type requestState struct {
	mode    p.TURNMode
	service string
	until   time.Time
}
type turnManager struct {
	owner     *ReceiverSession
	trace     turngrant.Trace
	negotiate func(context.Context, v2signal.Binding, p.TURNGrant) (Attempt, error)
	runtime   turnRuntime
	pool      *peerslot.Pool
	paths     [peerslot.Count]v2signal.PeerPathID
	in        chan p.TURNGrants
	serial    uint64
	wait      map[uint64]requestState
	services  map[string]*turnService
	exhausted map[string]struct{}
	policy    turnsession.Demand
	full      fullTURNDemand
	disabled  bool
	settling  time.Time
}

func newTURNManager(s *ReceiverSession, r turnRuntime) *turnManager {
	id := receiverSessionID(s.config.Lanes)
	s.owner.mu.Lock()
	pool := s.owner.slotPoolLocked(id)
	s.owner.mu.Unlock()
	config := s.config
	m := &turnManager{
		owner: s, trace: turngrant.Trace{Sink: config.Trace, Session: id}, runtime: r, pool: pool,
		in: make(chan p.TURNGrants, 16), wait: make(map[uint64]requestState),
		services: make(map[string]*turnService), exhausted: make(map[string]struct{}),
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
	clear(m.exhausted)
	clear(m.wait)
	m.full = fullTURNDemand{}
	m.policy = turnsession.Demand{}
	m.settling = time.Time{}
}

func (m *turnManager) step(now time.Time, active bool, d lanes.SchedulingDemand) {
	if !active || m.owner.ctx.Err() != nil {
		m.reset()
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
			m.failed(now, r)
		}
	}
	if !m.disabled {
		mode := m.policy.Observe(now, d)
		if len(m.services) < p.MaxGrantsPerMessage && !m.pending("") && m.full.needed(now, mode) {
			m.request(now, mode, "")
		}
	}
	for id, service := range m.services {
		m.maintainService(now, id, service, d.UnfinishedBytes > 0 || d.NetworkWaitingBytes > 0)
	}
}

func (m *turnManager) settleReplacement(now time.Time, s *turnService) {
	next := s.next
	if next == nil {
		return
	}
	ended := signalled(next.done)
	if !ended && !signalled(next.ready) {
		return
	}
	s.next = nil
	// A successor can start closing between admission and this handoff.
	if !ended && m.runtime.RetireLaneIfCovered(s.current.lane, &next.lane) {
		s.current.close()
		s.current = next
		return
	}
	next.close()
	s.retry.failed(now)
}

func (m *turnManager) maintainService(now time.Time, id string, s *turnService, networkDemand bool) {
	m.settleReplacement(now, s)
	l := s.current
	if l.stop != nil && signalled(l.done) {
		l.retire()
		m.settling = now.Add(turnSettlingPeriod)
		s.retry.failed(now)
	} else if l.stop != nil && !l.counted && signalled(l.ready) {
		l.counted = true
		s.retry = turnRetry{}
		m.settling = now.Add(turnSettlingPeriod)
	}
	if l.stop == nil && s.next == nil && s.retry.exhausted() && !m.pending(id) {
		l.close()
		// New service IDs can appear after discovery. Remember exhausted IDs
		// across the bounded full requests without charging them a service place.
		delete(m.services, id)
		m.exhausted[id] = struct{}{}
		return
	}
	if s.next != nil || (l.stop != nil && !signalled(l.ready)) {
		return
	}
	due := l.stop == nil || (!l.refresh.IsZero() && !now.Before(l.refresh))
	if due && networkDemand && !m.disabled && s.retry.ready(now) && !m.pending(id) {
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
func (m *turnManager) failed(now time.Time, r requestState) {
	if r.service == "" {
		m.full.retry.failed(now)
	} else if s := m.services[r.service]; s != nil {
		s.retry.failed(now)
	}
}

func (m *turnManager) request(now time.Time, mode p.TURNMode, service string) {
	if service == "" && mode > m.full.mode {
		m.full = fullTURNDemand{mode: mode}
	}
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
	m.trace.Event("turn_grants_received", r.service, diag.Fields{
		"request_id": diag.Uint(g.RequestID), "mode": diag.Uint(uint64(r.mode)),
		"status": diag.Uint(uint64(g.Status)), "has_fallback": diag.Bool(g.HasFallback),
	})
	if g.Status == p.TURNUnconfigured {
		m.disabled = true
		return
	}
	if g.Status == p.TURNRetryLater {
		m.failed(now, r)
		return
	}
	matched := false
	for _, grant := range g.Grants {
		if r.service == "" || r.service == grant.ServiceID {
			m.acceptService(now, grant, r.service != "")
			matched = true
		}
	}
	if r.service == "" {
		if g.Status == p.TURNPartialRetryable {
			m.full.retry.failed(now)
		} else {
			m.full.complete = true
		}
	} else if !matched {
		m.failed(now, r)
	}
}

func (m *turnManager) acceptService(now time.Time, grant p.TURNGrant, refresh bool) {
	s := m.services[grant.ServiceID]
	if s == nil {
		if _, spent := m.exhausted[grant.ServiceID]; spent || refresh || len(m.services) >= p.MaxGrantsPerMessage {
			return
		}
		s = &turnService{current: &turnLeg{}}
		m.services[grant.ServiceID] = s
	} else if !refresh || s.next != nil {
		// Full responses can replay live grants and must not renew their lanes.
		return
	}
	target := s.current
	protect := target.stop != nil && signalled(target.ready) && !signalled(target.done) &&
		!m.runtime.RetireLaneIfCovered(target.lane, nil)
	if protect {
		target = &turnLeg{}
	} else {
		target.retire()
	}
	if target.slot == nil {
		slot, err := m.pool.Acquire()
		if err != nil {
			s.retry.failed(now)
			return
		}
		target.slot = slot
	}
	if protect {
		s.next = target
	}
	m.start(now, target, grant)
}
