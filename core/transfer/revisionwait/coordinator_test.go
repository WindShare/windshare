package revisionwait

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session/protocolsession"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *fakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeClock) advance(elapsed time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(elapsed)
	clock.mu.Unlock()
}

type instantTimerFactory struct {
	clock *fakeClock
	mu    sync.Mutex
	waits []time.Duration
}

func (factory *instantTimerFactory) NewTimer(delay time.Duration) Timer {
	factory.mu.Lock()
	factory.waits = append(factory.waits, delay)
	factory.mu.Unlock()
	factory.clock.advance(delay)
	done := make(chan time.Time, 1)
	done <- factory.clock.Now()
	return &fakeTimer{done: done}
}

type manualTimerFactory struct {
	created chan *fakeTimer
}

func (factory *manualTimerFactory) NewTimer(time.Duration) Timer {
	timer := &fakeTimer{done: make(chan time.Time)}
	factory.created <- timer
	return timer
}

type fakeTimer struct {
	done    chan time.Time
	mu      sync.Mutex
	stopped bool
}

func (timer *fakeTimer) Done() <-chan time.Time { return timer.done }
func (timer *fakeTimer) Stop() bool {
	timer.mu.Lock()
	defer timer.mu.Unlock()
	wasActive := !timer.stopped
	timer.stopped = true
	return wasActive
}

func (timer *fakeTimer) wasStopped() bool {
	timer.mu.Lock()
	defer timer.mu.Unlock()
	return timer.stopped
}

type fixedJitter time.Duration

func (jitter fixedJitter) AdditiveJitter(time.Duration) (time.Duration, error) {
	return time.Duration(jitter), nil
}

type sequenceWaitIDs struct{ next byte }

func (ids *sequenceWaitIDs) NewWaitID() (WaitID, error) {
	ids.next++
	var id WaitID
	id[0] = ids.next
	return id, nil
}

func waitTestIdentity[T ~[protocolsession.IdentityBytes]byte](value byte) T {
	var identity T
	identity[0] = value
	return identity
}

func waitTestToken(t *testing.T, value byte) GenerationToken {
	t.Helper()
	raw := make([]byte, GenerationBytes)
	raw[0] = value
	token, err := GenerationTokenFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func waitTestSignal(t *testing.T, token GenerationToken, hint time.Duration) *CapacitySignal {
	t.Helper()
	signal, err := NewCapacitySignal(CapacitySignalSpec{
		RetryAfter: hint, ProtocolSession: waitTestIdentity[protocolsession.ProtocolSessionID](2),
		ProtocolOperation: waitTestIdentity[protocolsession.OperationID](3), Generation: token,
	})
	if err != nil {
		t.Fatal(err)
	}
	return signal
}

func waitTestCoordinator(
	t *testing.T,
	clock *fakeClock,
	timers TimerFactory,
	fence GenerationFence,
	budget time.Duration,
	jitter time.Duration,
) *Coordinator {
	t.Helper()
	coordinator, err := NewCoordinator(Config{
		WaitBudget: budget, AdditiveJitterLimit: DefaultAdditiveJitterLimit,
		VisibilityThreshold: time.Second, Clock: clock, Timers: timers,
		Jitter: fixedJitter(jitter), WaitIDs: &sequenceWaitIDs{}, GenerationFence: fence,
	})
	if err != nil {
		t.Fatal(err)
	}
	return coordinator
}

func TestCoordinatorUsesHintPlusBoundedJitterAndProjectsThresholdedProgress(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	timers := &instantTimerFactory{clock: clock}
	token := waitTestToken(t, 1)
	fence, _ := NewLifetimeFence(token, context.Background())
	coordinator := waitTestCoordinator(t, clock, timers, fence, 10*time.Second, 75*time.Millisecond)
	var visible []bool
	var traces []Trace
	operation, err := coordinator.NewOperation(
		ObserverFunc(func(snapshot Snapshot) { visible = append(visible, snapshot.Visible(clock.Now())) }),
		TraceFunc(func(event Trace) { traces = append(traces, event) }),
	)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := operation.Wait(context.Background(), waitTestSignal(t, token, 2*time.Second))
	if err != nil || outcome != WaitRetry {
		t.Fatalf("outcome=%v err=%v", outcome, err)
	}
	if len(timers.waits) != 1 || timers.waits[0] != 2*time.Second+75*time.Millisecond {
		t.Fatalf("waits=%v", timers.waits)
	}
	snapshot := coordinator.Snapshot()
	if snapshot.ActiveWaiters != 1 || snapshot.Attempts != 1 ||
		snapshot.AccumulatedWait != timers.waits[0] || !snapshot.Visible(clock.Now()) {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	if len(visible) < 2 || visible[0] || !visible[len(visible)-1] {
		t.Fatalf("threshold visibility=%v", visible)
	}
	operation.Succeed()
	if final := coordinator.Snapshot(); final.ActiveWaiters != 0 || final.Visible(clock.Now()) {
		t.Fatalf("final snapshot=%+v", final)
	}
	wantStages := []TraceStage{TraceRetryScheduled, TraceRetryReady, TraceRetrySucceeded}
	if len(traces) != len(wantStages) {
		t.Fatalf("traces=%+v", traces)
	}
	for index, stage := range wantStages {
		if traces[index].Stage != stage || traces[index].WaitID != operation.ID() {
			t.Fatalf("trace[%d]=%+v", index, traces[index])
		}
	}
}

func TestCoordinatorCancellationStopsTimerAndClearsWaiter(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	timers := &manualTimerFactory{created: make(chan *fakeTimer, 1)}
	token := waitTestToken(t, 4)
	fence, _ := NewLifetimeFence(token, context.Background())
	coordinator := waitTestCoordinator(t, clock, timers, fence, 10*time.Second, 0)
	operation, _ := coordinator.NewOperation(nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		outcome WaitOutcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		outcome, err := operation.Wait(ctx, waitTestSignal(t, token, time.Second))
		done <- result{outcome: outcome, err: err}
	}()
	timer := <-timers.created
	cancel()
	resultValue := <-done
	if resultValue.outcome != WaitCanceled || !errors.Is(resultValue.err, context.Canceled) {
		t.Fatalf("result=%+v", resultValue)
	}
	if !timer.wasStopped() || coordinator.Snapshot().ActiveWaiters != 0 {
		t.Fatalf("timer stopped=%v snapshot=%+v", timer.wasStopped(), coordinator.Snapshot())
	}
}

func TestCoordinatorLifetimeEndPreventsStaleRetry(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	timers := &manualTimerFactory{created: make(chan *fakeTimer, 1)}
	token := waitTestToken(t, 5)
	lifecycle, endRuntime := context.WithCancelCause(context.Background())
	fence, _ := NewLifetimeFence(token, lifecycle)
	coordinator := waitTestCoordinator(t, clock, timers, fence, 10*time.Second, 0)
	operation, _ := coordinator.NewOperation(nil, nil)
	type result struct {
		outcome WaitOutcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		outcome, err := operation.Wait(context.Background(), waitTestSignal(t, token, time.Second))
		done <- result{outcome: outcome, err: err}
	}()
	timer := <-timers.created
	endRuntime(protocolsession.ErrSessionTerminated)
	resultValue := <-done
	if resultValue.outcome != WaitGenerationEnded || !errors.Is(resultValue.err, protocolsession.ErrSessionTerminated) {
		t.Fatalf("result=%+v", resultValue)
	}
	if !timer.wasStopped() || coordinator.Snapshot().ActiveWaiters != 0 {
		t.Fatalf("timer stopped=%v snapshot=%+v", timer.wasStopped(), coordinator.Snapshot())
	}
}

func TestCoordinatorSharesAccumulatedWallClockBudgetAcrossOperations(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	timers := &instantTimerFactory{clock: clock}
	token := waitTestToken(t, 6)
	fence, _ := NewLifetimeFence(token, context.Background())
	coordinator := waitTestCoordinator(t, clock, timers, fence, 5*time.Second, 0)
	first, _ := coordinator.NewOperation(nil, nil)
	if outcome, err := first.Wait(context.Background(), waitTestSignal(t, token, 3*time.Second)); err != nil || outcome != WaitRetry {
		t.Fatalf("first outcome=%v err=%v", outcome, err)
	}
	first.Succeed()
	second, _ := coordinator.NewOperation(nil, nil)
	outcome, err := second.Wait(context.Background(), waitTestSignal(t, token, 3*time.Second))
	if outcome != WaitBudgetPaused || !errors.Is(err, ErrWaitBudgetExhausted) {
		t.Fatalf("second outcome=%v err=%v", outcome, err)
	}
	if len(timers.waits) != 2 || timers.waits[0] != 3*time.Second || timers.waits[1] != 2*time.Second {
		t.Fatalf("waits=%v", timers.waits)
	}
	if snapshot := coordinator.Snapshot(); snapshot.AccumulatedWait != 5*time.Second || snapshot.ActiveWaiters != 0 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
}

func TestCapacitySignalRequiresDirectAuthenticatedCarrier(t *testing.T) {
	token := waitTestToken(t, 7)
	signal := waitTestSignal(t, token, time.Second)
	if matched, ok := MatchCapacitySignal(signal); !ok || matched != signal {
		t.Fatalf("direct signal was not admitted")
	}
	if _, ok := MatchCapacitySignal(errors.Join(errors.New("diagnostic"), signal)); ok {
		t.Fatal("wrapped capacity signal acquired retry authority")
	}
	if _, err := NewCapacitySignal(CapacitySignalSpec{
		RetryAfter:        time.Millisecond + time.Nanosecond,
		ProtocolSession:   waitTestIdentity[protocolsession.ProtocolSessionID](2),
		ProtocolOperation: waitTestIdentity[protocolsession.OperationID](3), Generation: token,
	}); !errors.Is(err, ErrInvalidCapacitySignal) {
		t.Fatalf("fractional wire hint err=%v", err)
	}
}

func TestGenerationFenceAndValueContracts(t *testing.T) {
	token := waitTestToken(t, 8)
	current := waitTestToken(t, 9)
	if len(token.Bytes()) != GenerationBytes || token.Bytes()[0] != 8 {
		t.Fatalf("token bytes=%v", token.Bytes())
	}
	replacement, err := NewGenerationReplacement(token, current, nil)
	if err != nil || replacement.Kind() != GenerationReplaced || replacement.Previous() != token ||
		replacement.Current() != current || !errors.Is(replacement.Cause(), ErrGenerationChanged) ||
		!replacement.validFor(token) {
		t.Fatalf("replacement=%+v err=%v", replacement, err)
	}
	if _, err := NewGenerationReplacement(token, token, nil); !errors.Is(err, ErrInvalidGenerationToken) {
		t.Fatalf("same generation err=%v", err)
	}
	if _, err := NewGenerationEnd(GenerationToken{}, nil); !errors.Is(err, ErrInvalidGenerationToken) {
		t.Fatalf("zero end err=%v", err)
	}
	lifecycle, end := context.WithCancelCause(context.Background())
	fence, err := NewLifetimeFence(current, lifecycle)
	if err != nil || fence.Current() != current {
		t.Fatalf("fence=%v current=%v err=%v", fence, fence.Current(), err)
	}
	change, err := fence.WaitForChange(context.Background(), token)
	if err != nil || change.Kind() != GenerationReplaced || change.Current() != current {
		t.Fatalf("stale change=%+v err=%v", change, err)
	}
	end(protocolsession.ErrSessionTerminated)
	if !fence.Current().IsZero() {
		t.Fatalf("ended fence current=%v", fence.Current())
	}
}

func TestPolicyValidationStopAndRandomSources(t *testing.T) {
	token := waitTestToken(t, 10)
	fence, _ := NewLifetimeFence(token, context.Background())
	if _, err := NewCoordinator(Config{WaitBudget: -1, GenerationFence: fence}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid config err=%v", err)
	}
	if _, err := NewLifetimeFence(GenerationToken{}, context.Background()); !errors.Is(err, ErrInvalidGenerationToken) {
		t.Fatalf("invalid fence err=%v", err)
	}
	if _, err := GenerationTokenFromBytes([]byte{1}); !errors.Is(err, ErrInvalidGenerationToken) {
		t.Fatalf("short token err=%v", err)
	}
	if _, err := NewCapacitySignal(CapacitySignalSpec{}); !errors.Is(err, ErrInvalidCapacitySignal) {
		t.Fatalf("invalid signal err=%v", err)
	}
	if (*CapacitySignal)(nil).Error() != ErrInvalidCapacitySignal.Error() || waitTestSignal(t, token, time.Second).Error() == "" {
		t.Fatal("capacity signal error projection is empty")
	}

	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	timers := &instantTimerFactory{clock: clock}
	coordinator := waitTestCoordinator(t, clock, timers, fence, 10*time.Second, 0)
	operation, _ := coordinator.NewOperation(nil, nil)
	if outcome, err := operation.Wait(context.Background(), waitTestSignal(t, token, time.Second)); err != nil || outcome != WaitRetry {
		t.Fatalf("outcome=%v err=%v", outcome, err)
	}
	operation.Stop()
	operation.Stop()
	if coordinator.Snapshot().ActiveWaiters != 0 || len(operation.ID().Bytes()) != WaitIdentityBytes {
		t.Fatalf("snapshot=%+v id=%v", coordinator.Snapshot(), operation.ID())
	}

	random := &lockedRandom{source: bytes.NewReader([]byte{
		1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		5, 0, 0, 0, 0, 0, 0, 0,
	})}
	if id, err := random.NewWaitID(); err != nil || id.IsZero() {
		t.Fatalf("random id=%v err=%v", id, err)
	}
	if jitter, err := random.AdditiveJitter(10); err != nil || jitter != 5 {
		t.Fatalf("jitter=%v err=%v", jitter, err)
	}
	if jitter, err := random.AdditiveJitter(0); err != nil || jitter != 0 {
		t.Fatalf("zero jitter=%v err=%v", jitter, err)
	}
}
