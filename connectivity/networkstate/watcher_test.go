package networkstate

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type countingSource struct {
	mu      sync.Mutex
	calls   atomic.Int64
	state   State
	err     error
	entered chan struct{}
	block   chan struct{}
}

func (s *countingSource) Snapshot(ctx context.Context) (State, error) {
	s.calls.Add(1)
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	s.mu.Lock()
	entered, block := s.entered, s.block
	s.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return State{}, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, s.err
}

func (s *countingSource) setBlock(block chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.block = block
}

func (s *countingSource) setIP(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = State{Addresses: []Address{{IP: netip.MustParseAddr(ip), InterfaceIndex: 1}}}
}

func (s *countingSource) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func TestWatcherReusesFreshSnapshotAndFansOutTrueGenerationChanges(t *testing.T) {
	now := time.Unix(1000, 0)
	var clockMu sync.Mutex
	clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
	advance := func(d time.Duration) { clockMu.Lock(); now = now.Add(d); clockMu.Unlock() }

	source := &countingSource{block: make(chan struct{})}
	source.setIP("10.0.0.2")
	watcher := NewWatcher(WatcherConfig{
		Source:   source,
		Debounce: 500 * time.Millisecond,
		Interval: time.Hour,
		Now:      clock,
	})
	defer watcher.Close()

	// Concurrent cold-start Snapshot callers share one single-flight OS query.
	const callers = 8
	var wg sync.WaitGroup
	snapshots := make([]Snapshot, callers)
	for i := range callers {
		wg.Go(func() {
			snap, err := watcher.Snapshot(context.Background())
			if err != nil {
				t.Errorf("caller %d: %v", i, err)
			}
			snapshots[i] = snap
		})
	}
	for source.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	close(source.block)
	wg.Wait()
	if source.calls.Load() != 1 {
		t.Fatalf("cold-start calls = %d, want 1", source.calls.Load())
	}
	for i, snap := range snapshots {
		if snap.GenerationID() != 1 {
			t.Fatalf("caller %d generation = %d, want 1", i, snap.GenerationID())
		}
	}

	// Subscribing reuses the fresh baseline without firing a spurious 0 -> 1 change.
	subA, unsubA := watcher.Subscribe()
	defer unsubA()
	subB, unsubB := watcher.Subscribe()
	defer unsubB()

	for range 10 {
		snap, changed, err := watcher.Poll(context.Background(), clock())
		if err != nil || changed || snap.GenerationID() != 1 {
			t.Fatalf("cached Poll = (%d, %t, %v)", snap.GenerationID(), changed, err)
		}
	}
	if source.calls.Load() != 1 {
		t.Fatalf("cached polls triggered OS queries: calls=%d", source.calls.Load())
	}
	select {
	case snap := <-subA:
		t.Fatalf("baseline emitted spurious change: %+v", snap)
	default:
	}

	// A real address transition after debounce fans out to every subscriber.
	source.setIP("10.0.0.3")
	if _, changed, err := watcher.Step(context.Background()); err != nil || changed {
		t.Fatalf("pre-debounce step = (%t, %v)", changed, err)
	}
	advance(500 * time.Millisecond)
	next, changed, err := watcher.Step(context.Background())
	if err != nil || !changed || next.GenerationID() != 2 {
		t.Fatalf("post-debounce step = (%d, %t, %v)", next.GenerationID(), changed, err)
	}
	for name, ch := range map[string]<-chan Snapshot{"A": subA, "B": subB} {
		select {
		case got := <-ch:
			if got.GenerationID() != 2 {
				t.Fatalf("subscriber %s generation = %d, want 2", name, got.GenerationID())
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %s missed generation 2", name)
		}
	}
}

func TestWatcherIdlePauseAvoidsFalseResumeWhileActiveDetectsSleep(t *testing.T) {
	now := time.Unix(2000, 0)
	var clockMu sync.Mutex
	clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
	advance := func(d time.Duration) { clockMu.Lock(); now = now.Add(d); clockMu.Unlock() }

	source := &countingSource{}
	source.setIP("10.0.0.2")
	watcher := NewWatcher(WatcherConfig{
		Source:   source,
		Debounce: time.Nanosecond,
		Interval: time.Hour,
		Now:      clock,
	})
	defer watcher.Close()

	_, unsub := watcher.Subscribe()
	if snap, err := watcher.Snapshot(context.Background()); err != nil || snap.GenerationID() != 1 {
		t.Fatalf("initial snapshot = (%d, %v)", snap.GenerationID(), err)
	}
	unsub()

	// Pausing while idle for longer than ResumeGap must not synthesize a sleep event.
	advance(ResumeGap + time.Minute)
	_, unsub2 := watcher.Subscribe()
	defer unsub2()
	if snap, err := watcher.Snapshot(context.Background()); err != nil || snap.GenerationID() != 1 {
		t.Fatalf("post-idle snapshot = (%d, %v)", snap.GenerationID(), err)
	}
	if snap, changed, err := watcher.Step(context.Background()); err != nil || changed || snap.GenerationID() != 1 {
		t.Fatalf("idle pause triggered false resume: (%d, %t, %v)", snap.GenerationID(), changed, err)
	}

	// While actively watching, a gap beyond ResumeGap still advances generation.
	advance(ResumeGap + time.Second)
	if _, changed, err := watcher.Step(context.Background()); err != nil || changed {
		t.Fatalf("resume debounce step = (%t, %v)", changed, err)
	}
	advance(time.Second)
	if snap, changed, err := watcher.Step(context.Background()); err != nil || !changed || snap.GenerationID() != 2 {
		t.Fatalf("active sleep was not detected: (%d, %t, %v)", snap.GenerationID(), changed, err)
	}
}

func TestWatcherCloseClosesSubscribers(t *testing.T) {
	source := &countingSource{}
	source.setIP("10.0.0.2")
	watcher := NewWatcher(WatcherConfig{Source: source, Interval: time.Hour})
	ch, _ := watcher.Subscribe()
	watcher.Close()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected closed subscriber channel")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not close subscriber channel")
	}
}

func TestWatcherInitialPollSharesConcurrentColdStartSnapshot(t *testing.T) {
	now := time.Unix(3000, 0)
	clock := func() time.Time { return now }

	for range 16 {
		source := &countingSource{block: make(chan struct{})}
		source.setIP("10.0.0.2")
		watcher := NewWatcher(WatcherConfig{
			Source:   source,
			Debounce: time.Nanosecond,
			Interval: time.Hour,
			Now:      clock,
		})

		done := make(chan error, 1)
		go func() {
			snap, err := watcher.Snapshot(context.Background())
			if err == nil && snap.GenerationID() != 1 {
				t.Errorf("cold-start generation = %d, want 1", snap.GenerationID())
			}
			done <- err
		}()
		for source.calls.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		_, unsub := watcher.Subscribe()
		close(source.block)
		if err := <-done; err != nil {
			unsub()
			watcher.Close()
			t.Fatal(err)
		}
		unsub()
		watcher.Close()
		if calls := source.calls.Load(); calls != 1 {
			t.Fatalf("concurrent cold-start Snapshot + Subscribe calls = %d, want 1", calls)
		}
	}
}

func TestWatcherSurfacesExpiredSnapshotAndPersistentSamplingErrors(t *testing.T) {
	now := time.Unix(3000, 0)
	var clockMu sync.Mutex
	clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
	advance := func(d time.Duration) { clockMu.Lock(); now = now.Add(d); clockMu.Unlock() }

	source := &countingSource{}
	source.setIP("10.0.0.2")
	watcher := NewWatcher(WatcherConfig{
		Source:   source,
		Debounce: time.Nanosecond,
		Interval: time.Hour,
		Now:      clock,
	})
	defer watcher.Close()

	// Establish baseline before subscribing so run()'s initial check deterministically reuses it.
	if snap, err := watcher.Snapshot(context.Background()); err != nil || snap.GenerationID() != 1 {
		t.Fatalf("baseline snapshot = (%d, %v)", snap.GenerationID(), err)
	}
	_, unsub := watcher.Subscribe()
	defer unsub()
	baselineCalls := source.calls.Load()

	// Normal ticker jitter (within 2*Interval) still reuses the cached snapshot.
	advance(time.Hour + 10*time.Minute)
	if snap, changed, err := watcher.Poll(context.Background(), clock()); err != nil || changed || snap.GenerationID() != 1 {
		t.Fatalf("jitter-window Poll = (%d, %t, %v)", snap.GenerationID(), changed, err)
	}
	if source.calls.Load() != baselineCalls {
		t.Fatalf("jitter-window Poll queried OS: calls=%d want=%d", source.calls.Load(), baselineCalls)
	}

	// A single transient sampling error while the snapshot is still fresh is absorbed.
	sampleErr := errors.New("snapshot unavailable")
	source.setErr(sampleErr)
	if snap, changed, err := watcher.Step(context.Background()); !errors.Is(err, sampleErr) || changed || snap.GenerationID() != 1 {
		t.Fatalf("first failed Step = (%d, %t, %v)", snap.GenerationID(), changed, err)
	}
	if snap, changed, err := watcher.Poll(context.Background(), clock()); err != nil || changed || snap.GenerationID() != 1 {
		t.Fatalf("single transient failure Poll = (%d, %t, %v)", snap.GenerationID(), changed, err)
	}

	// Consecutive failures surface the error while preserving the last snapshot and throttling OS retries.
	if snap, changed, err := watcher.Step(context.Background()); !errors.Is(err, sampleErr) || changed || snap.GenerationID() != 1 {
		t.Fatalf("second failed Step = (%d, %t, %v)", snap.GenerationID(), changed, err)
	}
	callsAfterSecondFailure := source.calls.Load()
	for range 5 {
		snap, changed, err := watcher.Poll(context.Background(), clock())
		if !errors.Is(err, sampleErr) || changed || snap.GenerationID() != 1 {
			t.Fatalf("persistent failure Poll = (%d, %t, %v)", snap.GenerationID(), changed, err)
		}
	}
	if source.calls.Load() != callsAfterSecondFailure {
		t.Fatalf("throttled error Poll hammered OS: calls=%d want=%d", source.calls.Load(), callsAfterSecondFailure)
	}
	if watcher.Current().GenerationID() != 1 {
		t.Fatalf("Current lost last successful snapshot: gen=%d", watcher.Current().GenerationID())
	}

	// Advancing far past freshness (e.g. 24h) re-polls and surfaces the failure once per interval.
	advance(24 * time.Hour)
	snap, changed, err := watcher.Poll(context.Background(), clock())
	if !errors.Is(err, sampleErr) || changed || snap.GenerationID() != 1 {
		t.Fatalf("24h stale Poll = (%d, %t, %v)", snap.GenerationID(), changed, err)
	}
	if source.calls.Load() != callsAfterSecondFailure+1 {
		t.Fatalf("24h stale Poll calls = %d, want %d", source.calls.Load(), callsAfterSecondFailure+1)
	}

	// Recovery clears the cached error and advances the generation after debounce.
	source.setErr(nil)
	source.setIP("10.0.0.9")
	advance(time.Hour)
	if snap, _, err := watcher.Poll(context.Background(), clock()); err != nil || snap.GenerationID() != 1 {
		t.Fatalf("recovery debounce Poll = (%d, %v)", snap.GenerationID(), err)
	}
	advance(time.Millisecond)
	if snap, changed, err := watcher.Step(context.Background()); err != nil || !changed || snap.GenerationID() != 2 {
		t.Fatalf("post-recovery Step = (%d, %t, %v)", snap.GenerationID(), changed, err)
	}

	// Even without prior failed Steps, a 24h freshness expiry under an active loop surfaces injected errors.
	source.setErr(sampleErr)
	advance(24 * time.Hour)
	if snap, changed, err := watcher.Poll(context.Background(), clock()); !errors.Is(err, sampleErr) || changed || snap.GenerationID() != 2 {
		t.Fatalf("direct 24h expiry Poll = (%d, %t, %v)", snap.GenerationID(), changed, err)
	}
}

func TestWatcherCallerCancellationDoesNotPoisonSharedHealth(t *testing.T) {
	now := time.Unix(4000, 0)
	var clockMu sync.Mutex
	clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
	advance := func(d time.Duration) { clockMu.Lock(); now = now.Add(d); clockMu.Unlock() }

	source := &countingSource{}
	source.setIP("10.0.0.2")
	watcher := NewWatcher(WatcherConfig{
		Source:   source,
		Debounce: time.Nanosecond,
		Interval: time.Hour,
		Now:      clock,
	})
	defer watcher.Close()

	if snap, err := watcher.Snapshot(context.Background()); err != nil || snap.GenerationID() != 1 {
		t.Fatalf("baseline snapshot = (%d, %v)", snap.GenerationID(), err)
	}
	_, unsub := watcher.Subscribe()
	defer unsub()

	// Force a cache miss via a 24h jump and fail with a caller-canceled context.
	advance(24 * time.Hour)
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if snap, changed, err := watcher.Poll(canceledCtx, clock()); !errors.Is(err, context.Canceled) || changed || snap.GenerationID() != 1 {
		t.Fatalf("canceled Poll = (%d, %t, %v)", snap.GenerationID(), changed, err)
	}

	// A subsequent healthy caller at the same timestamp must not inherit context.Canceled.
	if snap, changed, err := watcher.Poll(context.Background(), clock()); err != nil || changed || snap.GenerationID() != 1 {
		t.Fatalf("healthy Poll after canceled caller = (%d, %t, %v)", snap.GenerationID(), changed, err)
	}
}

func TestWatcherPollWaiterCancelsWhileInFlightSampleBlocks(t *testing.T) {
	now := time.Unix(5000, 0)
	block := make(chan struct{})
	source := &countingSource{entered: make(chan struct{}, 1), block: block}
	source.setIP("10.0.0.2")
	watcher := NewWatcher(WatcherConfig{
		Source:   source,
		Debounce: time.Nanosecond,
		Interval: time.Hour,
		Now:      func() time.Time { return now },
	})
	defer watcher.Close()

	leaderDone := make(chan error, 1)
	go func() {
		snap, _, err := watcher.Poll(context.Background(), now)
		if err == nil && snap.GenerationID() != 1 {
			t.Errorf("leader generation = %d, want 1", snap.GenerationID())
		}
		leaderDone <- err
	}()
	<-source.entered

	waiterCtx, cancelWaiter := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancelWaiter()
	waiterDone := make(chan error, 1)
	go func() {
		_, _, err := watcher.Poll(waiterCtx, now)
		waiterDone <- err
	}()

	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			close(block)
			<-leaderDone
			t.Fatalf("waiter Poll err = %v, want DeadlineExceeded", err)
		}
	case <-time.After(time.Second):
		close(block)
		<-leaderDone
		t.Fatal("waiter Poll blocked past deadline while in-flight sample was stuck")
	}

	close(block)
	if err := <-leaderDone; err != nil {
		t.Fatalf("leader Poll err = %v", err)
	}
	if calls := source.calls.Load(); calls != 1 {
		t.Fatalf("source calls = %d, want 1", calls)
	}

	// Warm cache must still honor an already-canceled caller context.
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if snap, changed, err := watcher.Poll(canceledCtx, now); !errors.Is(err, context.Canceled) || changed || snap.GenerationID() != 1 {
		t.Fatalf("warm canceled Poll = (%d, %t, %v)", snap.GenerationID(), changed, err)
	}
}

func TestWatcherPollRechecksFreshnessAfterWaitingForSharedSample(t *testing.T) {
	t.Run("leader_canceled_waiter_takes_over", func(t *testing.T) {
		now := time.Unix(6000, 0)
		block := make(chan struct{})
		source := &countingSource{entered: make(chan struct{}, 2), block: block}
		source.setIP("10.0.0.2")
		watcher := NewWatcher(WatcherConfig{
			Source:   source,
			Debounce: time.Nanosecond,
			Interval: time.Hour,
			Now:      func() time.Time { return now },
		})
		defer watcher.Close()

		leaderCtx, cancelLeader := context.WithCancel(context.Background())
		leaderDone := make(chan error, 1)
		go func() {
			_, _, err := watcher.Poll(leaderCtx, now)
			leaderDone <- err
		}()
		<-source.entered

		waiterDone := make(chan Snapshot, 1)
		waiterErr := make(chan error, 1)
		go func() {
			snap, _, err := watcher.Poll(context.Background(), now)
			if err != nil {
				waiterErr <- err
				return
			}
			waiterDone <- snap
		}()

		// Let the waiter enter the shared-sample wait before canceling the leader,
		// and allow the next sample to proceed immediately once the leader exits.
		time.Sleep(5 * time.Millisecond)
		source.setBlock(nil)
		cancelLeader()
		if err := <-leaderDone; !errors.Is(err, context.Canceled) {
			t.Fatalf("leader err = %v, want Canceled", err)
		}
		select {
		case err := <-waiterErr:
			t.Fatalf("healthy waiter failed after leader canceled: %v", err)
		case snap := <-waiterDone:
			if snap.GenerationID() != 1 {
				t.Fatalf("waiter generation = %d, want 1", snap.GenerationID())
			}
		case <-time.After(time.Second):
			t.Fatal("healthy waiter did not complete after canceled leader exited")
		}
		if calls := source.calls.Load(); calls != 2 {
			t.Fatalf("source calls = %d, want 2", calls)
		}
	})

	t.Run("stale_for_waiter_timestamp", func(t *testing.T) {
		base := time.Unix(7000, 0)
		block := make(chan struct{})
		source := &countingSource{entered: make(chan struct{}, 2), block: block}
		source.setIP("10.0.0.2")
		watcher := NewWatcher(WatcherConfig{
			Source:   source,
			Debounce: time.Nanosecond,
			Interval: time.Second,
			Now:      func() time.Time { return base },
		})
		defer watcher.Close()

		leaderDone := make(chan error, 1)
		go func() {
			_, _, err := watcher.Poll(context.Background(), base)
			leaderDone <- err
		}()
		<-source.entered

		later := base.Add(2 * time.Second)
		waiterDone := make(chan error, 1)
		go func() {
			_, _, err := watcher.Poll(context.Background(), later)
			waiterDone <- err
		}()

		time.Sleep(5 * time.Millisecond)
		source.setBlock(nil)
		close(block)
		if err := <-leaderDone; err != nil {
			t.Fatalf("leader err = %v", err)
		}
		if err := <-waiterDone; err != nil {
			t.Fatalf("waiter err = %v", err)
		}
		if calls := source.calls.Load(); calls != 2 {
			t.Fatalf("stale-timestamp waiter reused expired sample: calls = %d, want 2", calls)
		}
	})
}

func TestWatcherStepCancelsWhileInFlightPollBlocks(t *testing.T) {
	now := time.Unix(8000, 0)
	block := make(chan struct{})
	source := &countingSource{entered: make(chan struct{}, 1), block: block}
	source.setIP("10.0.0.2")
	watcher := NewWatcher(WatcherConfig{
		Source:   source,
		Debounce: time.Nanosecond,
		Interval: time.Hour,
		Now:      func() time.Time { return now },
	})
	defer watcher.Close()

	leaderDone := make(chan error, 1)
	go func() {
		_, _, err := watcher.Poll(context.Background(), now)
		leaderDone <- err
	}()
	<-source.entered

	stepCtx, cancelStep := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancelStep()
	stepDone := make(chan error, 1)
	go func() {
		_, _, err := watcher.Step(stepCtx)
		stepDone <- err
	}()

	select {
	case err := <-stepDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			close(block)
			<-leaderDone
			t.Fatalf("Step err = %v, want DeadlineExceeded", err)
		}
	case <-time.After(time.Second):
		close(block)
		<-leaderDone
		t.Fatal("Step blocked past deadline while in-flight poll was stuck")
	}

	close(block)
	if err := <-leaderDone; err != nil {
		t.Fatalf("leader err = %v", err)
	}
}

type waitingContext struct {
	context.Context
	once    sync.Once
	waiting chan struct{}
}

func newWaitingContext(parent context.Context) *waitingContext {
	return &waitingContext{Context: parent, waiting: make(chan struct{})}
}

func (c *waitingContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestWatcherPollSharesNonCancellationErrorsAcrossFlightWaiters(t *testing.T) {
	t.Run("cold_start_shared_error_isolates_cancellation_and_retries_sequentially", func(t *testing.T) {
		now := time.Unix(9000, 0)
		sampleErr := errors.New("adapter query unavailable")
		block := make(chan struct{})
		source := &countingSource{entered: make(chan struct{}, 2), block: block, err: sampleErr}
		source.setIP("10.0.0.2")
		watcher := NewWatcher(WatcherConfig{
			Source:   source,
			Debounce: time.Nanosecond,
			Interval: time.Hour,
			Now:      func() time.Time { return now },
		})
		defer watcher.Close()

		leaderDone := make(chan error, 1)
		go func() {
			snap, changed, err := watcher.Poll(context.Background(), now)
			if changed || snap.GenerationID() != 0 {
				t.Errorf("leader Poll = (%d, %t, %v), want zero snapshot", snap.GenerationID(), changed, err)
			}
			leaderDone <- err
		}()
		<-source.entered

		const healthyWaiters = 4
		waiterErrs := make([]chan error, healthyWaiters)
		waiterReady := make([]*waitingContext, healthyWaiters)
		for i := range healthyWaiters {
			waiterErrs[i] = make(chan error, 1)
			waiterReady[i] = newWaitingContext(context.Background())
			go func(idx int) {
				snap, changed, err := watcher.Poll(waiterReady[idx], now)
				if changed || snap.GenerationID() != 0 {
					t.Errorf("waiter %d Poll = (%d, %t, %v), want zero snapshot", idx, snap.GenerationID(), changed, err)
				}
				waiterErrs[idx] <- err
			}(i)
		}

		cancelParent, cancelWaiter := context.WithCancel(context.Background())
		defer cancelWaiter()
		canceledCtx := newWaitingContext(cancelParent)
		canceledDone := make(chan error, 1)
		go func() {
			_, _, err := watcher.Poll(canceledCtx, now)
			canceledDone <- err
		}()

		for _, wctx := range waiterReady {
			<-wctx.waiting
		}
		<-canceledCtx.waiting

		cancelWaiter()
		select {
		case err := <-canceledDone:
			if !errors.Is(err, context.Canceled) {
				close(block)
				<-leaderDone
				t.Fatalf("canceled waiter err = %v, want Canceled", err)
			}
		case <-time.After(time.Second):
			close(block)
			<-leaderDone
			t.Fatal("canceled waiter did not exit promptly")
		}

		source.setBlock(nil)
		close(block)

		if err := <-leaderDone; !errors.Is(err, sampleErr) {
			t.Fatalf("leader err = %v, want %v", err, sampleErr)
		}
		for i, ch := range waiterErrs {
			if err := <-ch; !errors.Is(err, sampleErr) {
				t.Fatalf("waiter %d err = %v, want %v", i, err, sampleErr)
			}
		}
		if calls := source.calls.Load(); calls != 1 {
			t.Fatalf("cold-start concurrent failure calls = %d, want 1", calls)
		}

		// Cold-start failure must not negative-cache subsequent sequential callers.
		source.setErr(nil)
		snap, _, err := watcher.Poll(context.Background(), now)
		if err != nil || snap.GenerationID() != 1 {
			t.Fatalf("sequential recovery Poll = (%d, %v), want gen 1", snap.GenerationID(), err)
		}
		if calls := source.calls.Load(); calls != 2 {
			t.Fatalf("post-recovery calls = %d, want 2", calls)
		}
	})

	t.Run("idle_stale_shared_error_preserves_last_snapshot", func(t *testing.T) {
		now := time.Unix(10000, 0)
		source := &countingSource{}
		source.setIP("10.0.0.2")
		watcher := NewWatcher(WatcherConfig{
			Source:   source,
			Debounce: time.Nanosecond,
			Interval: time.Second,
			Now:      func() time.Time { return now },
		})
		defer watcher.Close()

		if snap, _, err := watcher.Poll(context.Background(), now); err != nil || snap.GenerationID() != 1 {
			t.Fatalf("baseline Poll = (%d, %v)", snap.GenerationID(), err)
		}

		sampleErr := errors.New("route table unavailable")
		block := make(chan struct{})
		source.entered = make(chan struct{}, 2)
		source.setBlock(block)
		source.setErr(sampleErr)

		stale := now.Add(2 * time.Second)
		leaderDone := make(chan error, 1)
		go func() {
			snap, changed, err := watcher.Poll(context.Background(), stale)
			if changed || snap.GenerationID() != 1 {
				t.Errorf("leader stale Poll = (%d, %t, %v), want gen 1", snap.GenerationID(), changed, err)
			}
			leaderDone <- err
		}()
		<-source.entered

		const waiters = 3
		waiterErrs := make([]chan error, waiters)
		waiterCtxs := make([]*waitingContext, waiters)
		for i := range waiters {
			waiterErrs[i] = make(chan error, 1)
			waiterCtxs[i] = newWaitingContext(context.Background())
			go func(idx int) {
				snap, changed, err := watcher.Poll(waiterCtxs[idx], stale)
				if changed || snap.GenerationID() != 1 {
					t.Errorf("waiter %d stale Poll = (%d, %t, %v), want gen 1", idx, snap.GenerationID(), changed, err)
				}
				waiterErrs[idx] <- err
			}(i)
		}
		for _, wctx := range waiterCtxs {
			<-wctx.waiting
		}

		source.setBlock(nil)
		close(block)

		if err := <-leaderDone; !errors.Is(err, sampleErr) {
			t.Fatalf("leader err = %v, want %v", err, sampleErr)
		}
		for i, ch := range waiterErrs {
			if err := <-ch; !errors.Is(err, sampleErr) {
				t.Fatalf("waiter %d err = %v, want %v", i, err, sampleErr)
			}
		}
		if calls := source.calls.Load(); calls != 2 {
			t.Fatalf("idle stale concurrent failure calls = %d, want 2", calls)
		}
	})

	t.Run("cold_start_initial_poll_shares_in_flight_failure", func(t *testing.T) {
		now := time.Unix(11000, 0)
		sampleErr := errors.New("adapter query unavailable")
		block := make(chan struct{})
		source := &countingSource{entered: make(chan struct{}, 2), block: block, err: sampleErr}
		watcher := NewWatcher(WatcherConfig{
			Source:   source,
			Debounce: time.Nanosecond,
			Interval: time.Hour,
			Now:      func() time.Time { return now },
		})
		defer watcher.Close()

		leaderDone := make(chan error, 1)
		go func() {
			_, _, err := watcher.Poll(context.Background(), now)
			leaderDone <- err
		}()
		<-source.entered

		initCtx := newWaitingContext(context.Background())
		initDone := make(chan struct{})
		go func() {
			watcher.initialPoll(initCtx)
			close(initDone)
		}()
		<-initCtx.waiting

		source.setBlock(nil)
		close(block)

		if err := <-leaderDone; !errors.Is(err, sampleErr) {
			t.Fatalf("leader err = %v, want %v", err, sampleErr)
		}
		<-initDone
		if calls := source.calls.Load(); calls != 1 {
			t.Fatalf("initialPoll after shared cold-start failure calls = %d, want 1", calls)
		}
	})
}
