package sessioncore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/requestlane"
)

const operationResponseFrames = 512

var (
	ErrOperationIDExhausted     = errors.New("session runtime operation identity space is exhausted")
	ErrRequestCarrierRetired    = fmt.Errorf("%w: request carrier retired before response", ErrLaneUnavailable)
	ErrOperationRetry           = errors.New("session runtime operation fell behind sender replay window")
	errRequestNotDelivered      = errors.New("session runtime request was not delivered")
	errContinuationNotDelivered = errors.New("session runtime continuation was not delivered")
	errRPCOperationAuthority    = errors.New("session runtime request admission returned incomplete operation authority")
)

type operationIDSource struct {
	mu      sync.Mutex
	counter uint64
}

func (source *operationIDSource) New() (protocolsession.OperationID, error) {
	if source == nil {
		return 0, session.ErrRuntimeConfig
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.counter == ^uint64(0) {
		return 0, ErrOperationIDExhausted
	}
	source.counter++
	return protocolsession.OperationID(source.counter), nil
}

// OperationObserver is the consumer contract for a long-lived request owner.
type OperationObserver interface {
	BindTerminalOwner(*TerminalOwner)
	ObserveAuthenticatedOperationViolation(protocolsession.AuthenticatedOperationViolation)
}

type Requests struct {
	runtime *Runtime
	ids     operationIDSource

	mu     sync.Mutex
	closed bool
	calls  map[protocolsession.OperationID]*Call
}

type operationResponse struct {
	message protocolsession.Message
	op      protocolsession.Operation
	lane    session.LaneIdentity
}

func NewRequests(runtime *Runtime, _ io.Reader) *Requests {
	return &Requests{
		runtime: runtime,
		calls:   make(map[protocolsession.OperationID]*Call),
	}
}

func (client *Requests) Runtime() *Runtime {
	if client == nil {
		return nil
	}
	return client.runtime
}

func (client *Requests) Register() error {
	for _, kind := range []protocolsession.MessageKind{
		protocolsession.MessageCatalogResult,
		protocolsession.MessageOpenResults,
		protocolsession.MessageLeaseResult,
		protocolsession.MessageBlockFragment,
		protocolsession.MessageOperationComplete,
		protocolsession.MessageOperationError,
		protocolsession.MessageOperationRetry,
		protocolsession.MessageScanProgress,
		protocolsession.MessageLaneAttach,
		protocolsession.MessagePeerAnswer,
		protocolsession.MessagePeerCandidate,
	} {
		if err := client.runtime.inbound.RegisterHandler(kind, client); err != nil {
			return fmt.Errorf("register receiver response %d: %w", kind, err)
		}
	}
	return nil
}

func (client *Requests) HandleMessage(
	ctx context.Context,
	op protocolsession.Operation,
	message protocolsession.Message,
) error {
	return client.handleInboundMessage(ctx, op, message, session.LaneIdentity{}, time.Time{})
}

func (client *Requests) handleInboundMessage(
	_ context.Context,
	op protocolsession.Operation,
	message protocolsession.Message,
	lane session.LaneIdentity,
	observedAt time.Time,
) error {
	id, ok := message.OperationID()
	if !ok {
		return session.ErrOperationMissing
	}
	client.mu.Lock()
	call := client.calls[id]
	client.mu.Unlock()
	if call == nil || op.IsZero() {
		return nil
	}
	expected := call.Operation()
	if expected.IsZero() {
		if !op.IsCurrent() {
			return nil
		}
	} else if !expected.Same(op) {
		return nil
	}
	if requestlane.Managed(call.requestKind) && senderResponseFinal(message.Kind()) {
		call.requests.Complete(client.runtime.now())
	}
	response := operationResponse{message: message, op: op, lane: lane}
	var err error
	if call.traceEnabled && message.Kind() == protocolsession.MessageOperationError {
		err = call.enqueueAuthenticatedFailure(response, authenticatedFailureSource{
			protocolSessionID: client.runtime.sessionID,
			observations:      client.runtime.protocolObservations,
			observedAt:        observedAt,
			lane:              lane,
			hasLane:           lane.Valid(true),
		})
	} else {
		err = call.enqueue(response)
	}
	if err != nil {
		call.RecordTraceFailure(err)
	}
	return err
}

func (client *Requests) Begin(
	ctx context.Context,
	kind protocolsession.MessageKind,
	body []byte,
) (*Call, error) {
	call, message, err := client.PrepareCall(ctx, kind, body)
	if err != nil {
		return nil, err
	}
	if err := client.BeginCall(ctx, nil, call, message); err != nil {
		return nil, err
	}
	return call, nil
}

func (client *Requests) PrepareCall(
	ctx context.Context,
	kind protocolsession.MessageKind,
	body []byte,
) (*Call, protocolsession.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, protocolsession.Message{}, err
	}
	id, err := client.ids.New()
	if err != nil {
		return nil, protocolsession.Message{}, err
	}
	message, err := protocolsession.NewMessage(kind, &id, body)
	if err != nil {
		return nil, protocolsession.Message{}, err
	}
	return client.newCall(ctx, id, kind), message, nil
}

func (client *Requests) BeginCall(
	ctx context.Context,
	lane *session.LaneIdentity,
	call *Call,
	message protocolsession.Message,
) error {
	id := call.id
	client.mu.Lock()
	if client.closed {
		client.mu.Unlock()
		call.RecordTraceFailure(session.ErrRuntimeClosed)
		client.End(call)
		return protocolsession.MarkNotSent(session.ErrRuntimeClosed)
	}
	if client.calls[id] != nil {
		client.mu.Unlock()
		call.RecordTraceFailure(session.ErrOperationMissing)
		client.End(call)
		return protocolsession.MarkNotSent(session.ErrOperationMissing)
	}
	client.calls[id] = call
	client.mu.Unlock()

	err := client.sendRequest(ctx, lane, call, message)
	if err == nil {
		call.recordProtocolTraceSend(sessionobs.SendStatusConfirmed)
		return nil
	}
	if protocolsession.IsNotSent(err) {
		call.recordProtocolTraceSend(sessionobs.SendStatusNotSent)
		call.RecordTraceFailure(err)
		if client.runtime.ctx.Err() != nil {
			call.RecordTraceFailure(session.ErrRuntimeClosed)
		}
		client.End(call)
		return protocolsession.MarkNotSent(rpcDeliveryError(client.runtime, errRequestNotDelivered, err))
	}
	// Uncertain physical send: the operation remains admitted in operationTable.
	call.recordProtocolTraceSend(sessionobs.SendStatusUncertain)
	if ctx.Err() == nil && client.runtime.ctx.Err() == nil {
		// Transport acceptance is ambiguous: peer may already own this exact
		// operation. Retain the request so a migrated continuation can replay it
		// ahead of dependent control messages on a replacement lane.
		call.setRequestReplay(message)
		return nil
	}
	cleanupErr := client.admitCancellation(call, contentflow.CancelReasonTimeout)
	deliveryErr := rpcDeliveryError(client.runtime, errRequestNotDelivered, errors.Join(err, cleanupErr))
	call.RecordTraceFailure(err)
	call.RecordTraceCleanup(cleanupErr)
	if client.runtime.ctx.Err() != nil {
		call.RecordTraceFailure(session.ErrRuntimeClosed)
	}
	client.End(call)
	return deliveryErr
}

func (client *Requests) sendRequest(
	ctx context.Context,
	lane *session.LaneIdentity,
	call *Call,
	message protocolsession.Message,
) error {
	var violationObserver func(protocolsession.AuthenticatedOperationViolation)
	if message.Kind() == protocolsession.MessagePeerOffer {
		violationObserver = call.observeAuthenticatedOperationViolation
	}
	for {
		if err := client.waitRequestCapacity(ctx, call); err != nil {
			return protocolsession.MarkNotSent(err)
		}
		op, maxContinuations, hasLimit, releaseInFlight, err := client.runtime.operations.AdmitOutboundRequest(
			message, violationObserver,
		)
		if err != nil {
			if protocolsession.IsOperationCapacityError(err) {
				continue
			}
			return protocolsession.MarkNotSent(err)
		}
		if !call.setAuthority(op, maxContinuations, hasLimit) {
			client.runtime.operations.RollbackUnsentRequest(op.ID())
			releaseInFlight(false)
			return protocolsession.MarkNotSent(session.ErrRuntimeClosed)
		}
		selected, reservation, estimate, err := client.runtime.lanes.selectRequestLane(lane, message.Kind())
		if err != nil {
			client.runtime.operations.RollbackUnsentRequest(op.ID())
			releaseInFlight(false)
			return protocolsession.MarkNotSent(err)
		}
		if reservation != nil && !call.requests.Reserve(reservation, estimate) {
			client.runtime.operations.RollbackUnsentRequest(op.ID())
			releaseInFlight(false)
			return protocolsession.MarkNotSent(session.ErrRuntimeClosed)
		}
		var usableAtSelection uint32
		if call.traceEnabled {
			usableAtSelection = client.runtime.lanes.usableCount()
		}
		call.setProtocolTraceLane(selected.identity, usableAtSelection)
		call.bindRequestCarrier(selected.retired)
		sendErr := selected.writer.SendControl(ctx, message, releaseInFlight)
		if sendErr != nil && protocolsession.IsNotSent(sendErr) {
			client.runtime.operations.RollbackUnsentRequest(op.ID())
			call.requests.Reserve(nil, estimate)
			return sendErr
		}
		return sendErr
	}
}

func (client *Requests) SendContinuation(
	ctx context.Context,
	call *Call,
	kind protocolsession.MessageKind,
	body []byte,
) (protocolsession.OperationDisposition, error) {
	if call == nil || call.id.IsZero() {
		return protocolsession.OperationDrop, session.ErrOperationMissing
	}
	if kind == protocolsession.MessagePeerCandidate {
		release, err := call.acquireCandidateSend(ctx, client.runtime.ctx)
		if err != nil {
			return protocolsession.OperationDrop, err
		}
		defer release()
	}
	client.mu.Lock()
	active := client.calls[call.id] == call
	client.mu.Unlock()
	if !active {
		return protocolsession.OperationDrop, session.ErrOperationMissing
	}
	op := call.Operation()
	if op.IsZero() {
		return protocolsession.OperationDrop, session.ErrOperationMissing
	}
	message, err := protocolsession.NewMessage(kind, &call.id, body)
	if err != nil {
		return protocolsession.OperationDrop, err
	}
	if kind == protocolsession.MessageCancel {
		// Commit the local tombstone synchronously; emit a wire CANCEL only when
		// this call actually transitions an active operation.
		transitioned, err := client.runtime.reconcileLocalCancel(op.ID())
		if err != nil {
			return protocolsession.OperationDrop, err
		}
		if !transitioned {
			return protocolsession.OperationDrop, nil
		}
		client.enqueueBestEffortCancel(call, message)
		return protocolsession.OperationDeliver, nil
	}
	disposition, lease, err := client.runtime.operations.AdmitOutboundContinuation(op.ID(), message)
	if err != nil {
		return protocolsession.OperationDrop, err
	}
	if disposition == protocolsession.OperationDrop || lease == nil {
		// Exact continuation replay or already-retired op is an idempotent drop.
		return protocolsession.OperationDrop, nil
	}
	return client.deliverContinuation(ctx, call, message, lease)
}

func (client *Requests) deliverContinuation(
	ctx context.Context,
	call *Call,
	message protocolsession.Message,
	lease *outboundContinuationLease,
) (protocolsession.OperationDisposition, error) {
	defer lease.release()
	var retryCause error
	for attempt := range 2 {
		selected, laneErr := client.continuationLane(call)
		if laneErr != nil {
			lease.rollback()
			return protocolsession.OperationDrop, errors.Join(retryCause, laneErr)
		}
		// Each SendControl gets its own unconditional pin so a caller
		// cancel-in-flight that later fails pre-acceptance still unpins the op.
		attemptPin := client.runtime.operations.pinInFlight(lease.record)
		sendErr := selected.writer.SendControl(ctx, message, attemptPin)
		if sendErr == nil {
			return protocolsession.OperationDeliver, nil
		}
		if !protocolsession.IsNotSent(sendErr) {
			// Uncertain send: keep continuation recorded while attemptPin holds
			// retention until laneWriter settles the in-flight frame.
			return protocolsession.OperationDeliver, protocolsession.MarkSendUncertain(rpcDeliveryError(
				client.runtime, errContinuationNotDelivered, errors.Join(retryCause, sendErr),
			))
		}
		if client.canRetryContinuation(ctx, attempt, sendErr) {
			client.runtime.lanes.markSelectedClosing(selected)
			retryCause = sendErr
			continue
		}
		lease.rollback()
		return protocolsession.OperationDrop, rpcDeliveryError(
			client.runtime, errContinuationNotDelivered, errors.Join(retryCause, sendErr),
		)
	}
	lease.rollback()
	return protocolsession.OperationDrop, errors.Join(retryCause, errContinuationNotDelivered)
}

func (client *Requests) canRetryContinuation(ctx context.Context, attempt int, sendErr error) bool {
	return attempt == 0 && ctx.Err() == nil && client.runtime.ctx.Err() == nil && retryableLaneFailure(sendErr)
}

func (client *Requests) continuationLane(call *Call) (selectedLane, error) {
	call.laneMu.Lock()
	defer call.laneMu.Unlock()
	selected, err := client.runtime.lanes.selectLane(&call.lane)
	if err == nil {
		return selected, nil
	}
	selected, err = client.runtime.lanes.selectLane(nil)
	if err != nil {
		return selectedLane{}, err
	}
	if err := call.queueRequestReplay(selected.writer); err != nil {
		return selectedLane{}, err
	}
	call.lane = selected.identity
	return selected, nil
}

func (client *Requests) Await(ctx context.Context, call *Call) (protocolsession.Message, error) {
	return client.AwaitCall(ctx, call, true)
}

func (client *Requests) AwaitPeer(ctx context.Context, call *Call) (protocolsession.Message, error) {
	return client.AwaitCall(ctx, call, false)
}

func (client *Requests) AwaitCall(
	ctx context.Context,
	call *Call,
	cancelOnContext bool,
) (protocolsession.Message, error) {
	if client == nil || client.runtime == nil {
		return protocolsession.Message{}, session.ErrRuntimeClosed
	}
	if call == nil {
		return protocolsession.Message{}, session.ErrOperationMissing
	}
	if ctx == nil {
		ctx = context.Background()
	}
	carrierRetired := call.controlCarrierRetired()
	for {
		done := call.doneChannel()
		select {
		case <-ctx.Done():
			call.RecordTraceFailure(ctx.Err())
			if !cancelOnContext {
				return protocolsession.Message{}, ctx.Err()
			}
			cleanupErr := client.admitCancellation(call, contentflow.CancelReasonTimeout)
			call.RecordTraceCleanup(cleanupErr)
			return protocolsession.Message{}, errors.Join(ctx.Err(), cleanupErr)
		case <-client.runtime.Done():
			call.RecordTraceFailure(session.ErrRuntimeClosed)
			return protocolsession.Message{}, errors.Join(session.ErrRuntimeClosed, client.runtime.Err())
		case <-done:
			call.RecordTraceFailure(session.ErrOperationMissing)
			return protocolsession.Message{}, session.ErrOperationMissing
		case <-carrierRetired:
			select {
			case response := <-call.messages:
				if message, accepted, err := call.acceptAwaitedResponse(response); accepted {
					return message, err
				}
				continue
			default:
			}
			if client.runtime.Lifetime().Err() != nil {
				call.RecordTraceFailure(session.ErrRuntimeClosed)
				return protocolsession.Message{}, errors.Join(session.ErrRuntimeClosed, client.runtime.Err())
			}
			call.RecordTraceFailure(ErrRequestCarrierRetired)
			return protocolsession.Message{}, ErrRequestCarrierRetired
		case response := <-call.messages:
			if message, accepted, err := call.acceptAwaitedResponse(response); accepted {
				return message, err
			}
		}
	}
}

func (call *Call) acceptAwaitedResponse(response operationResponse) (protocolsession.Message, bool, error) {
	message, accepted := call.acceptResponse(response)
	if !accepted {
		return protocolsession.Message{}, false, nil
	}
	if message.Kind() == protocolsession.MessageOperationRetry {
		call.RecordTraceFailure(ErrOperationRetry)
		return protocolsession.Message{}, true, ErrOperationRetry
	}
	return message, true, nil
}

func (client *Requests) End(call *Call) {
	if call == nil {
		return
	}
	if client == nil {
		call.Close()
		return
	}
	client.mu.Lock()
	if client.calls[call.id] == call {
		delete(client.calls, call.id)
	}
	client.mu.Unlock()
	if client.runtime != nil && client.runtime.protocolOperationTracingEnabled() {
		if event, ok := call.protocolOperationTrace(client.runtime.now()); ok {
			client.runtime.traceProtocolOperation(event)
		}
	}
	call.Close()
}

func (client *Requests) Close() {
	if client == nil {
		return
	}
	client.mu.Lock()
	client.closed = true
	calls := make([]*Call, 0, len(client.calls))
	for _, call := range client.calls {
		calls = append(calls, call)
	}
	clear(client.calls)
	client.mu.Unlock()
	for _, call := range calls {
		call.RecordTraceFailure(session.ErrRuntimeClosed)
		if client.runtime != nil && client.runtime.protocolOperationTracingEnabled() {
			if event, ok := call.protocolOperationTrace(client.runtime.now()); ok {
				client.runtime.traceProtocolOperation(event)
			}
		}
		call.Close()
	}
}

func (client *Requests) CancelAndEnd(call *Call, reason contentflow.CancelReason) error {
	if call == nil {
		return nil
	}
	err := client.admitCancellation(call, reason)
	call.RecordTraceCleanup(err)
	client.End(call)
	return err
}

func (client *Requests) admitCancellation(
	call *Call,
	reason contentflow.CancelReason,
) error {
	if client == nil || client.runtime == nil {
		return session.ErrRuntimeClosed
	}
	if call == nil {
		return session.ErrOperationMissing
	}
	op := call.Operation()
	if op.IsZero() {
		return session.ErrOperationMissing
	}
	transitioned, reconcileErr := client.runtime.reconcileLocalCancel(op.ID())
	if !transitioned {
		return reconcileErr
	}
	body, err := contentflow.EncodeCancelReason(reason)
	if err != nil {
		return errors.Join(err, reconcileErr)
	}
	message, err := protocolsession.NewMessage(protocolsession.MessageCancel, &call.id, body)
	if err != nil {
		return errors.Join(err, reconcileErr)
	}
	client.enqueueBestEffortCancel(call, message)
	return reconcileErr
}

func (client *Requests) cancelLane(call *Call) (selectedLane, error) {
	preferred := call.currentLane()
	selected, err := client.runtime.lanes.selectLane(&preferred)
	if err == nil || errors.Is(err, session.ErrRuntimeClosed) {
		return selected, err
	}
	return client.runtime.lanes.selectLane(nil)
}

func (client *Requests) enqueueBestEffortCancel(call *Call, message protocolsession.Message) {
	// CANCEL is a preemptive termination signal; never route it through
	// continuationLane, which would replay an already-cancelled original request
	// onto a replacement lane and overwrite call.lane for post-mortem tracing.
	for attempt := range 2 {
		selected, laneErr := client.cancelLane(call)
		if laneErr != nil {
			return
		}
		err := selected.writer.EnqueueControl(message, nil)
		if err == nil {
			return
		}
		if attempt == 0 && client.runtime.ctx.Err() == nil && errors.Is(err, protocolsession.ErrWriterStopped) {
			client.runtime.lanes.markSelectedClosing(selected)
			continue
		}
		return
	}
}

func retryableLaneFailure(err error) bool {
	if errors.Is(err, protocolsession.ErrWriterTerminal) ||
		errors.Is(err, protocolsession.ErrSessionTerminated) {
		return false
	}
	var deliveryErr *laneDeliveryError
	return errors.Is(err, protocolsession.ErrWriterStopped) ||
		errors.Is(err, protocolsession.ErrControlQueueFull) ||
		errors.Is(err, protocolsession.ErrDataQueueFull) ||
		errors.As(err, &deliveryErr)
}
