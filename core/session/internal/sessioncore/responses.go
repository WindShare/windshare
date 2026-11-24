package sessioncore

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type ResponseConfig struct {
	TerminalObserver func(TerminalSendObservation)
}

type Responses struct {
	runtime          *Runtime
	sequence         atomic.Uint64
	terminalObserver func(TerminalSendObservation)
}

func NewResponses(runtime *Runtime, config ResponseConfig) *Responses {
	return &Responses{
		runtime:          runtime,
		terminalObserver: config.TerminalObserver,
	}
}

func (outbound *Responses) SendControl(
	ctx context.Context,
	op protocolsession.Operation,
	kind protocolsession.MessageKind,
	body []byte,
) error {
	id := op.ID()
	message, err := protocolsession.NewMessage(kind, &id, body)
	if err != nil {
		return outbound.failPreparation(op, kind, sessionobs.ProtocolErrorForResponse(kind, body), err)
	}
	return outbound.executeResponse(ctx, op, message, false, sessionobs.ProtocolErrorForResponse(kind, body))
}

func (outbound *Responses) SendFragment(
	ctx context.Context,
	op protocolsession.Operation,
	message protocolsession.Message,
) error {
	id, ok := message.OperationID()
	if !ok || id != op.ID() {
		return outbound.failPreparation(op, message.Kind(), sessionobs.ProtocolErrorContent{}, session.ErrOperationMissing)
	}
	return outbound.executeResponse(ctx, op, message, true, sessionobs.ProtocolErrorContent{})
}

func (outbound *Responses) SendOperationError(
	ctx context.Context,
	op protocolsession.Operation,
	failure contentflow.OperationFailure,
) error {
	body, err := protocolsession.EncodeOperationFailure(failure)
	if err != nil {
		return outbound.failPreparation(op, protocolsession.MessageOperationError, sessionobs.ProtocolErrorContent{}, err)
	}
	return outbound.SendControl(ctx, op, protocolsession.MessageOperationError, body)
}

func (outbound *Responses) SendTerminalAll(ctx context.Context, body []byte) error {
	return outbound.sendTerminalAll(ctx, body)
}

func (outbound *Responses) failPreparation(
	op protocolsession.Operation,
	kind protocolsession.MessageKind,
	content sessionobs.ProtocolErrorContent,
	cause error,
) error {
	runtime := outbound.runtime
	sequence := outbound.sequence.Add(1)
	requestKind, _ := op.RequestKind()
	if senderResponseFinal(kind) && !op.IsZero() {
		_, _ = runtime.operations.CancelOperation(op.ID())
	}
	outbound.traceResponseSend(op.ID(), requestKind, kind, sequence, session.LaneIdentity{}, sessionobs.SendStatusNotSent, 0, content)
	return protocolsession.MarkNotSent(cause)
}

func (outbound *Responses) executeResponse(
	ctx context.Context,
	op protocolsession.Operation,
	message protocolsession.Message,
	isData bool,
	content sessionobs.ProtocolErrorContent,
) error {
	runtime := outbound.runtime
	sequence := outbound.sequence.Add(1)
	kind := message.Kind()
	final := senderResponseFinal(kind)
	if op.IsZero() {
		outbound.traceResponseSend(0, 0, kind, sequence, session.LaneIdentity{}, sessionobs.SendStatusNotSent, 0, content)
		return protocolsession.MarkNotSent(session.ErrOperationMissing)
	}
	lease, err := runtime.operations.BeginOutboundResponse(op.ID(), message)
	if err != nil {
		requestKind, _ := op.RequestKind()
		if final && !errors.Is(err, protocolsession.ErrOperationSuppressed) {
			_, _ = runtime.operations.CancelOperation(op.ID())
		}
		outbound.traceResponseSend(op.ID(), requestKind, kind, sequence, session.LaneIdentity{}, sessionobs.SendStatusNotSent, 0, content)
		return err
	}

	outcome := outbound.tryResponseLanes(ctx, lease, message, isData)
	deliveredOrUncertain := outcome.delivered || outcome.uncertain
	abandon := !deliveredOrUncertain && (final || (ctx != nil && ctx.Err() != nil) || runtime.ctx.Err() != nil)
	runtime.operations.CompleteOutboundResponse(lease, message, final, deliveredOrUncertain, abandon)
	lease.releaseInFlight(deliveredOrUncertain)

	status := sessionobs.SendStatusNotSent
	var sendErr error
	switch {
	case outcome.delivered:
		status = sessionobs.SendStatusConfirmed
	case outcome.uncertain:
		status = sessionobs.SendStatusUncertain
		sendErr = outcome.err
	default:
		sendErr = protocolsession.MarkNotSent(outcome.err)
	}
	outbound.traceResponseSend(op.ID(), lease.requestKind, kind, sequence, outcome.lastLane, status, outcome.attempts, content)
	return sendErr
}

type responseDeliveryOutcome struct {
	lastLane  session.LaneIdentity
	attempts  uint32
	delivered bool
	uncertain bool
	err       error
}

func (outbound *Responses) tryResponseLanes(
	ctx context.Context,
	lease *outboundResponseLease,
	message protocolsession.Message,
	isData bool,
) responseDeliveryOutcome {
	runtime := outbound.runtime
	excluded := make(map[session.LaneIdentity]struct{}, protocolsession.DefaultMaxLogicalLanes)
	var outcome responseDeliveryOutcome

	for range protocolsession.DefaultMaxLogicalLanes {
		lane, laneErr := outbound.selectResponseLane(lease, excluded)
		if laneErr != nil {
			outcome.err = errors.Join(outcome.err, laneErr)
			return outcome
		}
		outcome.lastLane = lane.identity
		outcome.attempts++
		attemptPin := runtime.operations.pinInFlight(lease.record)
		var sendErr error
		if isData {
			sendErr = lane.writer.SendData(ctx, message, attemptPin)
		} else {
			sendErr = lane.writer.SendControl(ctx, message, attemptPin)
		}
		if sendErr == nil {
			outcome.delivered = true
			return outcome
		}
		outcome.err = errors.Join(outcome.err, sendErr)
		notSent := protocolsession.IsNotSent(sendErr)
		if !notSent {
			outcome.uncertain = true
		}
		if stopErr, stop := responseStopError(runtime, ctx, sendErr, notSent); stop {
			if stopErr != nil {
				outcome.err = errors.Join(outcome.err, stopErr)
			}
			return outcome
		}
		// Sender responses (fragments, finals, PeerAnswer, PeerCandidate) are
		// idempotent under receiver deduplication, so a failed or ambiguous lane
		// triggers re-sealing and failover to any surviving lane.
		if !errors.Is(sendErr, protocolsession.ErrControlQueueFull) &&
			!errors.Is(sendErr, protocolsession.ErrDataQueueFull) {
			runtime.lanes.markSelectedClosing(lane)
		}
		excluded[lane.identity] = struct{}{}
	}
	return outcome
}

func responseStopError(runtime *Runtime, ctx context.Context, sendErr error, notSent bool) (error, bool) {
	if runtime.ctx.Err() != nil {
		return errors.Join(session.ErrRuntimeClosed, runtime.Err()), true
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err(), true
	}
	if errors.Is(sendErr, protocolsession.ErrWriterTerminal) ||
		(notSent && !retryableLaneFailure(sendErr)) {
		return nil, true
	}
	return nil, false
}

func (outbound *Responses) selectResponseLane(
	lease *outboundResponseLease,
	excluded map[session.LaneIdentity]struct{},
) (selectedLane, error) {
	lanes := outbound.runtime.lanes
	preferred := lease.preferredLane
	if preferred.Valid(true) {
		if _, blocked := excluded[preferred]; !blocked {
			lane, err := lanes.selectLane(&preferred)
			if err == nil || !errors.Is(err, ErrLaneUnavailable) {
				return lane, err
			}
		}
	}
	lane, err := lanes.selectLaneExcluding(excluded)
	if err != nil {
		return selectedLane{}, err
	}
	lease.preferredLane = lane.identity
	outbound.runtime.operations.updatePreferredLane(lease.record, lane.identity)
	return lane, nil
}

func (outbound *Responses) traceResponseSend(
	operationID protocolsession.OperationID,
	requestKind, responseKind protocolsession.MessageKind,
	sequence uint64,
	lane session.LaneIdentity,
	status sessionobs.SendStatus,
	attempts uint32,
	content sessionobs.ProtocolErrorContent,
) {
	runtime := outbound.runtime
	if !runtime.protocolOperationTracingEnabled() {
		return
	}
	if attempts > 1 || status != sessionobs.SendStatusConfirmed ||
		!content.IsZero() || (requestKind != protocolsession.MessageRequestBlocks && senderResponseFinal(responseKind)) {
		ctx := runtime.observationContext(operationID, requestKind)
		runtime.protocolObservations.TryPublish(
			sessionobs.NewResponseSendObserved(ctx, sequence, responseKind, lane, status, attempts, content),
		)
	}
}
