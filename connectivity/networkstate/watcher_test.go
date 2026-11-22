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
	mu    sync.Mutex
	calls atomic.Int64
	state State
	err   error
	block chan struct{}
}

func (s *countingSource) Snapshot(ctx context.Context) (State, error) {
	s.calls.Add(1)
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return State{}, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, s.err
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
