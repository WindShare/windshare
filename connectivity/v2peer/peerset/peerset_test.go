package peerset

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

type testTimer struct {
	ch       chan time.Time
	duration time.Duration
	mu       sync.Mutex
	stopped  bool
}

func (t *testTimer) C() <-chan time.Time { return t.ch }
func (t *testTimer) Stop()               { t.mu.Lock(); t.stopped = true; t.mu.Unlock() }
func (t *testTimer) fire(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.stopped {
		t.ch <- now
		t.stopped = true
	}
}

type testClock struct {
	mu     sync.Mutex
	now    time.Time
	timers chan *testTimer
}

func newClock() *testClock {
	return &testClock{now: time.Unix(100, 0), timers: make(chan *testTimer, 100)}
}
func (c *testClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *testClock) NewTimer(d time.Duration) Timer {
	timer := &testTimer{ch: make(chan time.Time, 1), duration: d}
	c.timers <- timer
	return timer
}
func (c *testClock) advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }
func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(time.Second):
		t.Fatal("event did not arrive")
		var zero T
		return zero
	}
}
func nextTimer(t *testing.T, c *testClock, d time.Duration) *testTimer {
	t.Helper()
	timer := receive(t, c.timers)
	if timer.duration != d {
		t.Fatalf("timer %s, want %s", timer.duration, d)
	}
	return timer
}

type fakeAttempt struct {
	ready, done chan struct{}
	once        sync.Once
}

func newAttempt() *fakeAttempt {
	return &fakeAttempt{ready: make(chan struct{}), done: make(chan struct{})}
}
func (a *fakeAttempt) Ready() <-chan struct{} { return a.ready }
func (a *fakeAttempt) Done() <-chan struct{}  { return a.done }
func (a *fakeAttempt) Lane() (sessionruntime.LaneIdentity, bool) {
	return sessionruntime.LaneIdentity{ID: 2, Epoch: 1}, true
}
func (a *fakeAttempt) Outcome() v2peer.ReceiverAttemptOutcome { return v2peer.ReceiverAttemptOutcome{} }
func (a *fakeAttempt) Err() error                             { return nil }
func (a *fakeAttempt) Close() error                           { a.once.Do(func() { close(a.done) }); return nil }

func TestBudgetReplenishesAcrossGenerationsWithoutReset(t *testing.T) {
	now := time.Unix(10, 0)
	budget := NewBudget(now)
	for range 4 {
		refund, delay := budget.reserve(now)
		if refund == nil || delay != 0 {
			t.Fatal("available budget denied")
		}
		refund(AttemptBudget)
	}
	if refund, delay := budget.reserve(now); refund != nil || delay <= 0 {
		t.Fatal("active-time debt not enforced")
	}
	// A new protocol/network owner shares the same receive budget.
	fresh, _ := New(Config{Budget: budget})
	if fresh.config.Budget != budget {
		t.Fatal("generation replaced the receive budget")
	}
	refund, delay := budget.reserve(now.Add(RefillWindow))
	if refund == nil || delay != 0 {
		t.Fatal("budget did not replenish")
	}
	refund(0)
	refund(0)
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if budget.active != float64(ActiveTimePerWindow) {
		t.Fatal("unused reservation not refunded once")
	}
}

func TestAttemptCountRemainsBoundedWhenSetupFailsImmediately(t *testing.T) {
	now := time.Unix(10, 0)
	budget := NewBudget(now)
	for range AttemptsPerWindow {
		refund, _ := budget.reserve(now)
		if refund == nil {
			t.Fatal("early depletion")
		}
		refund(0)
	}
	if refund, delay := budget.reserve(now); refund != nil || delay != RefillWindow/AttemptsPerWindow {
		t.Fatal("count bucket bypassed")
	}
	if refund, _ := budget.reserve(now.Add(-time.Hour)); refund != nil {
		t.Fatal("clock reversal replenished budget")
	}
}

func TestCapacityFIFOAndCanceledWaitersReleaseOnlyOwnSlot(t *testing.T) {
	capacity, _ := NewCapacity(1)
	first, err := capacity.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	secondCtx, cancel := context.WithCancel(context.Background())
	second := make(chan error, 1)
	go func() { _, err := capacity.acquire(secondCtx); second <- err }()
	waitQueue(t, capacity, 1)
	third := make(chan func(), 1)
	go func() { release, _ := capacity.acquire(context.Background()); third <- release }()
	waitQueue(t, capacity, 2)
	cancel()
	if !errors.Is(receive(t, second), context.Canceled) {
		t.Fatal("cancel not propagated")
	}
	first()
	release := receive(t, third)
	release()
	release()
	capacity.mu.Lock()
	defer capacity.mu.Unlock()
	if capacity.active != 0 || len(capacity.queue) != 0 {
		t.Fatal("slot leaked")
	}
}
func waitQueue(t *testing.T, c *Capacity, n int) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		c.mu.Lock()
		count := len(c.queue)
		c.mu.Unlock()
		if count == n {
			return
		}
		select {
		case <-deadline:
			t.Fatal("waiter not queued")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestPrewarmContentTakeoverRetainsSameAttemptAndReleasesOnDemandEnd(t *testing.T) {
	clock := newClock()
	capacity, _ := NewCapacity(1)
	owner, _ := New(Config{Clock: clock, Capacity: capacity})
	attempts := make(chan *fakeAttempt, 2)
	path, err := owner.Open(context.Background(), PathConfig{Demand: BrowseDemand, Start: func(context.Context, v2signal.Binding) (Attempt, error) {
		attempt := newAttempt()
		attempts <- attempt
		return attempt, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	attempt := receive(t, attempts)
	nextTimer(t, clock, AttemptBudget)
	close(attempt.ready)
	receive(t, path.Ready())
	hold := nextTimer(t, clock, PrewarmRetention)
	capacity.mu.Lock()
	active := capacity.active
	capacity.mu.Unlock()
	if active != 0 {
		t.Fatal("admitted lane retained attempt slot")
	}
	if err := path.SetDemand(ContentDemand); err != nil {
		t.Fatal(err)
	}
	// Even a raced retention expiration cannot end newly active content demand.
	hold.fire(clock.Now())
	select {
	case <-path.Done():
		t.Fatal("takeover lost lane")
	default:
	}
	if _, ok := path.Lane(); !ok {
		t.Fatal("admitted lane unavailable")
	}
	if err := path.SetDemand(NoDemand); err != nil {
		t.Fatal(err)
	}
	receive(t, path.Done())
	if !path.Result().Stopped {
		t.Fatal("demand end not locally owned")
	}
	if err := path.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-attempt.done:
	default:
		t.Fatal("attempt owner not released")
	}
}

func TestPrewarmHasOneAttemptAndBoundedRetention(t *testing.T) {
	clock := newClock()
	owner, _ := New(Config{Clock: clock})
	attempt := newAttempt()
	path, err := owner.Open(context.Background(), PathConfig{Demand: BrowseDemand, Start: func(context.Context, v2signal.Binding) (Attempt, error) { return attempt, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer path.Close()
	nextTimer(t, clock, AttemptBudget)
	close(attempt.ready)
	receive(t, path.Ready())
	nextTimer(t, clock, PrewarmRetention).fire(clock.Now())
	receive(t, attempt.Done())
	if _, err = owner.Open(context.Background(), PathConfig{Demand: BrowseDemand, Start: func(context.Context, v2signal.Binding) (Attempt, error) {
		t.Fatal("second prewarm started")
		return nil, nil
	}}); err == nil {
		t.Fatal("session prewarmed twice")
	}
}

func TestRetryAdvancesIdentityAndWaveCannotTruncateICEOpportunity(t *testing.T) {
	clock := newClock()
	owner, _ := New(Config{Clock: clock})
	bindings := make(chan v2signal.Binding, 4)
	attempts := make(chan *fakeAttempt, 4)
	path, err := owner.Open(context.Background(), PathConfig{Demand: ContentDemand, StopAfterWave: true, Start: func(_ context.Context, binding v2signal.Binding) (Attempt, error) {
		bindings <- binding
		a := newAttempt()
		attempts <- a
		return a, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	first := receive(t, bindings)
	a := receive(t, attempts)
	nextTimer(t, clock, AttemptBudget)
	a.Close()
	backoff := nextTimer(t, clock, time.Second)
	clock.advance(time.Second)
	backoff.fire(clock.Now())
	second := receive(t, bindings)
	receive(t, attempts)
	if first.PeerPathID != second.PeerPathID || first.AttemptID == second.AttemptID || second.AttemptSequence != 2 {
		t.Fatal("fresh retry lost stable identity")
	}
	timer := nextTimer(t, clock, AttemptBudget)
	clock.advance(AttemptBudget)
	timer.fire(clock.Now())
	backoff = nextTimer(t, clock, 2*time.Second)
	clock.advance(2 * time.Second)
	backoff.fire(clock.Now())
	receive(t, path.Done())
	if !errors.Is(path.Err(), ErrWaveExhausted) {
		t.Fatal("wave admitted shortened attempt")
	}
}

func TestPathIdentityAndCapacityAreIndependent(t *testing.T) {
	clock := newClock()
	owner, _ := New(Config{Clock: clock})
	var session protocolsession.ProtocolSessionID
	started := make(chan struct{}, MaximumPaths)
	start := func(context.Context, v2signal.Binding) (Attempt, error) {
		started <- struct{}{}
		return newAttempt(), nil
	}
	paths := make([]*Path, 0, MaximumPaths)
	for i := range MaximumPaths {
		path, err := owner.Open(context.Background(), PathConfig{SessionID: session, PeerPathID: v2signal.PeerPathID{byte(i + 1)}, Demand: ContentDemand, Start: start})
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	// Each path gets an independent lifecycle, while the receive account
	// reserves enough elapsed-time budget before admitting concurrent attempts.
	for range int(ActiveTimePerWindow / AttemptBudget) {
		receive(t, started)
	}
	if _, err := owner.Open(context.Background(), PathConfig{Start: start}); err == nil {
		t.Fatal("unbounded paths")
	}
	for _, path := range paths {
		path.SetDemand(NoDemand)
		receive(t, path.Done())
	}
}
