package networkstate

import (
	"context"
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
