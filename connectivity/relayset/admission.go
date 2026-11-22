package relayset

import (
	"context"
	"sync"
	"time"

	"github.com/windshare/windshare/connectivity/relayplan"
)

// AttemptControl limits individual handshakes, leaving retry clocks and resume
// authority with the endpoint. Waiting for capacity must not consume a dial timeout.
type AttemptControl interface {
	Begin(context.Context) (AttemptPermit, error)
	Unavailable()
}

type AttemptPermit interface {
	Admit() bool
	Release()
}

// NetworkAdmissions coordinates only directory candidates. Configured endpoints
// keep independent lifetimes, and no successful published connection is evicted.
type NetworkAdmissions struct {
	mu            sync.Mutex
	candidates    []relayplan.Candidate
	policy        relayplan.Policy
	states        []admissionState
	changed       chan struct{}
	primary       int
	inflight      int
	active        int
	clock         Clock
	serverRetryAt func(string) time.Time
	observe       func(Admission)
}

const (
	AdmissionAdmitted    = "admitted"
	AdmissionParked      = "parked"
	AdmissionUnavailable = "unavailable"
)

const (
	AdmissionReasonPrimary         = "primary"
	AdmissionReasonBackup          = "backup"
	AdmissionReasonPermitReleased  = "permit_released"
	AdmissionReasonTargetReached   = "target_reached"
	AdmissionReasonBackupNotChosen = "backup_not_selected"
	AdmissionReasonPrimaryLost     = "primary_lost"
	AdmissionReasonBackupLost      = "backup_lost"
)

type Admission struct {
	Endpoint, Primary, State, Reason string
	Count                            int
}

func (a *NetworkAdmissions) Observe(observer func(Admission)) {
	a.mu.Lock()
	a.observe = observer
	a.mu.Unlock()
}

func (a *NetworkAdmissions) record(index int, state, reason string) {
	if a.observe == nil {
		return
	}
	primary := ""
	if a.primary >= 0 {
		primary = a.candidates[a.primary].URL
	}
	a.observe(Admission{Endpoint: a.candidates[index].URL, Primary: primary, State: state, Reason: reason, Count: a.active})
}

type admissionState struct {
	eligible, admitted bool
	notBefore          time.Time
}
type admissionControl struct {
	owner *NetworkAdmissions
	index int
}
type admissionPermit struct {
	control  admissionControl
	released bool
}

func NewNetworkAdmissions(plan relayplan.Plan, clock Clock, serverRetryAt func(string) time.Time) *NetworkAdmissions {
	if clock == nil {
		clock = SystemClock{}
	}
	a := &NetworkAdmissions{clock: clock, serverRetryAt: serverRetryAt, candidates: append([]relayplan.Candidate(nil), plan.Network...), policy: plan.Policy,
		states: make([]admissionState, len(plan.Network)), changed: make(chan struct{}), primary: -1}
	started := clock.Now()
	for i, candidate := range a.candidates {
		at := candidate.ServerRetryAt
		if !plan.Decision.CooldownFallback && candidate.CooldownUntil.After(at) {
			at = candidate.CooldownUntil
		}
		a.states[i] = admissionState{notBefore: at, eligible: !at.After(started)}
	}
	return a
}

func (a *NetworkAdmissions) Control(url string) AttemptControl {
	for i, candidate := range a.candidates {
		if candidate.URL == url {
			return admissionControl{owner: a, index: i}
		}
	}
	return nil
}

func (c admissionControl) Begin(ctx context.Context) (AttemptPermit, error) {
	a := c.owner
	a.mu.Lock()
	state := &a.states[c.index]
	for {
		if err := ctx.Err(); err != nil {
			a.states[c.index].eligible = false
			a.notify()
			a.mu.Unlock()
			return nil, err
		}
		at := state.notBefore
		// Another share may receive an HTTP deadline after this link is frozen.
		// Recheck at admission, before spending a dial slot or reserving a ticket.
		if a.serverRetryAt != nil {
			if latest := a.serverRetryAt(a.candidates[c.index].Identity); latest.After(at) {
				at = latest
			}
		}
		delay := at.Sub(a.clock.Now())
		if eligible := delay <= 0; state.eligible != eligible {
			state.eligible = eligible
			a.notify()
		}
		if state.eligible && a.active < relayplan.TargetNetworkRegistrations && a.inflight < relayplan.MaximumConcurrentAttempts &&
			(a.primary < 0 || a.backup() == c.index) {
			a.inflight++
			a.mu.Unlock()
			return &admissionPermit{control: c}, nil
		}
		changed := a.changed
		a.mu.Unlock()
		var err error
		if delay > 0 {
			err = a.clock.Wait(ctx, delay, changed)
		} else {
			select {
			case <-ctx.Done():
			case <-changed:
			}
		}
		a.mu.Lock()
		if err != nil {
			state.eligible = false
			a.notify()
			a.mu.Unlock()
			return nil, err
		}
	}
}

func (c admissionControl) Unavailable() {
	a := c.owner
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.states[c.index].admitted {
		return
	}
	a.states[c.index].admitted = false
	a.active--
	reason := AdmissionReasonBackupLost
	if a.primary == c.index {
		reason = AdmissionReasonPrimaryLost
		a.releaseSoftCooldown()
		a.primary = -1
		for i, state := range a.states {
			if state.admitted {
				a.primary = i
				break
			}
		}
	}
	a.record(c.index, AdmissionUnavailable, reason)
	a.notify()
}

func (p *admissionPermit) Admit() bool {
	a, index := p.control.owner, p.control.index
	a.mu.Lock()
	defer a.mu.Unlock()
	if p.released {
		a.record(index, AdmissionParked, AdmissionReasonPermitReleased)
		return false
	}
	if a.active >= relayplan.TargetNetworkRegistrations {
		a.record(index, AdmissionParked, AdmissionReasonTargetReached)
		return false
	}
	if a.primary >= 0 && a.backup() != index {
		a.record(index, AdmissionParked, AdmissionReasonBackupNotChosen)
		return false
	}
	reason := AdmissionReasonBackup
	if a.primary < 0 {
		a.primary = index
		reason = AdmissionReasonPrimary
	}
	a.states[index].admitted = true
	a.active++
	a.record(index, AdmissionAdmitted, reason)
	a.notify()
	return true
}

func (p *admissionPermit) Release() {
	a, index := p.control.owner, p.control.index
	a.mu.Lock()
	defer a.mu.Unlock()
	if p.released {
		return
	}
	p.released = true
	a.inflight--
	if !a.states[index].admitted {
		a.releaseSoftCooldown()
	}
	a.states[index].eligible = false
	a.notify()
}

// A failed, unauthorized or lost primary frees its published backup to dial.
// Server deadlines still constrain that endpoint across all shares.
func (a *NetworkAdmissions) releaseSoftCooldown() {
	for i, candidate := range a.candidates {
		a.states[i].notBefore = candidate.ServerRetryAt
	}
}

// Backoff candidates are absent here: their lifecycle re-enters eligibility only
// after its existing retry clock expires, allowing another candidate to take over.
func (a *NetworkAdmissions) backup() int {
	var candidates []relayplan.Candidate
	var indexes []int
	for i, state := range a.states {
		if state.eligible && !state.admitted {
			candidates = append(candidates, a.candidates[i])
			indexes = append(indexes, i)
		}
	}
	if len(candidates) == 0 {
		return -1
	}
	return indexes[relayplan.BackupIndex(a.candidates[a.primary], candidates, a.policy)]
}

func (a *NetworkAdmissions) notify() { close(a.changed); a.changed = make(chan struct{}) }
