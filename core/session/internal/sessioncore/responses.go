package sessioncore

import (
	"context"
	"errors"
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
	"sync/atomic"
)

func (outbound *Responses) controlBinding(lane session.LaneIdentity) protocolsession.ControlBinding {
	runtime := outbound.runtime
	return protocolsession.ControlBinding{
		ShareInstance: runtime.share, ProtocolSessionID: runtime.sessionID,
		LaneID: lane.ID, LaneEpoch: lane.Epoch,
		Direction: protocolsession.DirectionSenderToReceiver,
	}
}

// SenderControlPreparer supplies signing authority without transferring key
// ownership to the shared runtime. The writer still chooses the sealed sequence.
type SenderControlPreparer interface {
	PrepareSenderControl(protocolsession.ControlBinding, protocolsession.MessageKind, *protocolsession.OperationID, []byte) (protocolsession.PreparedSenderControl, error)
}

type ResponseConfig struct {
	ControlPreparer  SenderControlPreparer
	Settlements      observationstream.Producer[protocolsession.SendAttemptSettlement]
	TerminalObserver func(TerminalSendObservation)
}

type Responses struct {
	runtime          *Runtime
	controlPreparer  SenderControlPreparer
	sequence         atomic.Uint64
	settlements      observationstream.Producer[protocolsession.SendAttemptSettlement]
	terminalObserver func(TerminalSendObservation)
}

func NewResponses(runtime *Runtime, config ResponseConfig) *Responses {
	return &Responses{runtime: runtime, controlPreparer: config.ControlPreparer,
		settlements: config.Settlements, terminalObserver: config.TerminalObserver}
}

func (outbound *Responses) prepareControl(binding protocolsession.ControlBinding, kind protocolsession.MessageKind, id *protocolsession.OperationID, body []byte) (protocolsession.PreparedSenderControl, error) {
	if outbound.controlPreparer == nil {
		return protocolsession.PreparedSenderControl{}, protocolsession.ErrControlSigningKey
	}
	return outbound.controlPreparer.PrepareSenderControl(binding, kind, id, body)
}

func (outbound *Responses) sendControl(
	ctx context.Context,
	kind protocolsession.MessageKind,
	operationID protocolsession.OperationID,
	body []byte,
) (protocolsession.ResponseSendResult, error) {
	return outbound.executeResponse(ctx, kind, operationID, sessionobs.ProtocolErrorForResponse(kind, body), func(transaction *outboundTransaction) (outboundLaneAttempt, error) {
		prepared, err := outbound.prepareControl(outbound.controlBinding(transaction.lane.identity), kind, &operationID, body)
		if err != nil {
			return nil, err
		}
		initial := transaction.lane.identity
		return func(lane selectedLane, permit protocolsession.OutboundReplayPermit) (protocolsession.SendReceipt, error) {
			control := prepared
			if lane.identity != initial {
				var prepareErr error
				control, prepareErr = outbound.prepareControl(outbound.controlBinding(lane.identity), kind, &operationID, body)
				if prepareErr != nil {
					return protocolsession.SendReceipt{}, prepareErr
				}
			}
			if !permit.IsZero() {
				return lane.writer.TrySenderControlReplay(control, permit)
			}
			return lane.writer.TryAuthorizedSenderControl(control, transaction.authority)
		}, nil
	})
}

// Preparation establishes immutable message inputs before the first writer
// attempt, so malformed input never becomes a fabricated receipt.
type responsePreparation func(*outboundTransaction) (outboundLaneAttempt, error)

func (outbound *Responses) executeResponse(ctx context.Context, kind protocolsession.MessageKind, operationID protocolsession.OperationID, content sessionobs.ProtocolErrorContent, prepare responsePreparation) (protocolsession.ResponseSendResult, error) {
	runtime := outbound.runtime
	sequence := outbound.sequence.Add(1)
	requestKind := protocolsession.MessageKind(0)
	if route, err := outboundRoute(ctx, operationID); err == nil {
		requestKind = route.requestKind
	}
	transaction, err := beginOutboundTransaction(outbound, ctx, operationID)
	final := senderResponseFinal(kind)
	if err != nil {
		end := protocolsession.ResponseSendEndPreparationFailed
		if errors.Is(err, ErrLaneUnavailable) {
			end = protocolsession.ResponseSendEndRouteUnavailable
		} else if errors.Is(err, session.ErrOperationMissing) {
			end = protocolsession.ResponseSendEndAuthorityUnavailable
		}
		result, _ := protocolsession.NewResponseSendNotStarted(end)
		if final {
			cleanupErr := runtime.abandonBoundOutboundOperation(ctx, operationID)
			result = withOutboundCleanup(result, cleanupErr)
			err = errors.Join(err, cleanupErr)
		}
		outbound.traceResult(operationID, requestKind, kind, sequence, content, result)
		return result, err
	}
	transaction.responseSequence = sequence
	transaction.responseKind = kind
	attempt, prepareErr := prepare(transaction)
	var result protocolsession.ResponseSendResult
	if prepareErr != nil {
		result, _ = protocolsession.NewResponseSendNotStarted(protocolsession.ResponseSendEndPreparationFailed)
		err = prepareErr
	} else {
		result, err = transaction.Run(ctx, attempt)
	}
	// Resource retirement is independent of physical evidence. A cleanup error
	// cannot turn transport confirmation into proof that a peer owns no resource.
	if final && err == nil && result.Evidence() == protocolsession.ResponseSendEvidenceTransportConfirmed {
		runtime.routes.releaseRoute(operationID, transaction.route)
		result = result.WithCleanup(protocolsession.SendCleanupRouteReleased)
	} else if final || ctx.Err() != nil || runtime.ctx.Err() != nil {
		cleanupErr := runtime.abandonOutboundOperation(operationID, transaction.route, transaction.generation)
		result = withOutboundCleanup(result, cleanupErr)
		err = errors.Join(err, cleanupErr)
	}
	transaction.Close()
	outbound.traceResult(operationID, transaction.route.requestKind, kind, sequence, content, result)
	return result, err
}

func withOutboundCleanup(result protocolsession.ResponseSendResult, err error) protocolsession.ResponseSendResult {
	if err != nil {
		return result.WithCleanup(protocolsession.SendCleanupFailed)
	}
	return result.WithCleanup(protocolsession.SendCleanupOperationRetired)
}

func (outbound *Responses) SendControl(ctx context.Context, kind protocolsession.MessageKind, operationID protocolsession.OperationID, body []byte) (protocolsession.ResponseSendResult, error) {
	return outbound.sendControl(ctx, kind, operationID, body)
}

func (outbound *Responses) SendFragment(ctx context.Context, message protocolsession.Message) error {
	operationID, ok := message.OperationID()
	if !ok {
		return session.ErrOperationMissing
	}
	_, err := outbound.executeResponse(ctx, message.Kind(), operationID, sessionobs.ProtocolErrorContent{}, func(transaction *outboundTransaction) (outboundLaneAttempt, error) {
		return func(lane selectedLane, permit protocolsession.OutboundReplayPermit) (protocolsession.SendReceipt, error) {
			if !permit.IsZero() {
				return lane.writer.TryDataReplay(message, permit)
			}
			return lane.writer.TryAuthorizedData(message, transaction.authority)
		}, nil
	})
	return err
}

func (outbound *Responses) SendOperationError(
	ctx context.Context,
	operationID protocolsession.OperationID,
	failure contentflow.OperationFailure,
) error {
	body, err := protocolsession.EncodeOperationFailure(failure)
	if err != nil {
		_, sendErr := outbound.executeResponse(ctx, protocolsession.MessageOperationError, operationID, sessionobs.ProtocolErrorContent{}, func(*outboundTransaction) (outboundLaneAttempt, error) { return nil, err })
		return sendErr
	}
	_, err = outbound.SendControl(ctx, protocolsession.MessageOperationError, operationID, body)
	return err
}

func (outbound *Responses) traceResult(operationID protocolsession.OperationID, requestKind, responseKind protocolsession.MessageKind, sequence uint64, content sessionobs.ProtocolErrorContent, result protocolsession.ResponseSendResult) {
	runtime := outbound.runtime
	if !runtime.protocolOperationTracingEnabled() {
		return
	}
	ctx := runtime.observationContext(operationID, requestKind)
	if !result.Started() && !result.IsZero() {
		runtime.protocolObservations.TryPublish(sessionobs.NewResponseSendNotStarted(ctx, sequence, responseKind, content, result))
		return
	}
	if result.AttemptCount() > 1 || result.Evidence() != protocolsession.ResponseSendEvidenceTransportConfirmed ||
		result.Cleanup() == protocolsession.SendCleanupFailed || result.End() != protocolsession.ResponseSendEndTransportConfirmed ||
		!content.IsZero() || (requestKind != protocolsession.MessageRequestBlocks && senderResponseFinal(responseKind)) {
		runtime.protocolObservations.TryPublish(sessionobs.NewResponseSendReturned(ctx, sequence, responseKind, content, result))
	}
}
