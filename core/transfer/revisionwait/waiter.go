// Package revisionwait owns receiver-side scheduling for authenticated sender
// capacity backpressure. Session adapters construct CapacitySignal only after
// authenticating a retryable capacity denial; TransferJob serializes file opens
// through one Waiter so waits share a bounded wall-clock budget and wake early
// on cancellation or protocol-session lifecycle changes.
package revisionwait

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/windshare/windshare/core/session/protocolsession"
)

const (
	MinimumRetryHint           = time.Millisecond
	MaximumRetryHint           = 30 * time.Second
	DefaultWaitBudget          = 2 * time.Minute
	MaximumWaitBudget          = 10 * time.Minute
	DefaultAdditiveJitterLimit = 250 * time.Millisecond
	MaximumAdditiveJitterLimit = time.Second
	DefaultVisibilityThreshold = 500 * time.Millisecond
	MaximumVisibilityThreshold = 5 * time.Second
)

var (
	ErrInvalidConfig         = errors.New("revision wait policy configuration is invalid")
	ErrInvalidCapacitySignal = errors.New("revision capacity signal is invalid")
	ErrWaitBudgetExhausted   = errors.New("revision capacity wait budget exhausted")
	ErrSessionEnded          = errors.New("revision wait protocol session ended")
	ErrSessionChanged        = errors.New("revision wait protocol session changed")
	ErrJitterSource          = errors.New("revision capacity jitter source failed")
	ErrTimerContract         = errors.New("revision capacity timer contract failed")
)

// CapacitySignal carries scheduling authority only when returned directly by an
// authenticated session adapter. MatchCapacitySignal refuses wrapped errors so
// diagnostic error graphs cannot inherit retry authority.
type CapacitySignal struct {
	retryAfter        time.Duration
	protocolSession   protocolsession.ProtocolSessionID
	protocolOperation protocolsession.OperationID
}

func NewCapacitySignal(
	retryAfter time.Duration,
	session protocolsession.ProtocolSessionID,
	operation protocolsession.OperationID,
) (*CapacitySignal, error) {
	signal := &CapacitySignal{
		retryAfter:        retryAfter,
		protocolSession:   session,
		protocolOperation: operation,
	}
	if !signal.valid() {
		return nil, ErrInvalidCapacitySignal
	}
	return signal, nil
}

func (s *CapacitySignal) Error() string {
	if s == nil {
		return ErrInvalidCapacitySignal.Error()
	}
	return fmt.Sprintf("revision capacity is busy; retry after %s", s.retryAfter)
}

func (s *CapacitySignal) RetryAfter() time.Duration { return s.retryAfter }
func (s *CapacitySignal) ProtocolSession() protocolsession.ProtocolSessionID {
	return s.protocolSession
}
func (s *CapacitySignal) ProtocolOperation() protocolsession.OperationID { return s.protocolOperation }

func (s *CapacitySignal) valid() bool {
	return s != nil && s.retryAfter >= MinimumRetryHint && s.retryAfter <= MaximumRetryHint &&
		s.retryAfter%time.Millisecond == 0 &&
		!s.protocolSession.IsZero() && !s.protocolOperation.IsZero()
}

// MatchCapacitySignal accepts only a direct *CapacitySignal value; wrapping a
// signal in fmt.Errorf or errors.Join intentionally strips retry authority.
func MatchCapacitySignal(err error) (*CapacitySignal, bool) {
	signal, ok := err.(*CapacitySignal) //nolint:errorlint // Intentionally rejects wrapped signals.
	return signal, ok && signal.valid()
}

type Snapshot struct {
	ActiveWaiters   uint32
	ActiveSince     time.Time
	VisibleAfter    time.Time
	AccumulatedWait time.Duration
	Attempts        uint64
}

func (s Snapshot) Visible(now time.Time) bool {
	return s.ActiveWaiters != 0 && !s.VisibleAfter.IsZero() && !now.Before(s.VisibleAfter)
}

func (s Snapshot) ActiveDuration(now time.Time) time.Duration {
	if s.ActiveWaiters == 0 || s.ActiveSince.IsZero() || now.Before(s.ActiveSince) {
		return 0
	}
	return now.Sub(s.ActiveSince)
}

type TraceStage uint8

const (
	TraceRetryScheduled TraceStage = iota + 1
	TraceRetryReady
	TraceRetrySucceeded
	TraceBudgetPaused
	TraceWaitCanceled
	TraceSessionEnded
)

type Trace struct {
	Stage             TraceStage
	ProtocolSession   protocolsession.ProtocolSessionID
	ProtocolOperation protocolsession.OperationID
	Attempt           uint64
	Hint              time.Duration
	Jitter            time.Duration
	Delay             time.Duration
	AccumulatedWait   time.Duration
	ActiveWaiters     uint32
	Cause             error
}

type WaitOutcome uint8

const (
	WaitRetry WaitOutcome = iota + 1
	WaitBudgetPaused
	WaitCanceled
	WaitSessionEnded
)

// SessionWatchFunc blocks until expected is replaced, ends, or ctx is canceled.
// Returning a non-zero session ID different from expected authorizes an
// immediate retry on the replacement session; returning a zero ID or an error
// terminates waiting.
type SessionWatchFunc func(ctx context.Context, expected protocolsession.ProtocolSessionID) (protocolsession.ProtocolSessionID, error)

// WatchLifetime adapts a fixed single-session runtime context to SessionWatchFunc.
func WatchLifetime(session protocolsession.ProtocolSessionID, lifetime context.Context) SessionWatchFunc {
	return func(ctx context.Context, expected protocolsession.ProtocolSessionID) (protocolsession.ProtocolSessionID, error) {
		if session.IsZero() || lifetime == nil || expected.IsZero() {
			return protocolsession.ProtocolSessionID{}, ErrSessionEnded
		}
		if expected != session {
			if lifetime.Err() != nil {
				return protocolsession.ProtocolSessionID{}, lifetimeCause(lifetime)
			}
			return session, nil
		}
		select {
		case <-ctx.Done():
			return protocolsession.ProtocolSessionID{}, context.Cause(ctx)
		case <-lifetime.Done():
			return protocolsession.ProtocolSessionID{}, lifetimeCause(lifetime)
		}
	}
}

func lifetimeCause(lifetime context.Context) error {
	if cause := context.Cause(lifetime); cause != nil {
		return cause
	}
	return ErrSessionEnded
}

type Config struct {
	WaitBudget          time.Duration
	AdditiveJitterLimit time.Duration
	VisibilityThreshold time.Duration
	WatchSession        SessionWatchFunc
	// Optional deterministic hooks for tests; nil uses wall clock, time.NewTimer, and crypto/rand.
	Now    func() time.Time
	After  func(time.Duration) (<-chan time.Time, func() bool)
	Jitter func(limit time.Duration) (time.Duration, error)
}

type Waiter struct {
	budget              time.Duration
	jitterLimit         time.Duration
	visibilityThreshold time.Duration
	watchSession        SessionWatchFunc
	now                 func() time.Time
	after               func(time.Duration) (<-chan time.Time, func() bool)
	jitter              func(time.Duration) (time.Duration, error)

	mu          sync.Mutex
	active      bool
	activeSince time.Time
	accumulated time.Duration
	attempts    uint64
	opAttempts  uint64
	session     protocolsession.ProtocolSessionID
	lastSignal  *CapacitySignal
}

func NewWaiter(config Config) (*Waiter, error) {
	if config.WaitBudget == 0 {
		config.WaitBudget = DefaultWaitBudget
	}
	if config.AdditiveJitterLimit == 0 {
		config.AdditiveJitterLimit = DefaultAdditiveJitterLimit
	}
	if config.VisibilityThreshold == 0 {
		config.VisibilityThreshold = DefaultVisibilityThreshold
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.After == nil {
		config.After = wallAfter
	}
	if config.Jitter == nil {
		config.Jitter = defaultJitter
	}
	if config.WaitBudget <= 0 || config.WaitBudget > MaximumWaitBudget ||
		config.AdditiveJitterLimit < 0 || config.AdditiveJitterLimit > MaximumAdditiveJitterLimit ||
		config.VisibilityThreshold < 0 || config.VisibilityThreshold > MaximumVisibilityThreshold ||
		config.WatchSession == nil {
		return nil, ErrInvalidConfig
	}
	return &Waiter{
		budget:              config.WaitBudget,
		jitterLimit:         config.AdditiveJitterLimit,
		visibilityThreshold: config.VisibilityThreshold,
		watchSession:        config.WatchSession,
		now:                 config.Now,
		after:               config.After,
		jitter:              config.Jitter,
	}, nil
}

func (w *Waiter) Snapshot() Snapshot {
	if w == nil {
		return Snapshot{}
	}
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.snapshotLocked(now)
}

func (w *Waiter) snapshotLocked(now time.Time) Snapshot {
	accumulated := w.accumulated
	var activeCount uint32
	var visibleAfter time.Time
	if w.active {
		activeCount = 1
		if !w.activeSince.IsZero() && now.After(w.activeSince) {
			accumulated = saturatingDurationAdd(accumulated, now.Sub(w.activeSince))
		}
		visibleAfter = w.activeSince.Add(w.visibilityThreshold)
	}
	return Snapshot{
		ActiveWaiters:   activeCount,
		ActiveSince:     w.activeSince,
		VisibleAfter:    visibleAfter,
		AccumulatedWait: accumulated,
		Attempts:        w.attempts,
	}
}

func (w *Waiter) Wait(
	ctx context.Context,
	signal *CapacitySignal,
	observe func(Snapshot),
	trace func(Trace),
) (WaitOutcome, error) {
	if w == nil || ctx == nil || !signal.valid() {
		return 0, ErrInvalidCapacitySignal
	}
	now := w.now()
	w.mu.Lock()
	if !w.active {
		w.active = true
		w.activeSince = now
		w.session = signal.ProtocolSession()
		w.opAttempts = 0
	} else if w.session != signal.ProtocolSession() {
		w.mu.Unlock()
		return w.finish(TraceSessionEnded, ErrSessionChanged, WaitSessionEnded, observe, trace)
	}
	w.attempts++
	w.opAttempts++
	attempt := w.opAttempts
	w.lastSignal = signal
	snap := w.snapshotLocked(now)
	remaining := max(w.budget-snap.AccumulatedWait, 0)
	w.mu.Unlock()

	callObserve(observe, snap)
	if remaining <= 0 {
		return w.finish(TraceBudgetPaused, ErrWaitBudgetExhausted, WaitBudgetPaused, observe, trace)
	}
	jitter, err := w.jitter(w.jitterLimit)
	if err != nil || jitter < 0 || jitter > w.jitterLimit {
		w.Stop(observe)
		return 0, errors.Join(ErrJitterSource, err)
	}
	delay := saturatingDurationAdd(signal.RetryAfter(), jitter)
	waitFor := min(delay, remaining)
	budgetBound := delay >= remaining
	callTrace(trace, Trace{
		Stage:             TraceRetryScheduled,
		ProtocolSession:   signal.ProtocolSession(),
		ProtocolOperation: signal.ProtocolOperation(),
		Attempt:           attempt,
		Hint:              signal.RetryAfter(),
		Jitter:            jitter,
		Delay:             waitFor,
		AccumulatedWait:   snap.AccumulatedWait,
		ActiveWaiters:     snap.ActiveWaiters,
	})
	if waitFor <= 0 {
		return w.finish(TraceBudgetPaused, ErrWaitBudgetExhausted, WaitBudgetPaused, observe, trace)
	}
	return w.sleep(ctx, signal, waitFor, budgetBound, observe, trace)
}

func (w *Waiter) sleep(
	ctx context.Context,
	signal *CapacitySignal,
	delay time.Duration,
	budgetBound bool,
	observe func(Snapshot),
	trace func(Trace),
) (WaitOutcome, error) {
	timerC, stopTimer := w.after(delay)
	if timerC == nil {
		w.Stop(observe)
		return 0, ErrTimerContract
	}
	watchCtx, cancelWatch := context.WithCancel(ctx)
	type watchResult struct {
		next protocolsession.ProtocolSessionID
		err  error
	}
	watchDone := make(chan watchResult, 1)
	go func() {
		next, err := w.watchSession(watchCtx, signal.ProtocolSession())
		watchDone <- watchResult{next: next, err: err}
	}()

	select {
	case <-ctx.Done():
		if stopTimer != nil {
			stopTimer()
		}
		cancelWatch()
		<-watchDone
		return w.finish(TraceWaitCanceled, context.Cause(ctx), WaitCanceled, observe, trace)
	case watched := <-watchDone:
		if stopTimer != nil {
			stopTimer()
		}
		cancelWatch()
		if cause := context.Cause(ctx); cause != nil {
			return w.finish(TraceWaitCanceled, cause, WaitCanceled, observe, trace)
		}
		if watched.err == nil && !watched.next.IsZero() && watched.next != signal.ProtocolSession() {
			// Authenticated session replacement wakes the waiter immediately while
			// preserving accumulated wall-clock budget across generations.
			w.mu.Lock()
			w.session = watched.next
			snap := w.snapshotLocked(w.now())
			w.mu.Unlock()
			callObserve(observe, snap)
			w.traceLast(TraceRetryReady, nil, snap, trace)
			return WaitRetry, nil
		}
		cause := watched.err
		if cause == nil {
			cause = ErrSessionEnded
		}
		return w.finish(TraceSessionEnded, cause, WaitSessionEnded, observe, trace)
	case <-timerC:
		cancelWatch()
		<-watchDone
		if cause := context.Cause(ctx); cause != nil {
			return w.finish(TraceWaitCanceled, cause, WaitCanceled, observe, trace)
		}
		snap := w.Snapshot()
		callObserve(observe, snap)
		if budgetBound || snap.AccumulatedWait >= w.budget {
			return w.finish(TraceBudgetPaused, ErrWaitBudgetExhausted, WaitBudgetPaused, observe, trace)
		}
		w.traceLast(TraceRetryReady, nil, snap, trace)
		return WaitRetry, nil
	}
}

func (w *Waiter) Succeed(observe func(Snapshot), trace func(Trace)) {
	if w == nil {
		return
	}
	_, _ = w.finish(TraceRetrySucceeded, nil, 0, observe, trace)
}

func (w *Waiter) Cancel(cause error, observe func(Snapshot), trace func(Trace)) {
	if w == nil {
		return
	}
	if cause == nil {
		cause = context.Canceled
	}
	_, _ = w.finish(TraceWaitCanceled, cause, WaitCanceled, observe, trace)
}

// Stop clears active wait progress without emitting a terminal wait trace when
// another error supersedes capacity retry authority.
func (w *Waiter) Stop(observe func(Snapshot)) {
	if w == nil {
		return
	}
	now := w.now()
	w.mu.Lock()
	if !w.active {
		w.mu.Unlock()
		return
	}
	snap := w.deactivateLocked(now)
	w.mu.Unlock()
	callObserve(observe, snap)
}

func (w *Waiter) finish(
	stage TraceStage,
	cause error,
	outcome WaitOutcome,
	observe func(Snapshot),
	trace func(Trace),
) (WaitOutcome, error) {
	now := w.now()
	w.mu.Lock()
	if !w.active {
		w.mu.Unlock()
		return outcome, cause
	}
	signal := w.lastSignal
	attempt := w.opAttempts
	snap := w.deactivateLocked(now)
	w.mu.Unlock()

	callObserve(observe, snap)
	callTrace(trace, buildTrace(stage, signal, attempt, snap, cause))
	return outcome, cause
}

func (w *Waiter) deactivateLocked(now time.Time) Snapshot {
	if !w.activeSince.IsZero() && now.After(w.activeSince) {
		w.accumulated = saturatingDurationAdd(w.accumulated, now.Sub(w.activeSince))
	}
	w.active = false
	w.activeSince = time.Time{}
	w.session = protocolsession.ProtocolSessionID{}
	w.opAttempts = 0
	w.lastSignal = nil
	return w.snapshotLocked(now)
}

func (w *Waiter) traceLast(stage TraceStage, cause error, snap Snapshot, trace func(Trace)) {
	w.mu.Lock()
	signal := w.lastSignal
	attempt := w.opAttempts
	w.mu.Unlock()
	callTrace(trace, buildTrace(stage, signal, attempt, snap, cause))
}

func buildTrace(stage TraceStage, signal *CapacitySignal, attempt uint64, snap Snapshot, cause error) Trace {
	event := Trace{
		Stage:           stage,
		Attempt:         attempt,
		AccumulatedWait: snap.AccumulatedWait,
		ActiveWaiters:   snap.ActiveWaiters,
		Cause:           cause,
	}
	if signal != nil {
		event.ProtocolSession = signal.ProtocolSession()
		event.ProtocolOperation = signal.ProtocolOperation()
		event.Hint = signal.RetryAfter()
	}
	return event
}

func callObserve(observe func(Snapshot), snap Snapshot) {
	if observe == nil {
		return
	}
	defer func() { _ = recover() }()
	observe(snap)
}

func callTrace(trace func(Trace), event Trace) {
	if trace == nil {
		return
	}
	defer func() { _ = recover() }()
	trace(event)
}

func wallAfter(delay time.Duration) (<-chan time.Time, func() bool) {
	timer := time.NewTimer(delay)
	return timer.C, timer.Stop
}

func defaultJitter(limit time.Duration) (time.Duration, error) {
	if limit <= 0 {
		return 0, nil
	}
	var encoded [8]byte
	if _, err := io.ReadFull(rand.Reader, encoded[:]); err != nil {
		return 0, err
	}
	return time.Duration(binary.LittleEndian.Uint64(encoded[:]) % (uint64(limit) + 1)), nil
}

func saturatingDurationAdd(left, right time.Duration) time.Duration {
	if right > 0 && left > time.Duration(1<<63-1)-right {
		return time.Duration(1<<63 - 1)
	}
	return left + right
}
