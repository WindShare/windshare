package networkstate

import (
	"context"
	"sync"
	"time"
)

type WatcherConfig struct {
	Source   Source
	Debounce time.Duration
	Interval time.Duration
	Now      func() time.Time
}

// Watcher owns one host-wide polling cadence and fans out immutable generation
// transitions to all subscribers. Polling runs only while at least one
// subscriber is active; Snapshot and Poll reuse the latest observation while
// active or within Interval so concurrent callers never multiply OS queries.
type Watcher struct {
	mu          sync.Mutex
	pollMu      sync.Mutex
	monitor     *Monitor
	interval    time.Duration
	now         func() time.Time
	polledAt    time.Time
	current     Snapshot
	subscribers map[uint64]chan Snapshot
	nextID      uint64
	loopActive  bool
	loopCancel  context.CancelFunc
	loopDone    chan struct{}
	draining    chan struct{}
	wg          sync.WaitGroup
	closed      bool
}

func NewWatcher(config WatcherConfig) *Watcher {
	interval := config.Interval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &Watcher{
		monitor:     NewMonitor(config.Source, config.Debounce),
		interval:    interval,
		now:         now,
		subscribers: make(map[uint64]chan Snapshot),
	}
}

func (w *Watcher) Current() Snapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.current
}

func (w *Watcher) Snapshot(ctx context.Context) (Snapshot, error) {
	snapshot, _, err := w.Poll(ctx, w.now())
	return snapshot, err
}

// Poll satisfies pull-based consumers without triggering redundant OS queries
// while a shared watcher loop is active or its last sample is still fresh.
func (w *Watcher) Poll(ctx context.Context, now time.Time) (Snapshot, bool, error) {
	w.mu.Lock()
	if snapshot, ok := w.freshLocked(now); ok {
		w.mu.Unlock()
		return snapshot, false, nil
	}
	w.mu.Unlock()

	w.pollMu.Lock()
	defer w.pollMu.Unlock()

	w.mu.Lock()
	if snapshot, ok := w.freshLocked(now); ok {
		w.mu.Unlock()
		return snapshot, false, nil
	}
	w.mu.Unlock()
	return w.pollLocked(ctx, now)
}

// Step forces one poll against the underlying monitor and broadcasts any true
// generation transition (after the initial baseline) to active subscribers.
func (w *Watcher) Step(ctx context.Context) (Snapshot, bool, error) {
	w.pollMu.Lock()
	defer w.pollMu.Unlock()
	return w.pollLocked(ctx, w.now())
}

// Subscribe registers for true network generation changes (never the initial
// 0 -> 1 baseline). The shared polling loop starts on the first subscriber and
// stops once the last subscriber cancels.
func (w *Watcher) Subscribe() (<-chan Snapshot, func()) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		closed := make(chan Snapshot)
		close(closed)
		return closed, func() {}
	}
	w.nextID++
	id := w.nextID
	ch := make(chan Snapshot, 1)
	w.subscribers[id] = ch
	if !w.loopActive {
		loopCtx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		prev := w.draining
		w.loopActive = true
		w.loopCancel = cancel
		w.loopDone = done
		w.draining = nil
		w.wg.Go(func() { w.run(loopCtx, prev, done) })
	}
	w.mu.Unlock()
	return ch, sync.OnceFunc(func() { w.unsubscribe(id) })
}

// Watch subscribes until ctx is canceled, invoking onChange for each subsequent
// network generation transition.
func (w *Watcher) Watch(ctx context.Context, onChange func(Snapshot)) {
	if w == nil || onChange == nil {
		<-ctx.Done()
		return
	}
	changes, unsubscribe := w.Subscribe()
	defer unsubscribe()
	for {
		select {
		case <-ctx.Done():
			return
		case snapshot, ok := <-changes:
			if !ok {
				return
			}
			onChange(snapshot)
		}
	}
}

func (w *Watcher) Close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	cancel := w.loopCancel
	w.loopActive = false
	w.loopCancel = nil
	w.loopDone = nil
	for id, ch := range w.subscribers {
		close(ch)
		delete(w.subscribers, id)
	}
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	w.wg.Wait()
}

func (w *Watcher) unsubscribe(id uint64) {
	w.mu.Lock()
	delete(w.subscribers, id)
	var cancel context.CancelFunc
	var done chan struct{}
	if len(w.subscribers) == 0 && w.loopActive {
		cancel, done = w.loopCancel, w.loopDone
		w.loopActive = false
		w.loopCancel = nil
		w.loopDone = nil
		w.draining = done
	}
	w.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (w *Watcher) freshLocked(now time.Time) (Snapshot, bool) {
	if w.current.GenerationID() == 0 {
		return Snapshot{}, false
	}
	if w.loopActive {
		return w.current, true
	}
	if !w.polledAt.IsZero() {
		elapsed := now.Sub(w.polledAt)
		if elapsed >= 0 && elapsed < w.interval {
			return w.current, true
		}
	}
	return Snapshot{}, false
}

func (w *Watcher) pollLocked(ctx context.Context, now time.Time) (Snapshot, bool, error) {
	snapshot, changed, err := w.monitor.Poll(ctx, now)
	if err != nil {
		return w.Current(), false, err
	}
	w.mu.Lock()
	previous := w.current.GenerationID()
	w.current = snapshot
	w.polledAt = now
	if changed && previous != 0 && snapshot.GenerationID() != previous {
		for _, ch := range w.subscribers {
			deliverSnapshot(ch, snapshot)
		}
	}
	w.mu.Unlock()
	return snapshot, changed, nil
}

func (w *Watcher) run(ctx context.Context, prev <-chan struct{}, done chan struct{}) {
	defer close(done)
	if prev != nil {
		select {
		case <-ctx.Done():
			return
		case <-prev:
		}
	}
	defer func() {
		w.pollMu.Lock()
		w.monitor.ResetPollGap()
		w.pollMu.Unlock()
	}()

	if w.needsInitialPoll() {
		_, _, _ = w.Step(ctx)
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _, _ = w.Step(ctx)
		}
	}
}

func (w *Watcher) needsInitialPoll() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.current.GenerationID() == 0 || w.polledAt.IsZero() {
		return true
	}
	elapsed := w.now().Sub(w.polledAt)
	return elapsed < 0 || elapsed >= w.interval
}

func deliverSnapshot(ch chan Snapshot, snapshot Snapshot) {
	select {
	case ch <- snapshot:
	default:
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- snapshot:
		default:
		}
	}
}
