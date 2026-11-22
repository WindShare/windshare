package networkstate

import (
	"context"
	"sync"
	"time"
)

const (
	// Active loops allow one extra interval for ticker jitter and absorb a
	// single transient OS sampling glitch before failing pull callers.
	activeFreshnessMultiplier = 2
	activeFailureThreshold    = 2
)

type WatcherConfig struct {
	Source   Source
	Debounce time.Duration
	Interval time.Duration
	Now      func() time.Time
}

// pollFlight tracks one in-flight OS sample shared across concurrent waiters.
// The flight owner writes snapshot and err before closing done, establishing
// a happens-before edge for waiters without holding w.mu on read.
type pollFlight struct {
	done     chan struct{}
	snapshot Snapshot
	err      error
}

// Watcher owns one host-wide polling cadence and fans out immutable generation
// transitions to all subscribers. Polling runs only while at least one
// subscriber is active; Snapshot and Poll reuse recent observations so
// concurrent callers never multiply OS queries, while still surfacing expired
// snapshots and persistent background sampling failures.
type Watcher struct {
	mu          sync.Mutex
	polling     *pollFlight
	monitor     *Monitor
	interval    time.Duration
	now         func() time.Time
	polledAt    time.Time
	attemptedAt time.Time
	lastErr     error
	failedPolls int
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
// while the cached snapshot is fresh. Concurrent cache misses wait on either
// the in-flight sample or their own context cancellation; non-cancellation
// sampling errors are shared with that round's waiters, while caller
// cancellation stays isolated and successes recheck snapshot freshness.
func (w *Watcher) Poll(ctx context.Context, now time.Time) (Snapshot, bool, error) {
	for {
		if err := ctx.Err(); err != nil {
			return w.Current(), false, err
		}
		w.mu.Lock()
		if snapshot, err, ok := w.cachedLocked(now); ok {
			w.mu.Unlock()
			return snapshot, false, err
		}
		if wait := w.polling; wait != nil {
			w.mu.Unlock()
			select {
			case <-wait.done:
				if err := ctx.Err(); err != nil {
					return w.Current(), false, err
				}
				if wait.err != nil {
					return wait.snapshot, false, wait.err
				}
				continue
			case <-ctx.Done():
				return w.Current(), false, ctx.Err()
			}
		}
		flight := &pollFlight{done: make(chan struct{})}
		w.polling = flight
		w.mu.Unlock()

		defer w.releasePoll(flight)
		return w.executePoll(ctx, now, flight)
	}
}

// Step forces one poll against the underlying monitor and broadcasts any true
// generation transition (after the initial baseline) to active subscribers.
func (w *Watcher) Step(ctx context.Context) (Snapshot, bool, error) {
	flight, err := w.claimPoll(ctx)
	if err != nil {
		return w.Current(), false, err
	}
	defer w.releasePoll(flight)
	return w.executePoll(ctx, w.now(), flight)
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
		initialPoll := w.needsInitialPollLocked()
		w.loopActive = true
		w.loopCancel = cancel
		w.loopDone = done
		w.draining = nil
		w.wg.Go(func() { w.run(loopCtx, prev, done, initialPoll) })
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

func (w *Watcher) cachedLocked(now time.Time) (Snapshot, error, bool) {
	if w.current.GenerationID() == 0 {
		return Snapshot{}, nil, false
	}
	if w.loopActive {
		if w.failedPolls < activeFailureThreshold && withinWindow(now, w.polledAt, activeFreshnessMultiplier*w.interval) {
			return w.current, nil, true
		}
		if w.lastErr != nil && withinWindow(now, w.attemptedAt, w.interval) {
			return w.current, w.lastErr, true
		}
		return Snapshot{}, nil, false
	}
	if w.lastErr == nil && withinWindow(now, w.polledAt, w.interval) {
		return w.current, nil, true
	}
	return Snapshot{}, nil, false
}

func withinWindow(now, since time.Time, window time.Duration) bool {
	if since.IsZero() {
		return false
	}
	elapsed := now.Sub(since)
	return elapsed >= 0 && elapsed < window
}

func (w *Watcher) claimPoll(ctx context.Context) (*pollFlight, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		w.mu.Lock()
		if wait := w.polling; wait != nil {
			w.mu.Unlock()
			select {
			case <-wait.done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		flight := &pollFlight{done: make(chan struct{})}
		w.polling = flight
		w.mu.Unlock()
		return flight, nil
	}
}

func (w *Watcher) releasePoll(flight *pollFlight) {
	w.mu.Lock()
	w.polling = nil
	close(flight.done)
	w.mu.Unlock()
}

// executePoll runs one OS sample while the caller owns the active polling slot
// and does not hold w.mu.
func (w *Watcher) executePoll(ctx context.Context, now time.Time, flight *pollFlight) (Snapshot, bool, error) {
	snapshot, changed, err := w.monitor.Poll(ctx, now)
	if err != nil {
		w.mu.Lock()
		current := w.current
		// Caller or loop-shutdown cancellation must not poison shared health or
		// fail concurrent healthy waiters riding the same in-flight sample.
		if ctx.Err() == nil {
			w.attemptedAt = now
			w.lastErr = err
			w.failedPolls++
			flight.snapshot = current
			flight.err = err
		}
		w.mu.Unlock()
		return current, false, err
	}
	w.mu.Lock()
	previous := w.current.GenerationID()
	w.current = snapshot
	w.polledAt = now
	w.attemptedAt = now
	w.lastErr = nil
	w.failedPolls = 0
	if changed && previous != 0 && snapshot.GenerationID() != previous {
		for _, ch := range w.subscribers {
			deliverSnapshot(ch, snapshot)
		}
	}
	w.mu.Unlock()
	return snapshot, changed, nil
}

func (w *Watcher) run(ctx context.Context, prev <-chan struct{}, done chan struct{}, initialPoll bool) {
	defer close(done)
	if prev != nil {
		select {
		case <-ctx.Done():
			return
		case <-prev:
		}
	}
	defer func() {
		// Wait for any in-flight poll before clearing the resume gap so a
		// concurrent sample cannot repopulate lastPoll after the loop goes idle.
		flight, _ := w.claimPoll(context.Background())
		w.monitor.ResetPollGap()
		w.releasePoll(flight)
	}()

	if initialPoll || prev != nil {
		w.initialPoll(ctx)
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

// initialPoll rechecks freshness after any in-flight poll so loop startup
// shares a concurrent cold-start Snapshot/Poll instead of issuing a second OS
// query, while still exiting promptly if the loop is canceled while waiting.
func (w *Watcher) initialPoll(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		w.mu.Lock()
		if !w.needsInitialPollLocked() {
			w.mu.Unlock()
			return
		}
		if wait := w.polling; wait != nil {
			w.mu.Unlock()
			select {
			case <-wait.done:
				if wait.err != nil {
					return
				}
				continue
			case <-ctx.Done():
				return
			}
		}
		flight := &pollFlight{done: make(chan struct{})}
		w.polling = flight
		w.mu.Unlock()

		defer w.releasePoll(flight)
		_, _, _ = w.executePoll(ctx, w.now(), flight)
		return
	}
}

func (w *Watcher) needsInitialPollLocked() bool {
	if w.current.GenerationID() == 0 || w.polledAt.IsZero() || w.lastErr != nil {
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
