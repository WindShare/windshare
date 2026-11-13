package relayset

import (
	"context"
	"sync"

	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/connectivity/senderrelay"
)

// NetworkAdmissions coordinates only directory candidates. Configured endpoints
// keep independent lifetimes, and no successful published connection is evicted.
type NetworkAdmissions struct {
	mu         sync.Mutex
	candidates []relayplan.Candidate
	policy     relayplan.Policy
	states     []admissionState
	changed    chan struct{}
	primary    int
	inflight   int
	active     int
	observe    func(Admission)
}

type Admission struct {
	Endpoint, Primary, State string
	Count                    int
}

func (a *NetworkAdmissions) Observe(observer func(Admission)) {
	a.mu.Lock()
	a.observe = observer
	a.mu.Unlock()
}

func (a *NetworkAdmissions) record(index int, state string) {
	if a.observe == nil {
		return
	}
	primary := ""
	if a.primary >= 0 {
		primary = a.candidates[a.primary].URL
	}
	a.observe(Admission{Endpoint: a.candidates[index].URL, Primary: primary, State: state, Count: a.active})
}

type admissionState struct{ eligible, attempting, admitted bool }
type admissionControl struct {
	owner *NetworkAdmissions
	index int
}
type admissionPermit struct {
	control  admissionControl
	released bool
}

func NewNetworkAdmissions(plan relayplan.Plan) *NetworkAdmissions {
	a := &NetworkAdmissions{candidates: append([]relayplan.Candidate(nil), plan.Network...), policy: plan.Policy,
		states: make([]admissionState, len(plan.Network)), changed: make(chan struct{}), primary: -1}
	for i := range a.states {
		a.states[i].eligible = true
	}
	return a
}

func (a *NetworkAdmissions) Control(url string) senderrelay.AttemptControl {
	for i, candidate := range a.candidates {
		if candidate.URL == url {
			return admissionControl{owner: a, index: i}
		}
	}
	return nil
}

func (c admissionControl) Begin(ctx context.Context) (senderrelay.AttemptPermit, error) {
	a := c.owner
	a.mu.Lock()
	a.states[c.index].eligible = true
	a.notify()
	for {
		if err := ctx.Err(); err != nil {
			a.states[c.index].eligible = false
			a.notify()
			a.mu.Unlock()
			return nil, err
		}
		if a.active < relayplan.TargetNetworkRegistrations && a.inflight < relayplan.MaximumConcurrentAttempts &&
			(a.primary < 0 || a.backup() == c.index) {
			a.states[c.index].attempting = true
			a.inflight++
			a.mu.Unlock()
			return &admissionPermit{control: c}, nil
		}
		changed := a.changed
		a.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-changed:
		}
		a.mu.Lock()
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
	if a.primary == c.index {
		a.primary = -1
		for i, state := range a.states {
			if state.admitted {
				a.primary = i
				break
			}
		}
	}
	a.record(c.index, "unavailable")
	a.notify()
}

func (p *admissionPermit) Admit() bool {
	a, index := p.control.owner, p.control.index
	a.mu.Lock()
	defer a.mu.Unlock()
	if p.released || a.active >= relayplan.TargetNetworkRegistrations || (a.primary >= 0 && a.backup() != index) {
		a.record(index, "parked")
		return false
	}
	a.states[index].admitted = true
	a.active++
	if a.primary < 0 {
		a.primary = index
	}
	a.record(index, "admitted")
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
	a.states[index].attempting = false
	a.states[index].eligible = false
	a.notify()
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
