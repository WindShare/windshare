package webrtc

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/observationstream"
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

// DefaultLifecycleObservationCapacity retains a bounded prefix until the owner
// attaches a consumer. Callers must opt in by assigning it explicitly.
const DefaultLifecycleObservationCapacity = 256

// ChannelOptions carries optional process observability without allowing
// consumer execution to become transport work. Zero capacity disables it.
type ChannelOptions struct {
	LifecycleObservationCapacity int
}

var nextLifecycleChannelID atomic.Uint64

type lifecycleTraceSource struct {
	mu       sync.Mutex
	producer observationstream.Producer[LifecycleTrace]
	consumer observationstream.Consumer[LifecycleTrace]

	completed       bool
	completion      LifecycleObservationCompletion
	capacityDropped uint64
	summarized      uint64
}

func newLifecycleTraceSource(
	capacity int,
) (*lifecycleTraceSource, observationstream.Consumer[LifecycleTrace], error) {
	if capacity == 0 {
		return nil, nil, nil
	}
	producer, consumer, err := observationstream.New[LifecycleTrace](
		observationstream.Capacity(capacity),
	)
	if err != nil {
		return nil, nil, err
	}
	return &lifecycleTraceSource{producer: producer, consumer: consumer}, consumer, nil
}

func (source *lifecycleTraceSource) emit(event LifecycleTrace) {
	if source == nil {
		return
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.completed {
		return
	}

	source.flushDropSummaryLocked(event)
	if !source.producer.TryPublish(event) {
		source.capacityDropped = saturatingLifecycleCount(source.capacityDropped, 1)
		return
	}
}

func (source *lifecycleTraceSource) complete() LifecycleObservationCompletion {
	if source == nil {
		return LifecycleObservationCompletion{}
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.completed {
		return source.completion
	}

	cut := source.producer.Complete()
	source.completed = true
	source.completion = LifecycleObservationCompletion{
		Enqueued: cut.Enqueued,
		Loss: LifecycleObservationLoss{
			CapacityDropped: cut.CapacityDropped,
		},
	}
	return source.completion
}

func (source *lifecycleTraceSource) flushDropSummaryLocked(next LifecycleTrace) {
	if source.capacityDropped == 0 || source.capacityDropped == source.summarized {
		return
	}
	// A skipped notice must not enter TryPublish: that counted path is reserved
	// for real lifecycle facts. With one producer, observed room cannot disappear.
	if len(source.consumer) >= cap(source.consumer) {
		return
	}
	dropped := source.capacityDropped
	if source.producer.TryPublish(lifecycleTraceDropNotice(next, dropped)) {
		source.summarized = dropped
	}
}

func lifecycleTraceDropNotice(next LifecycleTrace, dropped uint64) LifecycleTrace {
	return LifecycleTrace{
		ChannelID:  next.ChannelID,
		Operation:  LifecycleOperationChannel,
		Transition: LifecycleTransitionTraceDropped,
		State:      next.State,
		Terminal:   next.Terminal,
		Cause:      LifecycleCauseNone,
		Dropped:    dropped,
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

func (l *channelLifecycle) configureTrace(channelID uint64, source *lifecycleTraceSource) {
	l.channelID = channelID
	l.traces = source
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
