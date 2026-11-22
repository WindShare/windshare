package revisionwait

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session/protocolsession"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func instantAfter(clock *fakeClock) (func(time.Duration) (<-chan time.Time, func() bool), *[]time.Duration) {
	var mu sync.Mutex
	var waits []time.Duration
	after := func(delay time.Duration) (<-chan time.Time, func() bool) {
		mu.Lock()
		waits = append(waits, delay)
		mu.Unlock()
		clock.advance(delay)
		done := make(chan time.Time, 1)
		done <- clock.Now()
		return done, func() bool { return true }
	}
	return after, &waits
}

type manualTimer struct {
	done    chan time.Time
	mu      sync.Mutex
	stopped bool
}

func (t *manualTimer) stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	wasActive := !t.stopped
	t.stopped = true
	return wasActive
}

func (t *manualTimer) wasStopped() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stopped
}

func manualAfter() (func(time.Duration) (<-chan time.Time, func() bool), chan *manualTimer) {
	created := make(chan *manualTimer, 2)
	after := func(time.Duration) (<-chan time.Time, func() bool) {
		timer := &manualTimer{done: make(chan time.Time)}
		created <- timer
		return timer.done, timer.stop
	}
	return after, created
}

func waitTestID[T ~[protocolsession.IdentityBytes]byte](value byte) T {
	var id T
	id[0] = value
	return id
}

func waitTestSignal(t *testing.T, session protocolsession.ProtocolSessionID, hint time.Duration) *CapacitySignal {
	t.Helper()
	signal, err := NewCapacitySignal(hint, session, waitTestID[protocolsession.OperationID](3))
	if err != nil {
		t.Fatal(err)
	}
	return signal
}

func newTestWaiter(
	t *testing.T,
	clock *fakeClock,
	after func(time.Duration) (<-chan time.Time, func() bool),
	watch SessionWatchFunc,
	budget time.Duration,
	jitter time.Duration,
) *Waiter {
	t.Helper()
	waiter, err := NewWaiter(Config{
		WaitBudget:          budget,
		AdditiveJitterLimit: DefaultAdditiveJitterLimit,
		VisibilityThreshold: time.Second,
		WatchSession:        watch,
		Now:                 clock.Now,
		After:               after,
		Jitter:              func(time.Duration) (time.Duration, error) { return jitter, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return waiter
}

func TestWaiterUsesHintPlusBoundedJitterAndProjectsVisibilityThreshold(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	after, waits := instantAfter(clock)
	session := waitTestID[protocolsession.ProtocolSessionID](1)
	waiter := newTestWaiter(t, clock, after, WatchLifetime(session, context.Background()), 10*time.Second, 75*time.Millisecond)

	var visible []bool
	var traces []Trace
	observe := func(s Snapshot) { visible = append(visible, s.Visible(clock.Now())) }
	trace := func(e Trace) { traces = append(traces, e) }

	outcome, err := waiter.Wait(context.Background(), waitTestSignal(t, session, 2*time.Second), observe, trace)
	if err != nil || outcome != WaitRetry {
		t.Fatalf("outcome=%v err=%v", outcome, err)
	}
	if !slices.Equal(*waits, []time.Duration{2*time.Second + 75*time.Millisecond}) {
		t.Fatalf("waits=%v", *waits)
	}
	snap := waiter.Snapshot()
	if snap.ActiveWaiters != 1 || snap.Attempts != 1 || snap.AccumulatedWait != (*waits)[0] ||
		snap.ActiveDuration(clock.Now()) != (*waits)[0] || !snap.Visible(clock.Now()) {
		t.Fatalf("snapshot=%+v", snap)
	}
	if len(visible) < 2 || visible[0] || !visible[len(visible)-1] {
		t.Fatalf("threshold visibility=%v", visible)
	}
	waiter.Succeed(observe, trace)
	if final := waiter.Snapshot(); final.ActiveWaiters != 0 || final.Visible(clock.Now()) || final.ActiveDuration(clock.Now()) != 0 {
		t.Fatalf("final snapshot=%+v", final)
	}
	wantStages := []TraceStage{TraceRetryScheduled, TraceRetryReady, TraceRetrySucceeded}
	if len(traces) != len(wantStages) {
		t.Fatalf("traces=%+v", traces)
	}
	for i, stage := range wantStages {
		if traces[i].Stage != stage || traces[i].ProtocolSession != session {
			t.Fatalf("trace[%d]=%+v", i, traces[i])
		}
	}
}

func TestWaiterCancellationAndLifetimeEndStopTimerImmediately(t *testing.T) {
	t.Run("context cancel", func(t *testing.T) {
		clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
		after, created := manualAfter()
		session := waitTestID[protocolsession.ProtocolSessionID](4)
		waiter := newTestWaiter(t, clock, after, WatchLifetime(session, context.Background()), 10*time.Second, 0)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct {
			outcome WaitOutcome
			err     error
		}, 1)
		go func() {
			outcome, err := waiter.Wait(ctx, waitTestSignal(t, session, time.Second), nil, nil)
			done <- struct {
				outcome WaitOutcome
				err     error
			}{outcome, err}
		}()
		timer := <-created
		cancel()
		res := <-done
		if res.outcome != WaitCanceled || !errors.Is(res.err, context.Canceled) {
			t.Fatalf("result=%+v", res)
		}
		if !timer.wasStopped() || waiter.Snapshot().ActiveWaiters != 0 {
			t.Fatalf("timer stopped=%v snapshot=%+v", timer.wasStopped(), waiter.Snapshot())
		}
	})

	t.Run("session lifetime end", func(t *testing.T) {
		clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
		after, created := manualAfter()
		session := waitTestID[protocolsession.ProtocolSessionID](5)
		lifecycle, endRuntime := context.WithCancelCause(context.Background())
		waiter := newTestWaiter(t, clock, after, WatchLifetime(session, lifecycle), 10*time.Second, 0)
		done := make(chan struct {
			outcome WaitOutcome
			err     error
		}, 1)
		go func() {
			outcome, err := waiter.Wait(context.Background(), waitTestSignal(t, session, time.Second), nil, nil)
			done <- struct {
				outcome WaitOutcome
				err     error
			}{outcome, err}
		}()
		timer := <-created
		endRuntime(protocolsession.ErrSessionTerminated)
		res := <-done
		if res.outcome != WaitSessionEnded || !errors.Is(res.err, protocolsession.ErrSessionTerminated) {
			t.Fatalf("result=%+v", res)
		}
		if !timer.wasStopped() || waiter.Snapshot().ActiveWaiters != 0 {
			t.Fatalf("timer stopped=%v snapshot=%+v", timer.wasStopped(), waiter.Snapshot())
		}
	})
}

func TestWaiterReplacementReopensWithoutResettingBudgetAndSharesBudgetAcrossFiles(t *testing.T) {
	first := waitTestID[protocolsession.ProtocolSessionID](11)
	second := waitTestID[protocolsession.ProtocolSessionID](12)
	type change struct {
		next protocolsession.ProtocolSessionID
		err  error
	}
	changes := make(chan change, 2)
	watch := func(ctx context.Context, _ protocolsession.ProtocolSessionID) (protocolsession.ProtocolSessionID, error) {
		select {
		case <-ctx.Done():
			return protocolsession.ProtocolSessionID{}, ctx.Err()
		case c := <-changes:
			return c.next, c.err
		}
	}
	clock := &fakeClock{now: time.Unix(1, 0)}
	after, created := manualAfter()
	waiter := newTestWaiter(t, clock, after, watch, 10*time.Second, 0)

	done := make(chan WaitOutcome, 1)
	go func() {
		outcome, err := waiter.Wait(context.Background(), waitTestSignal(t, first, time.Second), nil, nil)
		if err != nil {
			t.Error(err)
		}
		done <- outcome
	}()
	<-created
	clock.advance(3 * time.Second)
	changes <- change{next: second}
	if outcome := <-done; outcome != WaitRetry {
		t.Fatalf("replacement outcome=%v", outcome)
	}
	if got := waiter.Snapshot().AccumulatedWait; got != 3*time.Second {
		t.Fatalf("spent=%v", got)
	}
	go func() {
		outcome, err := waiter.Wait(context.Background(), waitTestSignal(t, second, time.Second), nil, nil)
		if err == nil {
			t.Error("ended session was allowed to retry")
		}
		done <- outcome
	}()
	<-created
	clock.advance(2 * time.Second)
	changes <- change{err: ErrSessionEnded}
	if outcome := <-done; outcome != WaitSessionEnded {
		t.Fatalf("ended outcome=%v", outcome)
	}
	if got := waiter.Snapshot().AccumulatedWait; got != 5*time.Second {
		t.Fatalf("spent=%v", got)
	}

	// Next file shares remaining budget (5s left out of 10s).
	instantClock := &fakeClock{now: time.Unix(100, 0)}
	instAfter, waits := instantAfter(instantClock)
	shared := newTestWaiter(t, instantClock, instAfter, WatchLifetime(first, context.Background()), 5*time.Second, 0)
	if outcome, err := shared.Wait(context.Background(), waitTestSignal(t, first, 3*time.Second), nil, nil); err != nil || outcome != WaitRetry {
		t.Fatalf("first file outcome=%v err=%v", outcome, err)
	}
	shared.Succeed(nil, nil)
	outcome, err := shared.Wait(context.Background(), waitTestSignal(t, first, 3*time.Second), nil, nil)
	if outcome != WaitBudgetPaused || !errors.Is(err, ErrWaitBudgetExhausted) {
		t.Fatalf("second file outcome=%v err=%v", outcome, err)
	}
	if !slices.Equal(*waits, []time.Duration{3 * time.Second, 2 * time.Second}) {
		t.Fatalf("waits=%v", *waits)
	}
	if snap := shared.Snapshot(); snap.AccumulatedWait != 5*time.Second || snap.ActiveWaiters != 0 {
		t.Fatalf("snapshot=%+v", snap)
	}
}

func TestCapacitySignalAndConfigValidationContracts(t *testing.T) {
	session := waitTestID[protocolsession.ProtocolSessionID](7)
	signal := waitTestSignal(t, session, time.Second)
	if matched, ok := MatchCapacitySignal(signal); !ok || matched != signal {
		t.Fatal("direct signal was not matched")
	}
	if _, ok := MatchCapacitySignal(errors.Join(errors.New("wrapped"), signal)); ok {
		t.Fatal("wrapped signal acquired retry authority")
	}
	if _, err := NewCapacitySignal(time.Millisecond+time.Nanosecond, session, waitTestID[protocolsession.OperationID](3)); !errors.Is(err, ErrInvalidCapacitySignal) {
		t.Fatalf("fractional hint err=%v", err)
	}
	if (*CapacitySignal)(nil).Error() != ErrInvalidCapacitySignal.Error() || signal.Error() == "" {
		t.Fatal("signal error formatting invalid")
	}
	if _, err := NewWaiter(Config{WaitBudget: -1, WatchSession: WatchLifetime(session, context.Background())}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid budget err=%v", err)
	}
	if _, err := NewWaiter(Config{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("missing watcher err=%v", err)
	}
	if (*Waiter)(nil).Snapshot() != (Snapshot{}) {
		t.Fatal("nil waiter exposed non-zero snapshot")
	}
	var nilWaiter *Waiter
	nilWaiter.Succeed(nil, nil)
	nilWaiter.Cancel(nil, nil, nil)
	nilWaiter.Stop(nil)

	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	after, _ := instantAfter(clock)
	waiter := newTestWaiter(t, clock, after, WatchLifetime(session, context.Background()), 10*time.Second, 0)
	var traces []Trace
	if outcome, err := waiter.Wait(context.Background(), signal, nil, func(e Trace) { traces = append(traces, e) }); err != nil || outcome != WaitRetry {
		t.Fatalf("outcome=%v err=%v", outcome, err)
	}
	cause := errors.New("caller canceled")
	waiter.Cancel(cause, nil, func(e Trace) { traces = append(traces, e) })
	waiter.Cancel(nil, nil, func(e Trace) { traces = append(traces, e) })
	waiter.Stop(nil)
	if waiter.Snapshot().ActiveWaiters != 0 || len(traces) != 3 || traces[2].Stage != TraceWaitCanceled || !errors.Is(traces[2].Cause, cause) {
		t.Fatalf("cancel traces=%+v snap=%+v", traces, waiter.Snapshot())
	}
	if jitter, err := defaultJitter(DefaultAdditiveJitterLimit); err != nil || jitter < 0 || jitter > DefaultAdditiveJitterLimit {
		t.Fatalf("defaultJitter=%v err=%v", jitter, err)
	}
	if jitter, err := defaultJitter(0); err != nil || jitter != 0 {
		t.Fatalf("zero defaultJitter=%v err=%v", jitter, err)
	}
}
