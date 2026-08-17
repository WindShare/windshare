package webrtc

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/windshare/windshare/core/framechannel"
)

type LifecycleOperation string

const (
	LifecycleOperationChannel      LifecycleOperation = "channel"
	LifecycleOperationSend         LifecycleOperation = "send"
	LifecycleOperationSendTerminal LifecycleOperation = "send_terminal"
)

type LifecycleTransition string

const (
	LifecycleTransitionSendAccepted           LifecycleTransition = "send_accepted"
	LifecycleTransitionSendRejected           LifecycleTransition = "send_rejected"
	LifecycleTransitionSendRetired            LifecycleTransition = "send_retired"
	LifecycleTransitionRemoteTerminalReserved LifecycleTransition = "remote_terminal_reserved"
	LifecycleTransitionTerminationPending     LifecycleTransition = "termination_pending"
	LifecycleTransitionClosedClean            LifecycleTransition = "closed_clean"
	LifecycleTransitionClosedFailed           LifecycleTransition = "closed_failed"
	LifecycleTransitionTraceDropped           LifecycleTransition = "trace_dropped"
)

type LifecycleCause string

const (
	LifecycleCauseNone                   LifecycleCause = "none"
	LifecycleCauseCanceled               LifecycleCause = "canceled"
	LifecycleCauseDeadline               LifecycleCause = "deadline"
	LifecycleCauseNotOpen                LifecycleCause = "not_open"
	LifecycleCauseNaturalRetirement      LifecycleCause = "natural_retirement"
	LifecycleCauseRemoteClosed           LifecycleCause = "remote_closed"
	LifecycleCauseTerminalUnacknowledged LifecycleCause = "terminal_unacknowledged"
	LifecycleCausePeerProtocol           LifecycleCause = "peer_protocol"
	LifecycleCauseTransport              LifecycleCause = "transport"
	LifecycleCauseOther                  LifecycleCause = "other"
)

type LifecycleTerminalState string

const (
	LifecycleTerminalNone          LifecycleTerminalState = "none"
	LifecycleTerminalLocalPending  LifecycleTerminalState = "local_pending"
	LifecycleTerminalRemotePending LifecycleTerminalState = "remote_pending"
)

// LifecycleTrace is deliberately enum-like and excludes provider error text or
// frame content. ChannelID and OperationID remain stable for the channel lifetime
// so concurrent admission and closure decisions can be reconstructed safely.
type LifecycleTrace struct {
	ChannelID   uint64
	OperationID uint64
	Operation   LifecycleOperation
	Transition  LifecycleTransition
	Disposition framechannel.SendDisposition
	State       framechannel.ChannelState
	Terminal    LifecycleTerminalState
	Cause       LifecycleCause
	Dropped     uint64
}

// LifecycleTracer receives channel-ordered events asynchronously so observer
// latency cannot become transport latency.
type LifecycleTracer interface {
	TraceWebRTCLifecycle(LifecycleTrace)
}

type LifecycleContextTracer interface {
	LifecycleTracer
	TraceWebRTCLifecycleContext(context.Context, LifecycleTrace)
}

// LifecycleTraceFunc adapts a function to LifecycleTracer.
type LifecycleTraceFunc func(LifecycleTrace)

func (function LifecycleTraceFunc) TraceWebRTCLifecycle(event LifecycleTrace) {
	if function != nil {
		function(event)
	}
}

type LifecycleContextTraceFunc func(context.Context, LifecycleTrace)

func (function LifecycleContextTraceFunc) TraceWebRTCLifecycle(event LifecycleTrace) {
	function.TraceWebRTCLifecycleContext(context.Background(), event)
}

func (function LifecycleContextTraceFunc) TraceWebRTCLifecycleContext(ctx context.Context, event LifecycleTrace) {
	if function != nil {
		function(ctx, event)
	}
}

// ChannelOptions carries optional process observability without coupling the
// transport state machine to a logging framework.
type ChannelOptions struct {
	LifecycleTracer LifecycleTracer
}

var nextLifecycleChannelID atomic.Uint64

const (
	lifecycleTraceQueueCapacity = 256
	lifecycleCallbackLimit      = 25 * time.Millisecond
)

type lifecycleCallbackOutcome uint8

const (
	lifecycleCallbackDelivered lifecycleCallbackOutcome = iota + 1
	lifecycleCallbackPanicked
	lifecycleCallbackTimedOut
	lifecycleCallbackAbandoned
)

type lifecycleTraceDispatcher struct {
	tracer LifecycleTracer

	mu            sync.Mutex
	wake          *sync.Cond
	queue         []LifecycleTrace
	closing       bool
	detached      bool
	callbackLive  bool
	drained       bool
	delivered     uint64
	loss          LifecycleObservationLoss
	summarized    uint64
	last          LifecycleTrace
	callbackLimit time.Duration
	detach        chan struct{}
	detachOnce    sync.Once
	done          chan struct{}
}

func newLifecycleTraceDispatcher(tracer LifecycleTracer) *lifecycleTraceDispatcher {
	if tracer == nil {
		return nil
	}
	dispatcher := &lifecycleTraceDispatcher{
		tracer: tracer, drained: true, callbackLimit: lifecycleCallbackLimit,
		detach: make(chan struct{}), done: make(chan struct{}),
	}
	dispatcher.wake = sync.NewCond(&dispatcher.mu)
	go dispatcher.run()
	return dispatcher
}

func (d *lifecycleTraceDispatcher) emit(event LifecycleTrace) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closing || d.detached {
		d.loss.Undrained = saturatingLifecycleCount(d.loss.Undrained, 1)
		return
	}
	d.last = event
	d.flushDropSummaryLocked()
	if len(d.queue) < lifecycleTraceQueueCapacity {
		d.queue = append(d.queue, event)
	} else {
		d.loss.QueueOverflow = saturatingLifecycleCount(d.loss.QueueOverflow, 1)
	}
	d.wake.Signal()
}

func (d *lifecycleTraceDispatcher) shutdown() {
	if d == nil {
		return
	}
	d.mu.Lock()
	if !d.closing {
		d.closing = true
		d.flushDropSummaryLocked()
		d.wake.Broadcast()
	}
	d.mu.Unlock()
}

func (d *lifecycleTraceDispatcher) complete(ctx context.Context) LifecycleObservationCompletion {
	if d == nil {
		return LifecycleObservationCompletion{Drained: true}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	d.shutdown()
	select {
	case <-d.done:
	default:
		select {
		case <-d.done:
		case <-ctx.Done():
			d.mu.Lock()
			d.detachAtDeadlineLocked()
			d.mu.Unlock()
			<-d.done
		}
	}
	d.mu.Lock()
	completion := LifecycleObservationCompletion{Delivered: d.delivered, Loss: d.loss, Drained: d.drained}
	d.mu.Unlock()
	return completion
}

func (d *lifecycleTraceDispatcher) detachAtDeadlineLocked() {
	if d.detached {
		return
	}
	undrained := uint64(len(d.queue))
	if d.callbackLive {
		undrained = saturatingLifecycleCount(undrained, 1)
	}
	d.loss.Undrained = saturatingLifecycleCount(d.loss.Undrained, undrained)
	clear(d.queue)
	d.queue = nil
	d.detached = true
	d.drained = false
	d.detachOnce.Do(func() { close(d.detach) })
	d.wake.Broadcast()
}

func (d *lifecycleTraceDispatcher) flushDropSummaryLocked() {
	// Dropped is a queue-omission summary, while callback defects are reported
	// by the typed completion vector. Keeping those domains disjoint prevents a
	// panic or timeout from being relabeled and counted again as queue loss.
	dropped := d.loss.QueueOverflow
	if dropped == 0 || dropped == d.summarized || len(d.queue) >= lifecycleTraceQueueCapacity || d.detached {
		return
	}
	d.queue = append(d.queue, lifecycleTraceDropNotice(d.last, dropped))
	d.summarized = dropped
}

func lifecycleTraceDropNotice(last LifecycleTrace, dropped uint64) LifecycleTrace {
	return LifecycleTrace{
		ChannelID:  last.ChannelID,
		Operation:  LifecycleOperationChannel,
		Transition: LifecycleTransitionTraceDropped,
		State:      last.State,
		Terminal:   last.Terminal,
		Cause:      LifecycleCauseNone,
		Dropped:    dropped,
	}
}

func (d *lifecycleTraceDispatcher) run() {
	defer close(d.done)
	for {
		d.mu.Lock()
		d.flushDropSummaryLocked()
		for len(d.queue) == 0 && !d.closing && !d.detached {
			d.wake.Wait()
			d.flushDropSummaryLocked()
		}
		if d.detached || (len(d.queue) == 0 && d.closing) {
			d.mu.Unlock()
			return
		}
		event := d.queue[0]
		d.queue[0] = LifecycleTrace{}
		d.queue = d.queue[1:]
		d.callbackLive = true
		d.mu.Unlock()

		outcome := d.invoke(event)
		d.mu.Lock()
		d.callbackLive = false
		if d.detached || outcome == lifecycleCallbackAbandoned {
			d.mu.Unlock()
			return
		}
		switch outcome {
		case lifecycleCallbackDelivered:
			d.delivered = saturatingLifecycleCount(d.delivered, 1)
		case lifecycleCallbackPanicked:
			d.loss.ObserverPanic = saturatingLifecycleCount(d.loss.ObserverPanic, 1)
		case lifecycleCallbackTimedOut:
			d.loss.CallbackTimeout = saturatingLifecycleCount(d.loss.CallbackTimeout, 1)
			d.loss.Undrained = saturatingLifecycleCount(d.loss.Undrained, uint64(len(d.queue)))
			clear(d.queue)
			d.queue = nil
			d.detached = true
			d.drained = false
			d.detachOnce.Do(func() { close(d.detach) })
		}
		d.flushDropSummaryLocked()
		d.mu.Unlock()
		if outcome == lifecycleCallbackTimedOut {
			return
		}
	}
}

func (d *lifecycleTraceDispatcher) invoke(event LifecycleTrace) lifecycleCallbackOutcome {
	result := make(chan lifecycleCallbackOutcome, 1)
	callbackContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		outcome := lifecycleCallbackDelivered
		defer func() {
			if recover() != nil {
				outcome = lifecycleCallbackPanicked
			}
			result <- outcome
		}()
		if contextual, ok := d.tracer.(LifecycleContextTracer); ok {
			contextual.TraceWebRTCLifecycleContext(callbackContext, event)
			return
		}
		d.tracer.TraceWebRTCLifecycle(event)
	}()
	timer := time.NewTimer(d.callbackLimit)
	defer timer.Stop()
	select {
	case outcome := <-result:
		return outcome
	case <-timer.C:
		cancel()
		return lifecycleCallbackTimedOut
	case <-d.detach:
		cancel()
		return lifecycleCallbackAbandoned
	}
}

func lifecycleOperation(operation sendOperation) LifecycleOperation {
	if operation == sendTerminal {
		return LifecycleOperationSendTerminal
	}
	return LifecycleOperationSend
}

func lifecycleCause(err error) LifecycleCause {
	switch {
	case err == nil:
		return LifecycleCauseNone
	case errors.Is(err, context.Canceled):
		return LifecycleCauseCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return LifecycleCauseDeadline
	case errors.Is(err, ErrChannelNotOpen):
		return LifecycleCauseNotOpen
	case errors.Is(err, ErrTerminalNotAcknowledged):
		return LifecycleCauseTerminalUnacknowledged
	case errors.Is(err, ErrPeerProtocol):
		return LifecycleCausePeerProtocol
	case errors.Is(err, ErrTransport):
		return LifecycleCauseTransport
	case errors.Is(err, ErrRemoteClosed):
		return LifecycleCauseRemoteClosed
	case framechannel.SendDispositionOf(err) == framechannel.SendRetired:
		return LifecycleCauseNaturalRetirement
	default:
		return LifecycleCauseOther
	}
}

func terminalTraceState(state terminalState) LifecycleTerminalState {
	switch state {
	case terminalLocalPending:
		return LifecycleTerminalLocalPending
	case terminalRemotePending:
		return LifecycleTerminalRemotePending
	default:
		return LifecycleTerminalNone
	}
}

func (l *channelLifecycle) configureTrace(channelID uint64, dispatcher *lifecycleTraceDispatcher) {
	l.channelID = channelID
	l.traces = dispatcher
}

func (l *channelLifecycle) traceSendResolutionLocked(admission *sendAdmission) {
	if l.traces == nil {
		return
	}
	event, ok := sendResolutionLifecycleTrace(
		l.channelID, admission, l.state, terminalTraceState(l.terminal),
	)
	if ok {
		l.traces.emit(event)
	}
}

func sendResolutionLifecycleTrace(
	channelID uint64,
	admission *sendAdmission,
	state framechannel.ChannelState,
	terminal LifecycleTerminalState,
) (LifecycleTrace, bool) {
	// Successful ordinary frames are the hot data path. Trace terminal ownership
	// and every refusal, while emitting accepted ordinary sends only on failure.
	if admission.state == sendAdmissionAccepted && admission.operation == sendOrdinary {
		return LifecycleTrace{}, false
	}
	disposition := framechannel.SendAccepted
	transition := LifecycleTransitionSendAccepted
	if admission.state == sendAdmissionRefused {
		disposition = framechannel.SendDispositionOf(admission.err)
		if disposition == framechannel.SendRetired {
			transition = LifecycleTransitionSendRetired
		} else {
			transition = LifecycleTransitionSendRejected
		}
	}
	return LifecycleTrace{
		ChannelID:   channelID,
		OperationID: admission.id,
		Operation:   lifecycleOperation(admission.operation),
		Transition:  transition,
		Disposition: disposition,
		State:       state,
		Terminal:    terminal,
		Cause:       lifecycleCause(admission.err),
	}, true
}

func (l *channelLifecycle) traceAcceptedTransportFailureLocked(admission *sendAdmission) {
	if l.traces == nil {
		return
	}
	l.traces.emit(acceptedTransportFailureLifecycleTrace(
		l.channelID, admission, l.state, terminalTraceState(l.terminal),
	))
}

func acceptedTransportFailureLifecycleTrace(
	channelID uint64,
	admission *sendAdmission,
	state framechannel.ChannelState,
	terminal LifecycleTerminalState,
) LifecycleTrace {
	return LifecycleTrace{
		ChannelID: channelID, OperationID: admission.id,
		Operation:   lifecycleOperation(admission.operation),
		Transition:  LifecycleTransitionSendAccepted,
		Disposition: framechannel.SendAccepted,
		State:       state, Terminal: terminal, Cause: LifecycleCauseTransport,
	}
}

func (l *channelLifecycle) traceChannelLocked(transition LifecycleTransition, reason error) {
	if l.traces == nil {
		return
	}
	l.traces.emit(channelLifecycleTrace(
		l.channelID,
		transition,
		l.state,
		terminalTraceState(l.terminal),
		reason,
	))
}

func channelLifecycleTrace(
	channelID uint64,
	transition LifecycleTransition,
	state framechannel.ChannelState,
	terminal LifecycleTerminalState,
	reason error,
) LifecycleTrace {
	if transition == LifecycleTransitionRemoteTerminalReserved {
		// Reservation is the semantic transition being recorded; the internal
		// terminal state advances only when the ordered inbound owner accepts it.
		terminal = LifecycleTerminalRemotePending
	}
	return LifecycleTrace{
		ChannelID:  channelID,
		Operation:  LifecycleOperationChannel,
		Transition: transition,
		State:      state,
		Terminal:   terminal,
		Cause:      lifecycleCause(reason),
	}
}
