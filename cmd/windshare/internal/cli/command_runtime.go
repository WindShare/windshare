package cli

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
	"github.com/windshare/windshare/cmd/windshare/internal/humanoutput"
	"github.com/windshare/windshare/cmd/windshare/internal/runtrace"
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
}

type userTraceOpener func(
	path string,
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
	protocolDiagnostics bool

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

	pendingLifecycleLoss atomic.Uint64
	pendingProgressLoss  atomic.Uint64
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

	var recorder userTraceRecorder
	if options.traceEnabled() {
		opener := a.openUserTrace
		if opener == nil {
			opener = openNativeUserTrace
		}
		recorder, err = opener(options.tracePath, command, runtrace.Config{}, runtrace.Dependencies{
			Clock: terminal.clock,
			NewTicker: func(interval time.Duration) runtrace.Ticker {
				return terminal.clock.NewTicker(interval)
			},
		})
		if err != nil || recorder == nil {
			if recorder != nil {
				_ = recorder.Close()
			}
			renderTraceOpenFailure(renderer, command)
			return nil, errUserTraceOpen
		}
	}

	capacity := a.commandEventCapacity
	if capacity == 0 {
		capacity = defaultCommandEventCapacity
	}
	runtime := &commandRuntime{
		command: command, clock: terminal.clock, human: renderer, trace: recorder,
		protocolDiagnostics: options.verbose || options.traceEnabled(),
		canvas:              terminal.canvas, observerLifecycle: make([]queuedCommandEvent, capacity),
		ready: make(chan struct{}, 1), closing: make(chan struct{}), done: make(chan struct{}),
	}
	go runtime.run()
	return runtime, nil
}

func (runtime *commandRuntime) protocolDiagnosticsEnabled() bool {
	return runtime != nil && runtime.protocolDiagnostics
}

func openNativeUserTrace(
	path string,
	command clievent.Command,
	config runtrace.Config,
	dependencies runtrace.Dependencies,
) (userTraceRecorder, error) {
	return runtrace.OpenWithDependencies(path, command, config, dependencies)
}

func renderTraceOpenFailure(renderer *humanoutput.Renderer, command clievent.Command) {
	failure, err := clievent.NewFailure(clievent.FailureTraceOpen)
	if err != nil {
		return
	}
	event, err := clievent.NewCommandFailed(command, clievent.ExitFailure, failure)
	if err == nil {
		_ = renderer.Render(event)
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
			saturatingAtomicAdd(&runtime.pendingLifecycleLoss, 1)
		}
		runtime.signalReadyLocked()
		return false
	}
	if event == nil || event.Command() != runtime.command {
		saturatingAtomicAdd(&runtime.pendingLifecycleLoss, 1)
		runtime.signalReadyLocked()
		return false
	}
	_, progress := event.(clievent.TransferProgress)
	if runtime.entrySequence == ^uint64(0) {
		if progress {
			saturatingAtomicAdd(&runtime.pendingProgressLoss, 1)
		} else {
			saturatingAtomicAdd(&runtime.pendingLifecycleLoss, 1)
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
		saturatingAtomicAdd(&runtime.pendingLifecycleLoss, 1)
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
	runtime.entryMu.Lock()
	defer runtime.entryMu.Unlock()
	return runtime.publishLocked(events...)
}

// Finalize closes observer ingestion and retains the terminal command events at
// one sequence cut. Quiescing producers first keeps a rejected late fact from
// being the normal shutdown path while this guard prevents terminal reordering.
func (runtime *commandRuntime) Finalize(events ...clievent.Event) bool {
	if runtime == nil || len(events) == 0 {
		return false
	}
	runtime.entryMu.Lock()
	defer runtime.entryMu.Unlock()
	if runtime.observerFinalized || !runtime.publishLocked(events...) {
		return false
	}
	runtime.observerFinalized = true
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
	return runtime.Finalize(progress, settlement)
}

// ReportObserverLoss accounts for facts dropped by a bounded producer adapter
// before they could be offered to Observe. Recorder-local Record failures must not
// be reported here because runtrace already owns those counters.
func (runtime *commandRuntime) ReportObserverLoss(lifecycle, progress uint64) bool {
	if runtime == nil {
		return false
	}
	runtime.entryMu.Lock()
	defer runtime.entryMu.Unlock()
	if runtime.closed {
		return false
	}
	saturatingAtomicAdd(&runtime.pendingLifecycleLoss, lifecycle)
	saturatingAtomicAdd(&runtime.pendingProgressLoss, progress)
	runtime.signalReadyLocked()
	return true
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

func (runtime *commandRuntime) HumanOutputError() error {
	if runtime == nil || runtime.canvas == nil {
		return nil
	}
	return runtime.canvas.Err()
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

func (runtime *commandRuntime) traceHealth() <-chan clievent.TraceIncomplete {
	if runtime.trace == nil {
		return nil
	}
	return runtime.trace.Health()
}

func (runtime *commandRuntime) dispatch(event clievent.Event) {
	_ = runtime.human.Render(event)
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
	for event := runtime.takeNext(); event != nil; event = runtime.takeNext() {
		runtime.dispatch(event)
	}
	runtime.reportPendingLoss()
	if runtime.trace != nil {
		status := runtime.trace.Close()
		runtime.drainTraceHealth()
		if !status.Complete {
			runtime.warnTraceIncomplete(traceIncompleteFromStatus(runtime.command, status))
		}
	}
	runtime.canvas.FinishProgress()
}

func (runtime *commandRuntime) reportPendingLoss() {
	lifecycle := runtime.pendingLifecycleLoss.Swap(0)
	progress := runtime.pendingProgressLoss.Swap(0)
	if runtime.trace != nil && (lifecycle != 0 || progress != 0) {
		_ = runtime.trace.ReportUpstreamLoss(lifecycle, progress)
	}
}

func (runtime *commandRuntime) drainTraceHealth() {
	health := runtime.trace.Health()
	for {
		select {
		case event, open := <-health:
			if !open {
				return
			}
			runtime.warnTraceIncomplete(event)
		default:
			return
		}
	}
}

func (runtime *commandRuntime) warnTraceIncomplete(event clievent.TraceIncomplete) {
	runtime.warningOnce.Do(func() {
		_ = runtime.human.Render(event)
	})
}

func traceIncompleteFromStatus(command clievent.Command, status runtrace.Status) clievent.TraceIncomplete {
	cause := clievent.TraceIncompleteLifecycleDrop
	switch {
	case status.WriterFailed:
		cause = clievent.TraceIncompleteWriter
	case status.FlushFailed:
		cause = clievent.TraceIncompleteFlush
	case status.SchemaLimited:
		cause = clievent.TraceIncompleteSchemaLimit
	case status.LifecycleDropped == 0:
		cause = clievent.TraceIncompleteWriter
	}
	event, err := clievent.NewTraceIncomplete(
		command, cause, status.LifecycleDropped, status.ProgressDropped,
	)
	if err == nil {
		return event
	}
	// The fallback is constructible for every valid command and keeps an
	// inconsistent recorder status from leaking provider text into stderr.
	event, _ = clievent.NewTraceIncomplete(command, clievent.TraceIncompleteWriter, 0, 0)
	return event
}

func saturatingAtomicAdd(counter *atomic.Uint64, amount uint64) {
	if amount == 0 {
		return
	}
	for {
		current := counter.Load()
		next := current + amount
		if next < current {
			next = ^uint64(0)
		}
		if counter.CompareAndSwap(current, next) {
			return
		}
	}
}
