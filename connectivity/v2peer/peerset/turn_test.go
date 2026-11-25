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
	manager := newTURNManager(receiver, runtime, nil)
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
func (f *turnFixture) step(elapsed time.Duration, demand lanes.SchedulingDemand, failed ...lanes.Identity) {
	f.manager.step(f.origin.Add(elapsed), true, demand, failed)
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
	response := p.TURNGrants{RequestID: d.RequestID, HasFallback: status != p.TURNUnconfigured, Status: status}
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

func TestStalledCoveredTURNChannelRetiresAndRecoversWithinBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newTURNFixture(t)
		stream, err := diag.NewStream(16, nil)
		if err != nil {
			t.Fatal(err)
		}
		f.manager.trace.Sink = stream.Sink()
		delivered := make(map[session.LaneIdentity]bool)
		f.manager.delivered = func(id session.LaneIdentity) bool { return delivered[id] }

		f.runtime.attach(session.LaneIdentity{ID: 100})
		first := f.start(false)
		f.admit(first)
		routeBlocked := lanes.SchedulingDemand{QueuedBytes: 4096}
		f.step(0, routeBlocked)

		// A stale generation must not disturb the live TURN leg.
		f.step(0, routeBlocked, lanes.Identity{ID: first.lane.ID, Epoch: first.lane.Epoch + 1})
		if _, usable := f.runtime.UsableLaneDone(first.lane); !usable {
			t.Fatal("stale recovery generation retired a live TURN lane")
		}

		// Reporting the exact stalled generation retires the covered lane immediately
		// and schedules replacement after turnBackoff[0] without waiting for credential TTL.
		f.step(0, routeBlocked, lanes.Identity(first.lane))
		if _, usable := f.runtime.UsableLaneDone(first.lane); usable {
			t.Fatal("covered stalled TURN lane remained attached")
		}
		f.step(time.Second, routeBlocked)
		if len(f.runtime.requests) != 0 {
			t.Fatal("stalled TURN replacement bypassed initial backoff")
		}
		f.step(2*time.Second, routeBlocked)
		req := f.request(p.TURNAll, "home")
		f.reply(req, p.TURNOK, "home")
		f.step(2*time.Second, routeBlocked)
		second := receive(t, f.attempts)
		if second.binding.PeerPathID != first.binding.PeerPathID || second.binding.PeerSlot != first.binding.PeerSlot ||
			second.binding.AttemptSequence != first.binding.AttemptSequence+1 {
			t.Fatalf("covered recovery lost service slot watermark: first=%+v second=%+v", first.binding, second.binding)
		}
		f.admit(second)
		f.step(2*time.Second, routeBlocked)
		if f.manager.services["home"].retry.failures != 1 {
			t.Fatalf("ready without delivery reset stall retry budget: %+v", f.manager.services["home"].retry)
		}

		// Proving authenticated block delivery clears stall recovery and restores retry budget.
		delivered[second.lane] = true
		f.step(3*time.Second, routeBlocked)
		if f.manager.services["home"].retry.failures != 0 || f.manager.services["home"].awaitingDelivery {
			t.Fatalf("delivered replacement retained stall retry debt: %+v", f.manager.services["home"])
		}

		stream.Close()
		var sawClosedStall, sawRefreshStall bool
		for record := range stream.Records() {
			if record.Name() == "turn_path_closed" && record.Payload().Get("reason").Text() == turnReasonContentStalled {
				sawClosedStall = true
			}
			if record.Name() == "turn_refresh_requested" && record.Payload().Get("reason").Text() == turnReasonContentStalled {
				sawRefreshStall = true
			}
		}
		if !sawClosedStall || !sawRefreshStall {
			t.Fatalf("missing stall trace markers: closed=%t refresh=%t", sawClosedStall, sawRefreshStall)
		}
	})
}

func TestStalledLastTURNChannelProtectsUntilReplacementAndExhaustsOnPersistentBlackhole(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newTURNFixture(t)
		f.manager.delivered = func(session.LaneIdentity) bool { return false }

		current := f.start(false)
		f.admit(current)
		// Route-blocked demand has QueuedBytes > 0 while UnfinishedBytes and NetworkWaitingBytes are 0.
		routeBlocked := lanes.SchedulingDemand{QueuedBytes: 4096}
		f.step(0, routeBlocked)

		now := time.Duration(0)
		for i, backoff := range turnBackoff {
			prev := current
			f.step(now, routeBlocked, lanes.Identity(prev.lane))
			if _, usable := f.runtime.UsableLaneDone(prev.lane); !usable {
				t.Fatalf("stall %d retired the last usable lane before negotiating its successor", i+1)
			}
			// Repeated ticks during backoff must not double-charge the same stalled generation.
			f.step(now+backoff-time.Millisecond, routeBlocked, lanes.Identity(prev.lane))
			if len(f.runtime.requests) != 0 {
				t.Fatalf("stall %d bypassed backoff %v", i+1, backoff)
			}
			now += backoff
			f.step(now, routeBlocked, lanes.Identity(prev.lane))
			req := f.request(p.TURNAll, "home")
			f.reply(req, p.TURNOK, "home")
			f.step(now, routeBlocked, lanes.Identity(prev.lane))
			next := receive(t, f.attempts)
			if _, usable := f.runtime.UsableLaneDone(prev.lane); !usable {
				t.Fatalf("stall %d dropped last lane before successor became usable", i+1)
			}
			f.admit(next)
			f.step(now, routeBlocked, lanes.Identity(prev.lane))
			if _, usable := f.runtime.UsableLaneDone(prev.lane); usable {
				t.Fatalf("stall %d successor did not retire stalled predecessor", i+1)
			}
			if _, usable := f.runtime.UsableLaneDone(next.lane); !usable {
				t.Fatalf("stall %d successor was not retained", i+1)
			}
			current = next
		}

		// The 4th stall exhausts the retry budget and detaches the last zombie lane so
		// sessioncore can trigger TerminationPathsExhausted instead of hanging forever.
		f.step(now, routeBlocked, lanes.Identity(current.lane))
		if _, usable := f.runtime.UsableLaneDone(current.lane); usable {
			t.Fatal("exhausted stalled TURN lane remained attached as a zombie")
		}
		if _, exists := f.manager.services["home"]; exists {
			t.Fatal("exhausted stalled TURN service remained active")
		}
		if _, spent := f.manager.exhausted["home"]; !spent {
			t.Fatal("exhausted stalled TURN service was not remembered")
		}
		if len(f.runtime.requests) != 0 {
			t.Fatal("exhausted stalled TURN service sent another request")
		}
	})
}

func TestStalledLastTURNChannelDetachesWhenRecoveryBlocked(t *testing.T) {
	t.Run("grant timeouts exhaust retry budget", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newTURNFixture(t)
			leg := f.start(false)
			f.admit(leg)
			routeBlocked := lanes.SchedulingDemand{QueuedBytes: 4096}
			f.step(0, routeBlocked)

			now := time.Duration(0)
			f.step(now, routeBlocked, lanes.Identity(leg.lane))
			for _, backoff := range turnBackoff {
				now += backoff
				f.step(now, routeBlocked, lanes.Identity(leg.lane))
				f.request(p.TURNAll, "home")
				// Simulate a completely blackholed last lane where even control responses never arrive.
				now += grantResponseTimeout
				f.step(now, routeBlocked, lanes.Identity(leg.lane))
			}
			if _, usable := f.runtime.UsableLaneDone(leg.lane); usable {
				t.Fatal("blackholed stalled TURN lane was not detached after retry exhaustion")
			}
			if _, spent := f.manager.exhausted["home"]; !spent {
				t.Fatal("timed-out stalled TURN service was not marked exhausted")
			}
		})
	})

	t.Run("disabled TURN detaches stalled lane but preserves healthy lane", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newTURNFixture(t)
			f.manager.acceptService(f.origin, turnGrant("stalled"), false)
			f.manager.acceptService(f.origin, turnGrant("healthy"), false)
			synctest.Wait()
			a, b := receive(t, f.attempts), receive(t, f.attempts)
			stalledLeg, healthyLeg := a, b
			if a.grant.ServiceID != "stalled" {
				stalledLeg, healthyLeg = b, a
			}
			f.admit(stalledLeg)
			f.admit(healthyLeg)
			routeBlocked := lanes.SchedulingDemand{QueuedBytes: 4096}
			f.step(0, routeBlocked)
			f.step(0, routeBlocked, lanes.Identity(stalledLeg.lane))
			if _, usable := f.runtime.UsableLaneDone(stalledLeg.lane); usable {
				t.Fatal("covered stalled lane did not retire")
			}
			f.step(2*time.Second, routeBlocked)
			req := f.request(p.TURNAll, "stalled")
			f.reply(req, p.TURNUnconfigured)
			f.step(2*time.Second, routeBlocked)
			if _, exists := f.manager.services["stalled"]; exists {
				t.Fatal("disabled TURN kept dead stalled service occupied")
			}
			if _, usable := f.runtime.UsableLaneDone(healthyLeg.lane); !usable {
				t.Fatal("disabled TURN closed an unrelated healthy lane")
			}
		})
	})

	t.Run("disabled TURN detaches uncovered last stalled lane", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newTURNFixture(t)
			leg := f.start(false)
			f.admit(leg)
			routeBlocked := lanes.SchedulingDemand{QueuedBytes: 4096}
			f.step(0, routeBlocked)
			f.step(0, routeBlocked, lanes.Identity(leg.lane))
			f.step(2*time.Second, routeBlocked, lanes.Identity(leg.lane))
			req := f.request(p.TURNAll, "home")
			f.reply(req, p.TURNUnconfigured)
			f.step(2*time.Second, routeBlocked, lanes.Identity(leg.lane))
			if _, usable := f.runtime.UsableLaneDone(leg.lane); usable {
				t.Fatal("disabled TURN left uncovered stalled last lane attached")
			}
			if _, exists := f.manager.services["home"]; exists {
				t.Fatal("disabled TURN left uncovered stalled service active")
			}
		})
	})
}
