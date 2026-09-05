package nativepeer

import (
	"context"
	"errors"
	"github.com/windshare/windshare/connectivity/icepolicy"
	"sync"
	"time"
)

// These process limits mirror browser tab admission. They survive manager,
// authenticated-session and network-generation replacement.
const (
	ProcessConcurrentAttempts             = 4
	ProcessStartsPerWindow                = 16
	ProcessSTUNEndpointsPerWindow         = 32
	ProcessQueuedAttempts                 = 64
	ProcessAdmissionWindow                = time.Minute
	ProcessAttemptBudget                  = 85 * time.Second
	ProcessActiveTimePerWindow            = ProcessConcurrentAttempts * ProcessAttemptBudget
	ProcessQueueBudget                    = time.Minute
	MinimumPreparationOpportunity         = 50 * time.Second
	ProcessMaximumSTUNEndpointsPerAttempt = icepolicy.EndpointsPerProfile
)

var ErrProcessAdmission = errors.New("native process attempt admission unavailable")

type AdmissionKind string

const (
	AdmissionQueued   AdmissionKind = "queued"
	AdmissionGranted  AdmissionKind = "granted"
	AdmissionReleased AdmissionKind = "released"
	AdmissionRejected AdmissionKind = "rejected"
)

type AdmissionFacts struct {
	Kind                           AdmissionKind
	At                             time.Time
	Wait                           time.Duration
	Active, Queued                 int
	StartsRemaining, STUNRemaining float64
	ActiveTimeRemaining            time.Duration
}

// AdmissionClock permits deterministic refill and queue tests without sleeping.
type AdmissionClock struct {
	Now       func() time.Time
	AfterFunc func(time.Duration, func()) func()
}
type ProcessAdmission struct {
	mu                            sync.Mutex
	clock                         AdmissionClock
	updated                       time.Time
	starts, endpoints, activeTime float64
	active                        int
	queue                         []*admissionWaiter
	timer                         func()
}
type admissionWaiter struct {
	owner     *NativePeerConnectivity
	endpoints int
	ready     chan struct{}
	started   time.Time
	permit    *attemptPermit
	observe   func(AdmissionFacts)
}
type attemptPermit struct {
	gate    *ProcessAdmission
	started time.Time
	waiter  *admissionWaiter
	once    sync.Once
}

func NewProcessAdmission(clock AdmissionClock) *ProcessAdmission {
	if clock.Now == nil {
		clock.Now = time.Now
	}
	if clock.AfterFunc == nil {
		clock.AfterFunc = func(d time.Duration, f func()) func() { timer := time.AfterFunc(d, f); return func() { timer.Stop() } }
	}
	return &ProcessAdmission{clock: clock, updated: clock.Now(), starts: ProcessStartsPerWindow, endpoints: ProcessSTUNEndpointsPerWindow, activeTime: float64(ProcessActiveTimePerWindow)}
}

var processAdmission = NewProcessAdmission(AdmissionClock{})

func (g *ProcessAdmission) acquire(ctx context.Context, owner *NativePeerConnectivity, endpoints int, observe func(AdmissionFacts)) (*attemptPermit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if endpoints < 0 || endpoints > ProcessMaximumSTUNEndpointsPerAttempt {
		return nil, ErrProcessAdmission
	}
	w := &admissionWaiter{owner: owner, endpoints: endpoints, ready: make(chan struct{}), started: g.clock.Now(), observe: observe}
	g.mu.Lock()
	if len(g.queue) >= ProcessQueuedAttempts {
		g.emitLocked(w, AdmissionRejected)
		g.mu.Unlock()
		return nil, ErrProcessAdmission
	}
	g.queue = append(g.queue, w)
	g.emitLocked(w, AdmissionQueued)
	g.drainLocked()
	g.mu.Unlock()
	select {
	case <-w.ready:
		if err := ctx.Err(); err != nil {
			w.permit.release()
			return nil, err
		}
		return w.permit, nil
	case <-ctx.Done():
		g.mu.Lock()
		if w.permit == nil {
			for i, item := range g.queue {
				if item == w {
					g.queue = append(g.queue[:i], g.queue[i+1:]...)
					break
				}
			}
			g.emitLocked(w, AdmissionRejected)
			g.drainLocked()
		}
		permit := w.permit
		g.mu.Unlock()
		if permit != nil {
			permit.release()
		}
		return nil, context.Cause(ctx)
	}
}
func (g *ProcessAdmission) refillLocked() {
	now := g.clock.Now()
	elapsed := max(now.Sub(g.updated), 0)
	g.starts = min(ProcessStartsPerWindow, g.starts+float64(elapsed)*ProcessStartsPerWindow/float64(ProcessAdmissionWindow))
	g.endpoints = min(ProcessSTUNEndpointsPerWindow, g.endpoints+float64(elapsed)*ProcessSTUNEndpointsPerWindow/float64(ProcessAdmissionWindow))
	g.activeTime = min(float64(ProcessActiveTimePerWindow), g.activeTime+float64(elapsed)*float64(ProcessActiveTimePerWindow)/float64(ProcessAdmissionWindow))
	if now.After(g.updated) {
		g.updated = now
	}
}
func (g *ProcessAdmission) drainLocked() {
	if g.timer != nil {
		g.timer()
		g.timer = nil
	}
	g.refillLocked()
	for len(g.queue) > 0 && g.active < ProcessConcurrentAttempts {
		index := 0
		w := g.queue[index]
		wait := max((1-g.starts)*float64(ProcessAdmissionWindow)/ProcessStartsPerWindow,
			(float64(w.endpoints)-g.endpoints)*float64(ProcessAdmissionWindow)/ProcessSTUNEndpointsPerWindow,
			(float64(ProcessAttemptBudget)-g.activeTime)*float64(ProcessAdmissionWindow)/float64(ProcessActiveTimePerWindow))
		if wait > 0 {
			g.timer = g.clock.AfterFunc(max(time.Duration(wait), time.Nanosecond), func() { g.mu.Lock(); g.drainLocked(); g.mu.Unlock() })
			return
		}
		g.queue = append(g.queue[:index], g.queue[index+1:]...)
		// Rotate all remaining requests from this owner behind the other owners.
		var same []*admissionWaiter
		var other []*admissionWaiter
		for _, pending := range g.queue {
			if pending.owner == w.owner {
				same = append(same, pending)
			} else {
				other = append(other, pending)
			}
		}
		other = append(other, same...)
		g.queue = other
		g.active++
		g.starts--
		g.endpoints -= float64(w.endpoints)
		g.activeTime -= float64(ProcessAttemptBudget)
		w.permit = &attemptPermit{gate: g, started: g.clock.Now(), waiter: w}
		g.emitLocked(w, AdmissionGranted)
		close(w.ready)
	}
}
func (p *attemptPermit) release() {
	if p == nil {
		return
	}
	p.once.Do(func() {
		g := p.gate
		g.mu.Lock()
		g.refillLocked()
		used := min(max(g.clock.Now().Sub(p.started), 0), ProcessAttemptBudget)
		g.active--
		g.activeTime = min(float64(ProcessActiveTimePerWindow), g.activeTime+float64(ProcessAttemptBudget-used))
		g.emitLocked(p.waiter, AdmissionReleased)
		g.drainLocked()
		g.mu.Unlock()
	})
}
func (g *ProcessAdmission) emitLocked(w *admissionWaiter, kind AdmissionKind) {
	if w.observe != nil {
		wait := max(g.clock.Now().Sub(w.started), 0)
		if w.permit != nil {
			wait = max(w.permit.started.Sub(w.started), 0)
		}
		w.observe(AdmissionFacts{Kind: kind, At: g.clock.Now(), Wait: wait, Active: g.active, Queued: len(g.queue), StartsRemaining: g.starts, STUNRemaining: g.endpoints, ActiveTimeRemaining: time.Duration(g.activeTime)})
	}
}
