package nativepeer

import (
	"context"
	"errors"
	"github.com/windshare/windshare/connectivity/icepolicy"
	"github.com/windshare/windshare/connectivity/socketauthority"
	"github.com/windshare/windshare/connectivity/v2signal"
	"slices"
	"sync"
	"time"
)

// These application limits mirror browser tab admission. The injected authority
// survives manager, authenticated-session and network-generation replacement.
const (
	ConcurrentAttempts             = 4
	DirectAttemptReserve           = 1
	StartsPerWindow                = 16
	STUNEndpointsPerWindow         = 32
	MaximumQueuedAttempts          = 64
	AdmissionWindow                = time.Minute
	AttemptBudget                  = 85 * time.Second
	ActiveTimePerWindow            = ConcurrentAttempts * AttemptBudget
	QueueBudget                    = time.Minute
	MinimumPreparationOpportunity  = 50 * time.Second
	MaximumSTUNEndpointsPerAttempt = icepolicy.EndpointsPerProfile
)

var ErrAdmission = errors.New("native attempt admission unavailable")

type AdmissionKind string

const (
	AdmissionQueued   AdmissionKind = "queued"
	AdmissionGranted  AdmissionKind = "granted"
	AdmissionReleased AdmissionKind = "released"
	AdmissionRejected AdmissionKind = "rejected"
)

type AdmissionFacts struct {
	Purpose                        v2signal.ConnectionPurpose
	Kind                           AdmissionKind
	At                             time.Time
	Wait                           time.Duration
	Active, Queued                 int
	StartsRemaining, STUNRemaining float64
	ActiveTimeRemaining            time.Duration
	SocketCapacity                 *socketauthority.CapacityError
}

// AdmissionClock permits deterministic refill and queue tests without sleeping.
type AdmissionClock struct {
	Now       func() time.Time
	AfterFunc func(time.Duration, func()) func()
}

// Admission belongs to the application; every native owner shares its allowances.
// Replacing a connection manager must not replenish the application's rate budget.
type Admission struct {
	optional                      []*optionalHolding
	mu                            sync.Mutex
	clock                         AdmissionClock
	updated                       time.Time
	starts, endpoints, activeTime float64
	active                        int
	queue                         []*admissionWaiter
	timer                         func()
}
type admissionWaiter struct {
	purpose   v2signal.ConnectionPurpose
	relayed   bool
	owner     *NativePeerConnectivity
	endpoints int
	ready     chan struct{}
	started   time.Time
	permit    *attemptPermit
	observe   func(AdmissionFacts)
	sockets   *socketauthority.Request
	capacity  *socketauthority.CapacityError
	err       error
}
type attemptPermit struct {
	gate    *Admission
	started time.Time
	waiter  *admissionWaiter
	once    sync.Once
	sockets *socketauthority.Reservation
}

func NewAdmission(clock AdmissionClock) *Admission {
	if clock.Now == nil {
		clock.Now = time.Now
	}
	if clock.AfterFunc == nil {
		clock.AfterFunc = func(d time.Duration, f func()) func() { timer := time.AfterFunc(d, f); return func() { timer.Stop() } }
	}
	return &Admission{clock: clock, updated: clock.Now(), starts: StartsPerWindow, endpoints: STUNEndpointsPerWindow, activeTime: float64(ActiveTimePerWindow)}
}

func (g *Admission) acquirePurpose(ctx context.Context, owner *NativePeerConnectivity, endpoints int, sockets *socketauthority.Request, observe func(AdmissionFacts), relayed bool, purpose v2signal.ConnectionPurpose) (*attemptPermit, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if endpoints < 0 || endpoints > MaximumSTUNEndpointsPerAttempt {
		return nil, ErrAdmission
	}
	w := &admissionWaiter{purpose: purpose, owner: owner, endpoints: endpoints, sockets: sockets, ready: make(chan struct{}), started: g.clock.Now(), observe: observe, relayed: relayed}
	g.mu.Lock()
	if len(g.queue) >= MaximumQueuedAttempts {
		g.emitLocked(w, AdmissionRejected)
		g.mu.Unlock()
		return nil, ErrAdmission
	}
	g.queue = append(g.queue, w)
	g.emitLocked(w, AdmissionQueued)
	g.mu.Unlock()
	for {
		// Sampling before dispatch prevents a release between a failed reservation
		// and sleep from stranding a waiter. No native or admission lock is held asleep.
		changed := sockets.Changes()
		g.mu.Lock()
		g.drainLocked()
		if purpose == v2signal.ThroughputExploration && w.permit == nil && w.err == nil {
			g.queue = slices.DeleteFunc(g.queue, func(entry *admissionWaiter) bool { return entry == w })
			g.emitLocked(w, AdmissionRejected)
			g.mu.Unlock()
			return nil, ErrAdmission
		}
		g.mu.Unlock()
		select {
		case <-w.ready:
			if err := context.Cause(ctx); err != nil {
				w.permit.release()
				return nil, err
			}
			return w.permit, w.err
		case <-changed:
			continue
		case <-ctx.Done():
			return nil, g.cancelWaiter(w, context.Cause(ctx))
		}
	}
}

func (g *Admission) cancelWaiter(w *admissionWaiter, cause error) error {
	g.mu.Lock()
	if w.permit == nil {
		g.queue = slices.DeleteFunc(g.queue, func(item *admissionWaiter) bool { return item == w })
		g.emitLocked(w, AdmissionRejected)
		g.drainLocked()
	}
	permit, capacity := w.permit, w.capacity
	g.mu.Unlock()
	permit.release()
	if capacity != nil && errors.Is(cause, context.DeadlineExceeded) {
		return errors.Join(capacity, cause)
	}
	return cause
}
func (g *Admission) refillLocked() {
	now := g.clock.Now()
	elapsed := max(now.Sub(g.updated), 0)
	g.starts = min(StartsPerWindow, g.starts+float64(elapsed)*StartsPerWindow/float64(AdmissionWindow))
	g.endpoints = min(STUNEndpointsPerWindow, g.endpoints+float64(elapsed)*STUNEndpointsPerWindow/float64(AdmissionWindow))
	g.activeTime = min(float64(ActiveTimePerWindow), g.activeTime+float64(elapsed)*float64(ActiveTimePerWindow)/float64(AdmissionWindow))
	if now.After(g.updated) {
		g.updated = now
	}
}
func (g *Admission) drainLocked() {
	if g.timer != nil {
		g.timer()
		g.timer = nil
	}
	g.refillLocked()
	slices.SortStableFunc(g.queue, admissionPriority)
	var nextRefill time.Duration
	defer func() {
		if nextRefill > 0 && len(g.queue) > 0 {
			g.timer = g.clock.AfterFunc(nextRefill, func() { g.mu.Lock(); g.drainLocked(); g.mu.Unlock() })
		}
	}()
	for index := 0; index < len(g.queue) && g.active < ConcurrentAttempts; {
		w := g.queue[index]
		reserve := 0
		if w.relayed || w.purpose == v2signal.ThroughputExploration {
			// Leave one whole opportunity for ordinary direct recovery, including
			// its start token and time budget, even during TURN replacement.
			reserve = DirectAttemptReserve
			if g.active >= ConcurrentAttempts-reserve {
				index++
				continue
			}
		}
		wait := max((float64(1+reserve)-g.starts)*float64(AdmissionWindow)/StartsPerWindow,
			(float64(w.endpoints)-g.endpoints)*float64(AdmissionWindow)/STUNEndpointsPerWindow,
			(float64(AttemptBudget)*float64(1+reserve)-g.activeTime)*float64(AdmissionWindow)/float64(ActiveTimePerWindow))
		if wait > 0 {
			delay := max(time.Duration(wait), time.Nanosecond)
			if nextRefill == 0 || delay < nextRefill {
				nextRefill = delay
			}
			if w.relayed {
				index++
				continue
			}
			return
		}
		reservation, err := g.reserveSocketsLocked(w)
		if _, blocked := errors.AsType[*socketauthority.CapacityError](err); blocked {
			// One sender's full socket pool cannot stall independent receivers.
			index++
			continue
		}
		g.queue = append(g.queue[:index], g.queue[index+1:]...)
		if err != nil {
			w.err = err
			g.emitLocked(w, AdmissionRejected)
			close(w.ready)
			continue
		}
		w.capacity = nil
		g.rotateOwnerLocked(w.owner)
		g.active++
		g.starts--
		g.endpoints -= float64(w.endpoints)
		g.activeTime -= float64(AttemptBudget)
		w.permit = &attemptPermit{gate: g, started: g.clock.Now(), waiter: w, sockets: reservation}
		g.emitLocked(w, AdmissionGranted)
		close(w.ready)
		index = 0
	}
}

func (g *Admission) reserveSocketsLocked(w *admissionWaiter) (*socketauthority.Reservation, error) {
	var reservation *socketauthority.Reservation
	var err error
	if w.purpose == v2signal.ThroughputExploration {
		reservation, err = w.sockets.ReserveOptional()
	} else {
		reservation, err = w.sockets.Reserve()
	}
	if capacity, blocked := errors.AsType[*socketauthority.CapacityError](err); blocked {
		if w.purpose != v2signal.ThroughputExploration {
			g.yieldOptionalLocked(w.sockets)
		}
		if w.capacity == nil || *w.capacity != *capacity {
			w.capacity = capacity
			g.emitLocked(w, AdmissionQueued)
		}
	}
	return reservation, err
}
func (g *Admission) rotateOwnerLocked(owner *NativePeerConnectivity) {
	// Rotate all remaining requests from this owner behind the other owners.
	var same, other []*admissionWaiter
	for _, pending := range g.queue {
		if pending.owner == owner {
			same = append(same, pending)
		} else {
			other = append(other, pending)
		}
	}
	other = append(other, same...)
	g.queue = other
	// Fairness rotates owners within each purpose class. It cannot promote an
	// optional attempt ahead of a queued initial connection or recovery.
	slices.SortStableFunc(g.queue, admissionPriority)
}

func admissionPriority(a, b *admissionWaiter) int {
	if (a.purpose == v2signal.ThroughputExploration) == (b.purpose == v2signal.ThroughputExploration) {
		return 0
	}
	if a.purpose == v2signal.ThroughputExploration {
		return 1
	}
	return -1
}
func (p *attemptPermit) release() {
	if p == nil {
		return
	}
	p.once.Do(func() {
		p.sockets.Close()
		g := p.gate
		g.mu.Lock()
		g.refillLocked()
		used := min(max(g.clock.Now().Sub(p.started), 0), AttemptBudget)
		g.active--
		g.activeTime = min(float64(ActiveTimePerWindow), g.activeTime+float64(AttemptBudget-used))
		g.emitLocked(p.waiter, AdmissionReleased)
		g.drainLocked()
		g.mu.Unlock()
	})
}
func (g *Admission) emitLocked(w *admissionWaiter, kind AdmissionKind) {
	if w.observe != nil {
		wait := max(g.clock.Now().Sub(w.started), 0)
		if w.permit != nil {
			wait = max(w.permit.started.Sub(w.started), 0)
		}
		var capacity *socketauthority.CapacityError
		if w.capacity != nil {
			// Observers must not retain an alias to the queue's decision state.
			snapshot := *w.capacity
			capacity = &snapshot
		}
		w.observe(AdmissionFacts{Purpose: w.purpose, Kind: kind, At: g.clock.Now(), Wait: wait, Active: g.active, Queued: len(g.queue), StartsRemaining: g.starts, STUNRemaining: g.endpoints, ActiveTimeRemaining: time.Duration(g.activeTime), SocketCapacity: capacity})
	}
}
