package revisionwait

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/windshare/windshare/core/session/protocolsession"
)

const (
	DefaultWaitBudget          = 2 * time.Minute
	MaximumWaitBudget          = 10 * time.Minute
	DefaultAdditiveJitterLimit = 250 * time.Millisecond
	MaximumAdditiveJitterLimit = time.Second
	DefaultVisibilityThreshold = 500 * time.Millisecond
	MaximumVisibilityThreshold = 5 * time.Second
	WaitIdentityBytes          = protocolsession.IdentityBytes
)

var (
	ErrInvalidConfig       = errors.New("revision wait policy configuration is invalid")
	ErrWaitClosed          = errors.New("revision capacity wait is already closed")
	ErrWaitBudgetExhausted = errors.New("revision capacity wait budget exhausted")
	ErrWaitIdentity        = errors.New("revision capacity wait identity generation failed")
	ErrJitterSource        = errors.New("revision capacity jitter source failed")
	ErrTimerContract       = errors.New("revision capacity timer contract failed")
	ErrGenerationContract  = errors.New("revision capacity generation fence contract failed")
)

type Clock interface {
	Now() time.Time
}

type Timer interface {
	Done() <-chan time.Time
	Stop() bool
}

type TimerFactory interface {
	NewTimer(time.Duration) Timer
}

type JitterSource interface {
	AdditiveJitter(time.Duration) (time.Duration, error)
}

type WaitIDGenerator interface {
	NewWaitID() (WaitID, error)
}

type Config struct {
	WaitBudget          time.Duration
	AdditiveJitterLimit time.Duration
	VisibilityThreshold time.Duration
	Clock               Clock
	Timers              TimerFactory
	Jitter              JitterSource
	WaitIDs             WaitIDGenerator
	GenerationFence     GenerationFence
}

type WaitID [WaitIdentityBytes]byte

func (id WaitID) IsZero() bool { return id == WaitID{} }

func (id WaitID) Bytes() []byte {
	result := make([]byte, len(id))
	copy(result, id[:])
	return result
}

type Snapshot struct {
	ActiveWaiters   uint32
	ActiveSince     time.Time
	VisibleAfter    time.Time
	AccumulatedWait time.Duration
	Attempts        uint64
}

func (snapshot Snapshot) Visible(now time.Time) bool {
	return snapshot.ActiveWaiters != 0 && !snapshot.VisibleAfter.IsZero() && !now.Before(snapshot.VisibleAfter)
}

func (snapshot Snapshot) ActiveDuration(now time.Time) time.Duration {
	if snapshot.ActiveWaiters == 0 || snapshot.ActiveSince.IsZero() || now.Before(snapshot.ActiveSince) {
		return 0
	}
	return now.Sub(snapshot.ActiveSince)
}

type TraceStage uint8

const (
	TraceRetryScheduled TraceStage = iota + 1
	TraceRetryReady
	TraceRetrySucceeded
	TraceBudgetPaused
	TraceWaitCanceled
	TraceGenerationEnded
)

type Trace struct {
	Stage             TraceStage
	WaitID            WaitID
	ProtocolSession   protocolsession.ProtocolSessionID
	ProtocolOperation protocolsession.OperationID
	Generation        GenerationToken
	Attempt           uint64
	Hint              time.Duration
	Jitter            time.Duration
	Delay             time.Duration
	AccumulatedWait   time.Duration
	ActiveWaiters     uint32
	Cause             error
}

type Observer interface {
	ObserveRevisionWait(Snapshot)
}

type ObserverFunc func(Snapshot)

func (function ObserverFunc) ObserveRevisionWait(snapshot Snapshot) {
	if function != nil {
		function(snapshot)
	}
}

type Tracer interface {
	TraceRevisionWait(Trace)
}

type TraceFunc func(Trace)

func (function TraceFunc) TraceRevisionWait(event Trace) {
	if function != nil {
		function(event)
	}
}

type WaitOutcome uint8

const (
	WaitRetry WaitOutcome = iota + 1
	WaitBudgetPaused
	WaitCanceled
	WaitGenerationEnded
)

type Coordinator struct {
	budget              time.Duration
	jitterLimit         time.Duration
	visibilityThreshold time.Duration
	clock               Clock
	timers              TimerFactory
	jitter              JitterSource
	waitIDs             WaitIDGenerator
	fence               GenerationFence

	mu          sync.Mutex
	active      uint32
	activeSince time.Time
	accumulated time.Duration
	attempts    uint64
}

func NewCoordinator(config Config) (*Coordinator, error) {
	if config.WaitBudget == 0 {
		config.WaitBudget = DefaultWaitBudget
	}
	if config.AdditiveJitterLimit == 0 {
		config.AdditiveJitterLimit = DefaultAdditiveJitterLimit
	}
	if config.VisibilityThreshold == 0 {
		config.VisibilityThreshold = DefaultVisibilityThreshold
	}
	if config.Clock == nil {
		config.Clock = wallClock{}
	}
	if config.Timers == nil {
		config.Timers = wallTimerFactory{}
	}
	random := &lockedRandom{source: rand.Reader}
	if config.Jitter == nil {
		config.Jitter = random
	}
	if config.WaitIDs == nil {
		config.WaitIDs = random
	}
	if config.WaitBudget <= 0 || config.WaitBudget > MaximumWaitBudget ||
		config.AdditiveJitterLimit < 0 || config.AdditiveJitterLimit > MaximumAdditiveJitterLimit ||
		config.VisibilityThreshold < 0 || config.VisibilityThreshold > MaximumVisibilityThreshold ||
		config.GenerationFence == nil {
		return nil, ErrInvalidConfig
	}
	return &Coordinator{
		budget: config.WaitBudget, jitterLimit: config.AdditiveJitterLimit,
		visibilityThreshold: config.VisibilityThreshold, clock: config.Clock,
		timers: config.Timers, jitter: config.Jitter, waitIDs: config.WaitIDs,
		fence: config.GenerationFence,
	}, nil
}

func (coordinator *Coordinator) NewOperation(observer Observer, tracer Tracer) (*Operation, error) {
	if coordinator == nil {
		return nil, ErrInvalidConfig
	}
	id, err := coordinator.waitIDs.NewWaitID()
	if err != nil || id.IsZero() {
		return nil, errors.Join(ErrWaitIdentity, err)
	}
	return &Operation{coordinator: coordinator, id: id, observer: observer, tracer: tracer}, nil
}

func (coordinator *Coordinator) Snapshot() Snapshot {
	if coordinator == nil {
		return Snapshot{}
	}
	now := coordinator.clock.Now()
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	return coordinator.snapshotLocked(now)
}

func (coordinator *Coordinator) snapshotLocked(now time.Time) Snapshot {
	accumulated := coordinator.accumulated
	if coordinator.active != 0 && !coordinator.activeSince.IsZero() && now.After(coordinator.activeSince) {
		accumulated = saturatingDurationAdd(accumulated, now.Sub(coordinator.activeSince))
	}
	snapshot := Snapshot{
		ActiveWaiters: coordinator.active, ActiveSince: coordinator.activeSince,
		AccumulatedWait: accumulated, Attempts: coordinator.attempts,
	}
	if coordinator.active != 0 {
		snapshot.VisibleAfter = coordinator.activeSince.Add(coordinator.visibilityThreshold)
	}
	return snapshot
}

func (coordinator *Coordinator) activate() Snapshot {
	now := coordinator.clock.Now()
	coordinator.mu.Lock()
	if coordinator.active == 0 {
		coordinator.activeSince = now
	}
	coordinator.active++
	snapshot := coordinator.snapshotLocked(now)
	coordinator.mu.Unlock()
	return snapshot
}

func (coordinator *Coordinator) planWait() (Snapshot, time.Duration) {
	now := coordinator.clock.Now()
	coordinator.mu.Lock()
	coordinator.attempts++
	snapshot := coordinator.snapshotLocked(now)
	remaining := max(coordinator.budget-snapshot.AccumulatedWait, 0)
	coordinator.mu.Unlock()
	return snapshot, remaining
}

func (coordinator *Coordinator) finishActive() Snapshot {
	now := coordinator.clock.Now()
	coordinator.mu.Lock()
	if coordinator.active != 0 {
		coordinator.active--
		if coordinator.active == 0 {
			if !coordinator.activeSince.IsZero() && now.After(coordinator.activeSince) {
				coordinator.accumulated = saturatingDurationAdd(
					coordinator.accumulated, now.Sub(coordinator.activeSince),
				)
			}
			coordinator.activeSince = time.Time{}
		}
	}
	snapshot := coordinator.snapshotLocked(now)
	coordinator.mu.Unlock()
	return snapshot
}

type Operation struct {
	coordinator *Coordinator
	id          WaitID
	observer    Observer
	tracer      Tracer

	mu         sync.Mutex
	active     bool
	closed     bool
	generation GenerationToken
	attempt    uint64
	lastSignal *CapacitySignal
}

func (operation *Operation) ID() WaitID {
	if operation == nil {
		return WaitID{}
	}
	return operation.id
}

func (operation *Operation) Wait(ctx context.Context, signal *CapacitySignal) (WaitOutcome, error) {
	if operation == nil || operation.coordinator == nil || ctx == nil || !signal.valid() {
		return 0, ErrInvalidCapacitySignal
	}
	operation.mu.Lock()
	if operation.closed {
		operation.mu.Unlock()
		return 0, ErrWaitClosed
	}
	if operation.generation.IsZero() {
		operation.generation = signal.Generation()
	} else if operation.generation != signal.Generation() {
		operation.mu.Unlock()
		return operation.finish(TraceGenerationEnded, ErrGenerationChanged, WaitGenerationEnded)
	}
	operation.attempt++
	attempt := operation.attempt
	operation.lastSignal = signal
	activate := !operation.active
	operation.active = true
	operation.mu.Unlock()

	if activate {
		operation.observe(operation.coordinator.activate())
	}
	snapshot, remaining := operation.coordinator.planWait()
	operation.observe(snapshot)
	if remaining <= 0 {
		return operation.finish(TraceBudgetPaused, ErrWaitBudgetExhausted, WaitBudgetPaused)
	}
	jitter, err := operation.coordinator.jitter.AdditiveJitter(operation.coordinator.jitterLimit)
	if err != nil || jitter < 0 || jitter > operation.coordinator.jitterLimit {
		operation.Stop()
		return 0, errors.Join(ErrJitterSource, err)
	}
	delay := saturatingDurationAdd(signal.RetryAfter(), jitter)
	waitFor := delay
	budgetBound := waitFor >= remaining
	if budgetBound {
		waitFor = remaining
	}
	operation.trace(Trace{
		Stage: TraceRetryScheduled, WaitID: operation.id,
		ProtocolSession: signal.ProtocolSession(), ProtocolOperation: signal.ProtocolOperation(),
		Generation: signal.Generation(), Attempt: attempt, Hint: signal.RetryAfter(),
		Jitter: jitter, Delay: waitFor, AccumulatedWait: snapshot.AccumulatedWait,
		ActiveWaiters: snapshot.ActiveWaiters,
	})
	if waitFor <= 0 {
		return operation.finish(TraceBudgetPaused, ErrWaitBudgetExhausted, WaitBudgetPaused)
	}
	return operation.wait(ctx, signal, waitFor, budgetBound)
}

func (operation *Operation) wait(
	ctx context.Context,
	signal *CapacitySignal,
	delay time.Duration,
	budgetBound bool,
) (WaitOutcome, error) {
	timer := operation.coordinator.timers.NewTimer(delay)
	if timer == nil || timer.Done() == nil {
		operation.Stop()
		return 0, ErrTimerContract
	}
	fenceContext, cancelFence := context.WithCancel(ctx)
	type generationResult struct {
		change GenerationChange
		err    error
	}
	generationDone := make(chan generationResult, 1)
	go func() {
		change, err := operation.coordinator.fence.WaitForChange(fenceContext, signal.Generation())
		generationDone <- generationResult{change: change, err: err}
	}()

	select {
	case <-ctx.Done():
		_ = timer.Stop()
		cancelFence()
		<-generationDone
		return operation.finish(TraceWaitCanceled, context.Cause(ctx), WaitCanceled)
	case result := <-generationDone:
		_ = timer.Stop()
		cancelFence()
		if cause := context.Cause(ctx); cause != nil {
			return operation.finish(TraceWaitCanceled, cause, WaitCanceled)
		}
		cause := result.err
		if cause == nil {
			if !result.change.validFor(signal.Generation()) {
				cause = ErrGenerationContract
			} else {
				cause = result.change.Cause()
			}
		}
		return operation.finish(TraceGenerationEnded, cause, WaitGenerationEnded)
	case <-timer.Done():
		cancelFence()
		<-generationDone
		if cause := context.Cause(ctx); cause != nil {
			return operation.finish(TraceWaitCanceled, cause, WaitCanceled)
		}
		snapshot := operation.coordinator.Snapshot()
		operation.observe(snapshot)
		if budgetBound || snapshot.AccumulatedWait >= operation.coordinator.budget {
			return operation.finish(TraceBudgetPaused, ErrWaitBudgetExhausted, WaitBudgetPaused)
		}
		operation.traceFromLast(TraceRetryReady, nil, snapshot)
		return WaitRetry, nil
	}
}

func (operation *Operation) Succeed() {
	if operation == nil {
		return
	}
	_, _ = operation.finish(TraceRetrySucceeded, nil, 0)
}

func (operation *Operation) Cancel(cause error) {
	if operation == nil {
		return
	}
	if cause == nil {
		cause = context.Canceled
	}
	_, _ = operation.finish(TraceWaitCanceled, cause, WaitCanceled)
}

// Stop clears progress when retry authority is superseded by another result.
// It emits no terminal fact because that result owns the subsequent diagnosis.
func (operation *Operation) Stop() {
	if operation == nil {
		return
	}
	operation.mu.Lock()
	if operation.closed {
		operation.mu.Unlock()
		return
	}
	operation.closed = true
	active := operation.active
	operation.active = false
	operation.mu.Unlock()
	if active {
		operation.observe(operation.coordinator.finishActive())
	}
}

func (operation *Operation) finish(
	stage TraceStage,
	cause error,
	outcome WaitOutcome,
) (WaitOutcome, error) {
	operation.mu.Lock()
	if operation.closed {
		operation.mu.Unlock()
		return 0, ErrWaitClosed
	}
	operation.closed = true
	active := operation.active
	operation.active = false
	operation.mu.Unlock()
	snapshot := operation.coordinator.Snapshot()
	if active {
		snapshot = operation.coordinator.finishActive()
		operation.observe(snapshot)
	}
	operation.traceFromLast(stage, cause, snapshot)
	return outcome, cause
}

func (operation *Operation) traceFromLast(stage TraceStage, cause error, snapshot Snapshot) {
	operation.mu.Lock()
	signal := operation.lastSignal
	attempt := operation.attempt
	operation.mu.Unlock()
	event := Trace{
		Stage: stage, WaitID: operation.id, Attempt: attempt,
		AccumulatedWait: snapshot.AccumulatedWait, ActiveWaiters: snapshot.ActiveWaiters, Cause: cause,
	}
	if signal != nil {
		event.ProtocolSession = signal.ProtocolSession()
		event.ProtocolOperation = signal.ProtocolOperation()
		event.Generation = signal.Generation()
		event.Hint = signal.RetryAfter()
	}
	operation.trace(event)
}

func (operation *Operation) observe(snapshot Snapshot) {
	if operation.observer == nil {
		return
	}
	defer func() { _ = recover() }()
	operation.observer.ObserveRevisionWait(snapshot)
}

func (operation *Operation) trace(event Trace) {
	if operation.tracer == nil {
		return
	}
	defer func() { _ = recover() }()
	operation.tracer.TraceRevisionWait(event)
}

func saturatingDurationAdd(left, right time.Duration) time.Duration {
	if right > 0 && left > time.Duration(1<<63-1)-right {
		return time.Duration(1<<63 - 1)
	}
	return left + right
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

type wallTimer struct{ timer *time.Timer }

func (timer wallTimer) Done() <-chan time.Time { return timer.timer.C }
func (timer wallTimer) Stop() bool             { return timer.timer.Stop() }

type wallTimerFactory struct{}

func (wallTimerFactory) NewTimer(delay time.Duration) Timer {
	return wallTimer{timer: time.NewTimer(delay)}
}

type lockedRandom struct {
	mu     sync.Mutex
	source io.Reader
}

func (random *lockedRandom) NewWaitID() (WaitID, error) {
	random.mu.Lock()
	defer random.mu.Unlock()
	var id WaitID
	_, err := io.ReadFull(random.source, id[:])
	return id, err
}

func (random *lockedRandom) AdditiveJitter(limit time.Duration) (time.Duration, error) {
	if limit <= 0 {
		return 0, nil
	}
	random.mu.Lock()
	defer random.mu.Unlock()
	var encoded [8]byte
	if _, err := io.ReadFull(random.source, encoded[:]); err != nil {
		return 0, err
	}
	return time.Duration(binary.LittleEndian.Uint64(encoded[:]) % (uint64(limit) + 1)), nil
}
