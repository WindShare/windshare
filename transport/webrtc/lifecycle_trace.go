package webrtc

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

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

// LifecycleTraceFunc adapts a function to LifecycleTracer.
type LifecycleTraceFunc func(LifecycleTrace)

func (function LifecycleTraceFunc) TraceWebRTCLifecycle(event LifecycleTrace) {
	if function != nil {
		function(event)
	}
}

// ChannelOptions carries optional process observability without coupling the
// transport state machine to a logging framework.
type ChannelOptions struct {
	LifecycleTracer LifecycleTracer
}

var nextLifecycleChannelID atomic.Uint64

const lifecycleTraceQueueCapacity = 256

type lifecycleTraceDispatcher struct {
	tracer LifecycleTracer

	mu      sync.Mutex
	wake    *sync.Cond
	queue   []LifecycleTrace
	closing bool
	dropped uint64
	last    LifecycleTrace
}

func newLifecycleTraceDispatcher(tracer LifecycleTracer) *lifecycleTraceDispatcher {
	if tracer == nil {
		return nil
	}
	dispatcher := &lifecycleTraceDispatcher{tracer: tracer}
	dispatcher.wake = sync.NewCond(&dispatcher.mu)
	go dispatcher.run()
	return dispatcher
}

func (d *lifecycleTraceDispatcher) emit(event LifecycleTrace) {
	if d == nil {
		return
	}
	d.mu.Lock()
	if !d.closing {
		d.last = event
		if d.dropped > 0 && len(d.queue) < lifecycleTraceQueueCapacity {
			d.queue = append(d.queue, lifecycleTraceDropNotice(event, d.dropped))
			d.dropped = 0
		}
		if len(d.queue) < lifecycleTraceQueueCapacity {
			d.queue = append(d.queue, event)
		} else {
			// A blocked observer must not become transport backpressure. The next
			// available record reports the exact number of omitted trace events.
			d.dropped++
		}
		d.wake.Signal()
	}
	d.mu.Unlock()
}

func (d *lifecycleTraceDispatcher) shutdown() {
	if d == nil {
		return
	}
	d.mu.Lock()
	if d.dropped > 0 {
		d.queue = append(d.queue, lifecycleTraceDropNotice(d.last, d.dropped))
		d.dropped = 0
	}
	d.closing = true
	d.wake.Signal()
	d.mu.Unlock()
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
	for {
		d.mu.Lock()
		for len(d.queue) == 0 && !d.closing {
			d.wake.Wait()
		}
		if len(d.queue) == 0 {
			d.mu.Unlock()
			return
		}
		event := d.queue[0]
		d.queue[0] = LifecycleTrace{}
		d.queue = d.queue[1:]
		d.mu.Unlock()
		func() {
			// Observability must never acquire lifecycle ownership through a panic.
			defer func() { _ = recover() }()
			d.tracer.TraceWebRTCLifecycle(event)
		}()
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
	// Successful ordinary frames are the hot data path. Trace terminal ownership
	// and every refusal, while emitting accepted ordinary sends only on failure.
	if admission.state == sendAdmissionAccepted && admission.operation == sendOrdinary {
		return
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
	l.traces.emit(LifecycleTrace{
		ChannelID:   l.channelID,
		OperationID: admission.id,
		Operation:   lifecycleOperation(admission.operation),
		Transition:  transition,
		Disposition: disposition,
		State:       l.state,
		Terminal:    terminalTraceState(l.terminal),
		Cause:       lifecycleCause(admission.err),
	})
}

func (l *channelLifecycle) traceAcceptedTransportFailureLocked(admission *sendAdmission) {
	if l.traces == nil {
		return
	}
	l.traces.emit(LifecycleTrace{
		ChannelID:   l.channelID,
		OperationID: admission.id,
		Operation:   lifecycleOperation(admission.operation),
		Transition:  LifecycleTransitionSendAccepted,
		Disposition: framechannel.SendAccepted,
		State:       l.state,
		Terminal:    terminalTraceState(l.terminal),
		Cause:       LifecycleCauseTransport,
	})
}

func (l *channelLifecycle) traceChannelLocked(transition LifecycleTransition, reason error) {
	if l.traces == nil {
		return
	}
	l.traces.emit(LifecycleTrace{
		ChannelID:  l.channelID,
		Operation:  LifecycleOperationChannel,
		Transition: transition,
		State:      l.state,
		Terminal:   terminalTraceState(l.terminal),
		Cause:      lifecycleCause(reason),
	})
}
