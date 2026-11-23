package peerset

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/connectivity/v2signal"
	diag "github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/session"
	p "github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/lanes"
)

type turnTestLane struct {
	usable bool
	done   chan struct{}
}

type turnTestRuntime struct {
	mu       sync.Mutex
	lanes    map[session.LaneIdentity]*turnTestLane
	requests chan p.TURNDemand
	handler  func(context.Context, []byte) error
}

func (r *turnTestRuntime) SetTURNControlHandler(h func(context.Context, []byte) error) { r.handler = h }
func (r *turnTestRuntime) SendTURNControl(_ context.Context, body []byte) error {
	demand, err := p.DecodeTURNDemand(body)
	if err == nil {
		r.requests <- demand
	}
	return err
}
func (r *turnTestRuntime) attach(id session.LaneIdentity) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lanes[id] = &turnTestLane{usable: true, done: make(chan struct{})}
}
func (r *turnTestRuntime) markClosing(id session.LaneIdentity) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lanes[id].usable = false
}
func (r *turnTestRuntime) UsableLaneDone(id session.LaneIdentity) (<-chan struct{}, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if lane := r.lanes[id]; lane != nil && lane.usable {
		return lane.done, true
	}
	return nil, false
}
func (r *turnTestRuntime) DetachLane(id session.LaneIdentity) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropLocked(id)
}
func (r *turnTestRuntime) dropLocked(id session.LaneIdentity) bool {
	if lane := r.lanes[id]; lane != nil {
		delete(r.lanes, id)
		close(lane.done)
		return true
	}
	return false
}
func (r *turnTestRuntime) RetireLaneIfCovered(id session.LaneIdentity, successor *session.LaneIdentity) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if successor != nil {
		if next := r.lanes[*successor]; *successor == id || next == nil || !next.usable {
			return false
		}
	} else if current := r.lanes[id]; current != nil && current.usable {
		covered := false
		for other, lane := range r.lanes {
			covered = covered || (other != id && lane.usable)
		}
		if !covered {
			return false
		}
	}
	r.dropLocked(id)
	return true
}

type testTURNAttempt struct {
	*fakeAttempt
	binding    v2signal.Binding
	grant      p.TURNGrant
	lane       session.LaneIdentity
	beforeLane func()
}

func (a *testTURNAttempt) Lane() (session.LaneIdentity, bool) {
	if a.beforeLane != nil {
		a.beforeLane()
	}
	return a.lane, signalled(a.ready)
}

type turnFixture struct {
	t        *testing.T
	origin   time.Time
	manager  *turnManager
	runtime  *turnTestRuntime
	cancel   context.CancelFunc
	attempts chan *testTURNAttempt
}

func newTURNFixture(t *testing.T) *turnFixture {
	owner, _ := New(Config{})
	ctx, cancel := context.WithCancel(t.Context())
	receiver := &ReceiverSession{owner: owner, ctx: ctx}
	runtime := &turnTestRuntime{lanes: make(map[session.LaneIdentity]*turnTestLane), requests: make(chan p.TURNDemand, 16)}
	manager := newTURNManager(receiver, runtime)
	f := &turnFixture{t: t, origin: time.Now(), manager: manager, runtime: runtime, cancel: cancel, attempts: make(chan *testTURNAttempt, 8)}
	var sequence atomic.Uint32
	manager.negotiate = func(_ context.Context, binding v2signal.Binding, grant p.TURNGrant) (Attempt, error) {
		attempt := &testTURNAttempt{fakeAttempt: newAttempt(), binding: binding, grant: grant, lane: session.LaneIdentity{ID: sequence.Add(1), Epoch: 1}}
		f.attempts <- attempt
		return attempt, nil
	}
	t.Cleanup(func() { cancel(); manager.close(); receiver.work.Wait() })
	return f
}

func turnGrant(id string) p.TURNGrant {
	grant := p.TURNGrant{ServiceID: id, URLs: []string{"turn:turn.example:3478"}, Username: "receiver", Password: "test"}
	copy(grant.GrantID[:], id)
	return grant
}

func (f *turnFixture) start(refresh bool) *testTURNAttempt {
	f.manager.acceptService(f.origin, turnGrant("home"), refresh)
	synctest.Wait()
	return receive(f.t, f.attempts)
}
func (f *turnFixture) admit(a *testTURNAttempt) {
	f.runtime.attach(a.lane)
	close(a.ready)
	synctest.Wait()
}
func (f *turnFixture) step(elapsed time.Duration, demand lanes.SchedulingDemand) {
	f.manager.step(f.origin.Add(elapsed), true, demand)
	synctest.Wait()
}
func (f *turnFixture) request(mode p.TURNMode, service string) p.TURNDemand {
	f.t.Helper()
	d := receive(f.t, f.runtime.requests)
	if d.Mode != mode || (service == "" && len(d.ServiceIDs) != 0) || (service != "" && (len(d.ServiceIDs) != 1 || d.ServiceIDs[0] != service)) {
		f.t.Fatalf("unexpected request %+v, wanted mode %v service %q", d, mode, service)
	}
	return d
}
func (f *turnFixture) reply(d p.TURNDemand, status p.TURNStatus, services ...string) {
	response := p.TURNGrants{RequestID: d.RequestID, HasFallback: true, Status: status}
	for _, id := range services {
		response.Grants = append(response.Grants, turnGrant(id))
	}
	body, err := p.EncodeTURNGrants(response)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.runtime.handler(f.t.Context(), body); err != nil {
		f.t.Fatal(err)
	}
}

func TestTURNRenewalProtectsTheLastLaneUntilItsSuccessorAttaches(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newTURNFixture(t)
		relay := session.LaneIdentity{ID: 100}
		f.runtime.attach(relay)
		var demand lanes.SchedulingDemand
		first := f.start(false)
		f.admit(first)
		f.step(0, demand)
		second := f.start(true)
		if second.binding.PeerPathID != first.binding.PeerPathID || second.binding.PeerSlot != first.binding.PeerSlot || second.binding.AttemptSequence != 2 {
			t.Fatal("ordinary renewal lost the service path or sequence watermark")
		}
		if _, usable := f.runtime.UsableLaneDone(first.lane); usable {
			t.Fatal("ordinary renewal failed to retire its previous lane")
		}
		f.admit(second)
		f.step(0, demand)
		f.runtime.markClosing(relay)

		failed := f.start(true)
		failed.Close()
		synctest.Wait()
		f.step(0, demand)
		closing := f.start(true)
		f.admit(closing)
		f.runtime.markClosing(closing.lane)
		f.step(0, demand)
		if _, usable := f.runtime.UsableLaneDone(second.lane); !usable {
			t.Fatal("failed or closing replacement retired the last usable lane")
		}
		spare := f.start(true)
		if spare.binding.PeerSlot == second.binding.PeerSlot || spare.binding.PeerPathID == second.binding.PeerPathID ||
			spare.binding.PeerPathID != failed.binding.PeerPathID || spare.binding.AttemptSequence != 3 {
			t.Fatal("replacement lost its spare slot watermark")
		}
		f.admit(spare)
		f.step(0, demand)
		if _, usable := f.runtime.UsableLaneDone(second.lane); usable {
			t.Fatal("authenticated successor did not retire its predecessor")
		}
		if current := f.manager.services["home"].current; current.lane != spare.lane {
			t.Fatalf("unexpected promoted lane %+v", current.lane)
		}
	})
}

func TestTURNRefreshAfterLaneLossReusesItsOwnSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newTURNFixture(t)
		f.runtime.attach(session.LaneIdentity{ID: 100})
		old := f.start(false)
		f.admit(old)
		f.runtime.DetachLane(old.lane)
		synctest.Wait()
		// The refresh response arrives before the next maintenance tick.
		next := f.start(true)
		if next.binding.PeerPathID != old.binding.PeerPathID || next.binding.PeerSlot != old.binding.PeerSlot || next.binding.AttemptSequence != 2 {
			t.Fatal("a dead predecessor took a spare slot while the relay was the sole survivor")
		}
	})
}

func TestCanceledTURNOwnerDoesNotPublishRacingAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newTURNFixture(t)
		stream, err := diag.NewStream(8, nil)
		if err != nil {
			t.Fatal(err)
		}
		f.manager.trace.Sink = stream.Sink()
		a := f.start(false)
		a.beforeLane = f.cancel
		f.admit(a)
		if leg := f.manager.services["home"].current; signalled(leg.ready) || !signalled(leg.done) {
			t.Fatal("canceled attachment published its ready signal")
		}
		if _, usable := f.runtime.UsableLaneDone(a.lane); usable {
			t.Fatal("canceled attachment leaked its lane")
		}
		stream.Close()
		for record := range stream.Records() {
			if record.Name() == "turn_path_admitted" {
				t.Fatal("canceled attachment published turn_path_admitted")
			}
		}
	})
}
