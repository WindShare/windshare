package cli

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/humanoutput"
	"github.com/windshare/windshare/cmd/wind/internal/runtrace"
)

const defaultCommandEventCapacity = 256

var (
	errCommandRuntimeConfig = errors.New("command observation runtime configuration is invalid")
	errUserTraceOpen        = errors.New("user trace could not be opened")
)

type userTraceRecorder interface {
	Record(clievent.Event) bool
	ReportUpstreamLoss(lifecycle, progress uint64) bool
	Health() <-chan clievent.TraceIncomplete
	Close() runtrace.Status
	Path() string
}

type userTraceOpener func(
	target runtrace.Target,
	command clievent.Command,
	config runtrace.Config,
	dependencies runtrace.Dependencies,
) (userTraceRecorder, error)

type queuedCommandEvent struct {
	sequence uint64
	event    clievent.Event
}

type commandRuntime struct {
	command             clievent.Command
	clock               commandClock
	human               *humanoutput.Renderer
	trace               userTraceRecorder
	canvas              canvasHealth
	detailedDiagnostics bool

	ready   chan struct{}
	closing chan struct{}
	done    chan struct{}

	entryMu                sync.Mutex
	closed                 bool
	observerFinalized      bool
	entrySequence          uint64
	observerLifecycle      []queuedCommandEvent
	observerLifecycleHead  int
	observerLifecycleCount int
	observerProgress       queuedCommandEvent
	hasObserverProgress    bool
	commandPublications    []queuedCommandEvent
	commandPublicationHead int
	stagedFinalProgress    *clievent.TransferProgress
	stagedTerminal         clievent.TerminalEvent
	presentationTerminal   clievent.TerminalEvent

	pendingObserverLoss  [clievent.ObserverLossCategoryLimit][clievent.ObserverLossReasonLimit]atomic.Uint64
	upstreamCumulative   [clievent.ObserverLossCategoryLimit][clievent.ObserverLossReasonLimit]atomic.Uint64
	pendingProgressLoss  atomic.Uint64
	pendingTraceLoss     atomic.Uint64
	pendingTraceProgress atomic.Uint64
	warningOnce          sync.Once
	closeOnce            sync.Once
}

// canvasHealth is deliberately narrower than TerminalCanvas. Runtime users can
// observe presentation health but cannot turn a writer failure into authority.
type canvasHealth interface {
	Err() error
	FinishProgress()
}

func (a *App) newCommandRuntime(
	command clievent.Command,
	options observationOptions,
) (*commandRuntime, error) {
	if !command.Valid() || a.commandEventCapacity < 0 {
		return nil, errCommandRuntimeConfig
	}
	if err := options.validate(); err != nil {
		return nil, err
	}
	terminal := a.terminalOutput()
	renderer, err := humanoutput.New(humanoutput.Config{
		Canvas:       terminal.canvas,
		Capabilities: terminal.caps,
		Clock:        terminal.clock,
		CellWidth:    terminal.width,
		Verbose:      options.verbose,
	})
	if err != nil {
		return nil, errCommandRuntimeConfig
	}

	recorder, err := a.openCommandTrace(command, options, terminal, renderer)
	if err != nil {
		return nil, err
	}

	capacity := a.commandEventCapacity
	if capacity == 0 {
		capacity = defaultCommandEventCapacity
	}
	runtime := &commandRuntime{
		command: command, clock: terminal.clock, human: renderer, trace: recorder,
		detailedDiagnostics: options.verbose || options.traceEnabled(),
		canvas:              terminal.canvas, observerLifecycle: make([]queuedCommandEvent, capacity),
		ready: make(chan struct{}, 1), closing: make(chan struct{}), done: make(chan struct{}),
	}
	go runtime.run()
	return runtime, nil
}

func (a *App) openCommandTrace(
	command clievent.Command,
	options observationOptions,
	terminal *terminalOutputState,
	renderer *humanoutput.Renderer,
) (userTraceRecorder, error) {
	if !options.traceEnabled() {
		return nil, nil
	}
	opener := a.openUserTrace
	if opener == nil {
		opener = openNativeUserTrace
	}
	target, targetErr := options.traceTarget()
	if targetErr != nil {
		renderTraceOpenFailure(renderer, command, targetErr)
		return nil, errUserTraceOpen
	}
	recorder, err := opener(target, command, runtrace.Config{}, runtrace.Dependencies{
		Clock: terminal.clock,
		NewTicker: func(interval time.Duration) runtrace.Ticker {
			return terminal.clock.NewTicker(interval)
		},
	})
	if err == nil && recorder != nil {
		if options.traceDirectoryMode() {
			_ = renderer.RenderTracePath(recorder.Path())
		}
		return recorder, nil
	}
	if recorder != nil {
		_ = recorder.Close()
	}
	renderTraceOpenFailure(renderer, command, err)
	if errors.Is(err, runtrace.ErrTraceExists) {
		return nil, errors.Join(errUserTraceOpen, runtrace.ErrTraceExists)
	}
	return nil, errUserTraceOpen
}

func openNativeUserTrace(
	target runtrace.Target,
	command clievent.Command,
	config runtrace.Config,
	dependencies runtrace.Dependencies,
) (userTraceRecorder, error) {
	recorder, err := runtrace.OpenWithDependencies(target, command, config, dependencies)
	if err != nil {
		// Converting a nil *Recorder directly to the interface would create a
		// non-nil typed-nil cleanup target on startup failure.
		return nil, err
	}
	return recorder, nil
}

func renderTraceOpenFailure(renderer *humanoutput.Renderer, command clievent.Command, openErr error) {
	code := clievent.FailureTraceOpen
	if errors.Is(openErr, runtrace.ErrTraceExists) {
		code = clievent.FailureTraceExists
	}
	failure, err := clievent.NewFailure(code)
	if err != nil {
		return
	}
	event, err := clievent.NewCommandFailed(command, clievent.ExitFailure, failure)
	if err == nil {
		_ = renderer.RenderTerminal(event)
	}
}

// Observe is the bounded, nonblocking ingestion path for provider facts.
// Lifecycle overload is trace-visible loss; progress intentionally coalesces
// because intermediate presentation samples carry no command authority.
func (runtime *commandRuntime) Observe(event clievent.Event) bool {
	if runtime == nil {
		return false
	}
	runtime.entryMu.Lock()
	defer runtime.entryMu.Unlock()
	if runtime.closed {
		return false
	}
	if runtime.observerFinalized {
		if _, progress := event.(clievent.TransferProgress); progress {
			saturatingAtomicAdd(&runtime.pendingProgressLoss, 1)
		} else {
			runtime.addObserverLoss(clievent.ObserverLossCommandAdapter, clievent.ObserverLossRecorderClosed, 1)
		}
		runtime.signalReadyLocked()
		return false
	}
	if event == nil || event.Command() != runtime.command {
		runtime.addObserverLoss(clievent.ObserverLossCommandAdapter, clievent.ObserverLossEventContract, 1)
		runtime.signalReadyLocked()
		return false
	}
	if _, terminal := event.(clievent.TerminalEvent); terminal {
		runtime.addObserverLoss(clievent.ObserverLossCommandAdapter, clievent.ObserverLossEventContract, 1)
		runtime.signalReadyLocked()
		return false
	}
	_, progress := event.(clievent.TransferProgress)
	if runtime.entrySequence == ^uint64(0) {
		if progress {
			saturatingAtomicAdd(&runtime.pendingProgressLoss, 1)
		} else {
			runtime.addObserverLoss(clievent.ObserverLossCommandAdapter, clievent.ObserverLossAdapterCapacityTimeout, 1)
		}
		runtime.signalReadyLocked()
		return false
	}
	runtime.entrySequence++
	queued := queuedCommandEvent{sequence: runtime.entrySequence, event: event}
	if progress {
		if runtime.hasObserverProgress {
			saturatingAtomicAdd(&runtime.pendingProgressLoss, 1)
		}
		runtime.observerProgress = queued
		runtime.hasObserverProgress = true
		runtime.signalReadyLocked()
		return true
	}
	if runtime.observerLifecycleCount == len(runtime.observerLifecycle) {
		runtime.addObserverLoss(clievent.ObserverLossCommandAdapter, clievent.ObserverLossAdapterCapacityTimeout, 1)
		runtime.signalReadyLocked()
		return false
	}
	index := (runtime.observerLifecycleHead + runtime.observerLifecycleCount) % len(runtime.observerLifecycle)
	runtime.observerLifecycle[index] = queued
	runtime.observerLifecycleCount++
	runtime.signalReadyLocked()
	return true
}

// Publish retains command-authoritative milestones independently of observer
// capacity. Only command supervisors use this path, so retaining this small set
// in memory cannot put terminal or trace IO onto provider callback goroutines.
func (runtime *commandRuntime) Publish(events ...clievent.Event) bool {
	if runtime == nil || len(events) == 0 {
		return false
	}
	for _, event := range events {
		if _, terminal := event.(clievent.TerminalEvent); terminal {
			return false
		}
	}
	runtime.entryMu.Lock()
	defer runtime.entryMu.Unlock()
	return runtime.publishLocked(events...)
}

// Finalize closes observer ingestion and assigns exactly one terminal event its
// causal sequence. Its trace record remains ordered work; only human rendering
// is staged until trace sealing.
func (runtime *commandRuntime) Finalize(terminal clievent.TerminalEvent) bool {
	return runtime.finalize(nil, terminal)
}

func (runtime *commandRuntime) finalize(
	progress *clievent.TransferProgress,
	terminal clievent.TerminalEvent,
) bool {
	if runtime == nil || terminal == nil || terminal.Command() != runtime.command {
		return false
	}
	runtime.entryMu.Lock()
	defer runtime.entryMu.Unlock()
	if runtime.closed || runtime.observerFinalized || runtime.stagedTerminal != nil {
		return false
	}
	events := make([]clievent.Event, 0, 2)
	if progress != nil {
		if runtime.command != clievent.CommandGet || progress.Command() != runtime.command {
			return false
		}
		events = append(events, *progress)
	}
	events = append(events, terminal)
	loss := runtime.collectPendingLossLocked()
	if !runtime.publishLocked(events...) {
		return false
	}
	runtime.observerFinalized = true
	runtime.scheduleUpstreamLossLocked(loss)
	return true
}

// StageFinalization retains a decided terminal authority while product owners
// stop and complete their streams. It accepts no ordinary events by design.
func (runtime *commandRuntime) StageFinalization(terminal clievent.TerminalEvent) bool {
	if runtime == nil || terminal == nil || terminal.Command() != runtime.command {
		return false
	}
	runtime.entryMu.Lock()
	defer runtime.entryMu.Unlock()
	if runtime.closed || runtime.observerFinalized || runtime.stagedTerminal != nil {
		return false
	}
	runtime.stagedTerminal = terminal
	return true
}

func (runtime *commandRuntime) StageTransferFinalization(
	progress clievent.TransferProgress,
	terminal clievent.TransferSettled,
) bool {
	if runtime == nil || runtime.command != clievent.CommandGet {
		return false
	}
	runtime.entryMu.Lock()
	defer runtime.entryMu.Unlock()
	if runtime.closed || runtime.observerFinalized || runtime.stagedTerminal != nil {
		return false
	}
	runtime.stagedFinalProgress = &progress
	runtime.stagedTerminal = terminal
	return true
}

func (runtime *commandRuntime) FinalizeStaged() bool {
	if runtime == nil {
		return false
	}
	runtime.entryMu.Lock()
	if runtime.closed || runtime.observerFinalized || runtime.stagedTerminal == nil {
		runtime.entryMu.Unlock()
		return false
	}
	progress := runtime.stagedFinalProgress
	terminal := runtime.stagedTerminal
	runtime.stagedFinalProgress = nil
	runtime.stagedTerminal = nil
	loss := runtime.collectPendingLossLocked()
	events := make([]clievent.Event, 0, 2)
	if progress != nil {
		events = append(events, *progress)
	}
	events = append(events, terminal)
	if !runtime.publishLocked(events...) {
		runtime.stagedFinalProgress = progress
		runtime.stagedTerminal = terminal
		runtime.entryMu.Unlock()
		return false
	}
	runtime.observerFinalized = true
	runtime.scheduleUpstreamLossLocked(loss)
	runtime.entryMu.Unlock()
	return true
}

func (runtime *commandRuntime) publishLocked(events ...clievent.Event) bool {
	if runtime.closed || runtime.observerFinalized ||
		uint64(len(events)) > ^uint64(0)-runtime.entrySequence {
		return false
	}
	for _, event := range events {
		if event == nil || event.Command() != runtime.command {
			return false
		}
	}
	for _, event := range events {
		runtime.entrySequence++
		runtime.commandPublications = append(runtime.commandPublications, queuedCommandEvent{
			sequence: runtime.entrySequence,
			event:    event,
		})
	}
	runtime.signalReadyLocked()
	return true
}

// PublishTransferFinalization makes the frozen final progress and settlement
// adjacent command publications after the command has quiesced its producers.
func (runtime *commandRuntime) PublishTransferFinalization(
	progress clievent.TransferProgress,
	settlement clievent.TransferSettled,
) bool {
	if runtime == nil || runtime.command != clievent.CommandGet {
		return false
	}
	return runtime.finalize(&progress, settlement)
}

func (runtime *commandRuntime) signalReadyLocked() {
	select {
	case runtime.ready <- struct{}{}:
	default:
	}
}

func (runtime *commandRuntime) Clock() commandClock {
	if runtime == nil {
		return nil
	}
	return runtime.clock
}

func (runtime *commandRuntime) Close() {
	if runtime == nil {
		return
	}
	runtime.closeOnce.Do(func() {
		runtime.entryMu.Lock()
		runtime.closed = true
		close(runtime.closing)
		runtime.entryMu.Unlock()
	})
	<-runtime.done
}

func (runtime *commandRuntime) run() {
	defer close(runtime.done)
	health := runtime.traceHealth()
	for {
		if event := runtime.takeNext(); event != nil {
			runtime.dispatch(event)
			runtime.reportPendingLoss()
			continue
		}
		select {
		case <-runtime.ready:
			runtime.reportPendingLoss()
		case event, open := <-health:
			if !open {
				health = nil
				continue
			}
			runtime.warnTraceIncomplete(event)
		case <-runtime.closing:
			runtime.drain()
			return
		}
	}
}

func (runtime *commandRuntime) dispatch(event clievent.Event) {
	if terminal, ok := event.(clievent.TerminalEvent); ok {
		runtime.presentationTerminal = terminal
	} else {
		_ = runtime.human.Render(event)
	}
	if runtime.trace != nil {
		_ = runtime.trace.Record(event)
	}
}

func (runtime *commandRuntime) takeNext() clievent.Event {
	runtime.entryMu.Lock()
	defer runtime.entryMu.Unlock()

	const (
		noEntry uint8 = iota
		observerLifecycleEntry
		observerProgressEntry
		commandPublicationEntry
	)
	source := noEntry
	var earliest uint64
	selectEntry := func(candidate uint8, sequence uint64) {
		if source == noEntry || sequence < earliest {
			source = candidate
			earliest = sequence
		}
	}
	if runtime.observerLifecycleCount != 0 {
		selectEntry(
			observerLifecycleEntry,
			runtime.observerLifecycle[runtime.observerLifecycleHead].sequence,
		)
	}
	if runtime.hasObserverProgress {
		selectEntry(observerProgressEntry, runtime.observerProgress.sequence)
	}
	if runtime.commandPublicationHead < len(runtime.commandPublications) {
		selectEntry(
			commandPublicationEntry,
			runtime.commandPublications[runtime.commandPublicationHead].sequence,
		)
	}

	switch source {
	case observerLifecycleEntry:
		event := runtime.observerLifecycle[runtime.observerLifecycleHead].event
		runtime.observerLifecycle[runtime.observerLifecycleHead] = queuedCommandEvent{}
		runtime.observerLifecycleHead = (runtime.observerLifecycleHead + 1) % len(runtime.observerLifecycle)
		runtime.observerLifecycleCount--
		return event
	case observerProgressEntry:
		event := runtime.observerProgress.event
		runtime.observerProgress = queuedCommandEvent{}
		runtime.hasObserverProgress = false
		return event
	case commandPublicationEntry:
		event := runtime.commandPublications[runtime.commandPublicationHead].event
		runtime.commandPublications[runtime.commandPublicationHead] = queuedCommandEvent{}
		runtime.commandPublicationHead++
		if runtime.commandPublicationHead == len(runtime.commandPublications) {
			runtime.commandPublications = nil
			runtime.commandPublicationHead = 0
		}
		return event
	default:
		return nil
	}
}

func (runtime *commandRuntime) drain() {
	for {
		runtime.reportPendingLoss()
		event := runtime.takeNext()
		if event == nil {
			break
		}
		runtime.dispatch(event)
	}
	if runtime.trace != nil {
		status := runtime.trace.Close()
		runtime.drainTraceHealth()
		if !status.Complete {
			runtime.warnTraceIncomplete(traceIncompleteFromStatus(runtime.command, status))
		}
	}
	if runtime.presentationTerminal != nil {
		_ = runtime.human.RenderTerminal(runtime.presentationTerminal)
		return
	}
	runtime.canvas.FinishProgress()
}

// commandClock is shared by presentation sampling and user trace metadata for
// one command. Connectivity policy keeps its own clock because transport
// deadlines must not become presentation policy accidentally.
type commandClock interface {
	Now() time.Time
	NewTimer(time.Duration) commandTimer
	NewTicker(time.Duration) commandTicker
}

type commandTimer interface {
	C() <-chan time.Time
	Stop() bool
}

type commandTicker interface {
	C() <-chan time.Time
	Stop()
}

type systemCommandClock struct {
	now func() time.Time
}

func newSystemCommandClock(now func() time.Time) systemCommandClock {
	if now == nil {
		now = time.Now
	}
	return systemCommandClock{now: now}
}

func (clock systemCommandClock) Now() time.Time {
	return clock.now()
}

func (systemCommandClock) NewTimer(delay time.Duration) commandTimer {
	return systemCommandTimer{timer: time.NewTimer(delay)}
}

func (systemCommandClock) NewTicker(interval time.Duration) commandTicker {
	return systemCommandTicker{ticker: time.NewTicker(interval)}
}

type systemCommandTimer struct{ timer *time.Timer }

func (timer systemCommandTimer) C() <-chan time.Time { return timer.timer.C }
func (timer systemCommandTimer) Stop() bool          { return timer.timer.Stop() }

type systemCommandTicker struct{ ticker *time.Ticker }

func (ticker systemCommandTicker) C() <-chan time.Time { return ticker.ticker.C }
func (ticker systemCommandTicker) Stop()               { ticker.ticker.Stop() }
