package relayset

import (
	"context"
	"slices"
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

// AttemptPermit tracks one in-flight handshake slot. ReleaseFailed records an
// actual handshake failure for in-share backup ranking, whereas Release frees
// the slot without penalizing the candidate (admitted, parked, auth wait, cancel).
type AttemptPermit interface {
	Admit() bool
	Release()
	ReleaseFailed()
}

const supplementalCommunityRegistrations = 1

// NetworkAdmissions reserves the official pair and one independent community
// admission within the remaining MaximumEndpoints budget, or up to the normal
// network target pair when the initial wave is community-only. Explicitly
// configured endpoints own their own registration lifetimes.
type NetworkAdmissions struct {
	mu              sync.Mutex
	candidates      []relayplan.Candidate
	policy          relayplan.Policy
	states          []admissionState
	changed         chan struct{}
	primary         int
	remaining       int
	target          int
	communityTarget int
	inflight        int
	communityActive int
	active          int
	clock           Clock
	serverRetryAt   func(string) time.Time
	observe         func(Admission)
}

const (
	AdmissionAdmitted    = "admitted"
	AdmissionParked      = "parked"
	AdmissionUnavailable = "unavailable"
)

const (
	AdmissionReasonPrimary         = "primary"
	AdmissionReasonBackup          = "backup"
	AdmissionReasonCommunity       = "community"
	AdmissionReasonPermitReleased  = "permit_released"
	AdmissionReasonTargetReached   = "target_reached"
	AdmissionReasonBackupNotChosen = "backup_not_selected"
	AdmissionReasonPrimaryLost     = "primary_lost"
	AdmissionReasonBackupLost      = "backup_lost"
	AdmissionReasonCommunityLost   = "community_lost"
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
	a.observe(Admission{Endpoint: a.candidates[index].URL, Primary: primary, State: state, Reason: reason, Count: a.totalActive()})
}

type candidateHealth uint8

const (
	candidateHealthInitial candidateHealth = iota
	candidateHealthFailed
	candidateHealthCleared
)

type admissionState struct {
	eligible, admitted bool
	notBefore          time.Time
	health             candidateHealth
}
type admissionControl struct {
	owner *NetworkAdmissions
	index int
}
type admissionPermit struct {
	control   admissionControl
	completed bool
	released  bool
}

func NewNetworkAdmissions(plan relayplan.Plan, clock Clock, serverRetryAt func(string) time.Time) *NetworkAdmissions {
	if clock == nil {
		clock = SystemClock{}
	}
	remaining := max(0, MaximumEndpoints-len(plan.Configured))
	target := min(relayplan.TargetNetworkRegistrations, remaining)
	// Community routes stay a single supplemental registration alongside an
	// official initial wave, and scale up to the normal target pair when no
	// official routes are dialable in the initial wave (pure-community or
	// server-deferred official fallback).
	communityTarget := min(supplementalCommunityRegistrations, remaining)
	if !slices.ContainsFunc(plan.Network, func(c relayplan.Candidate) bool { return c.Source != relayplan.Community }) {
		communityTarget = target
	}
	candidates := plan.NetworkCandidates()
	a := &NetworkAdmissions{clock: clock, serverRetryAt: serverRetryAt, candidates: candidates, policy: plan.Policy,
		remaining: remaining, target: target, communityTarget: communityTarget,
		states: make([]admissionState, len(candidates)), changed: make(chan struct{}), primary: -1}
	for i, candidate := range a.candidates {
		at := candidate.ServerRetryAt
		if !plan.Decision.CooldownFallback && candidate.CooldownUntil.After(at) {
			at = candidate.CooldownUntil
		}
		// Eligibility is entered only from Begin so candidates that fail or stop
		// before dialing cannot occupy backup ranking.
		a.states[i] = admissionState{notBefore: at}
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
		if state.eligible && a.canAdmit(c.index) && a.inflight < relayplan.MaximumConcurrentAttempts {
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
	if a.candidates[c.index].Source == relayplan.Community {
		a.communityActive--
		a.record(c.index, AdmissionUnavailable, AdmissionReasonCommunityLost)
		a.notify()
		return
	}
	a.active--
	reason := AdmissionReasonBackupLost
	if a.primary == c.index {
		reason = AdmissionReasonPrimaryLost
		a.releaseSoftCooldown()
		a.primary = -1
		for i, state := range a.states {
			if state.admitted && a.candidates[i].Source != relayplan.Community {
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
	p.completed = true
	// Completing a handshake proves current reachability before backup ranking,
	// even if the connection ends up parked.
	a.states[index].health = candidateHealthCleared
	if a.candidates[index].Source == relayplan.Community {
		if !a.canAdmit(index) {
			a.record(index, AdmissionParked, AdmissionReasonTargetReached)
			return false
		}
		a.communityActive++
		a.states[index].admitted = true
		a.record(index, AdmissionAdmitted, AdmissionReasonCommunity)
		a.notify()
		return true
	}
	if a.active >= a.target || a.totalActive() >= a.remaining {
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

func (p *admissionPermit) Release() { p.release(false) }

func (p *admissionPermit) ReleaseFailed() { p.release(true) }

func (p *admissionPermit) release(failed bool) {
	a, index := p.control.owner, p.control.index
	a.mu.Lock()
	defer a.mu.Unlock()
	if p.released {
		return
	}
	p.released = true
	a.inflight--
	if !a.states[index].admitted {
		if failed && !p.completed {
			a.states[index].health = candidateHealthFailed
		}
		// Community admissions are isolated from the official pair, so a failed
		// or parked community attempt must not thaw a soft-cooled official backup.
		if a.candidates[index].Source != relayplan.Community {
			a.releaseSoftCooldown()
		}
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

// Backoff candidates stay ineligible while sleeping, and candidates whose
// handshake failed in this share lose priority once their retry clock wakes them.
func (a *NetworkAdmissions) backup() int {
	var candidates []relayplan.Candidate
	var indexes []int
	for i, state := range a.states {
		if state.eligible && !state.admitted && a.candidates[i].Source != relayplan.Community {
			candidates = append(candidates, a.candidateForBackup(i))
			indexes = append(indexes, i)
		}
	}
	if len(candidates) == 0 {
		return -1
	}
	return indexes[relayplan.BackupIndex(a.candidates[a.primary], candidates, a.policy)]
}

func (a *NetworkAdmissions) candidateForBackup(index int) relayplan.Candidate {
	candidate := a.candidates[index]
	switch a.states[index].health {
	case candidateHealthFailed:
		return candidate.WithRegistrationFailure()
	case candidateHealthCleared:
		return candidate.WithoutRegistrationFailure()
	default:
		return candidate
	}
}

func (a *NetworkAdmissions) totalActive() int {
	return a.active + a.communityActive
}

func (a *NetworkAdmissions) canAdmit(index int) bool {
	if a.totalActive() >= a.remaining {
		return false
	}
	if a.candidates[index].Source == relayplan.Community {
		return a.communityActive < a.communityTarget
	}
	return a.active < a.target && (a.primary < 0 || a.backup() == index)
}

func (a *NetworkAdmissions) notify() { close(a.changed); a.changed = make(chan struct{}) }
