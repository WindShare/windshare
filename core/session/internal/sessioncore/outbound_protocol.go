package sessioncore

import (
	"context"
	"errors"
	"github.com/windshare/windshare/core/content"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func RevisionOperationCause(code uint16) (error, bool) {
	switch code {
	case contentflow.RevisionCodeStale:
		return content.ErrRevisionStale, true
	case contentflow.RevisionCodeNotFound:
		return content.ErrRevisionNotFound, true
	case contentflow.RevisionCodeUnreadable:
		return content.ErrRevisionUnreadable, true
	case contentflow.RevisionCodeUnsupportedStability:
		return content.ErrUnsupportedStability, true
	case contentflow.RevisionCodeQuota:
		return content.ErrQuotaExceeded, true
	case contentflow.RevisionCodeLeaseExpired:
		return content.ErrLeaseExpired, true
	case contentflow.RevisionCodeDrift:
		return content.ErrRevisionDrift, true
	case contentflow.RevisionCodeInvalidLease:
		return content.ErrInvalidLease, true
	default:
		return nil, false
	}
}

var (
	errOutboundReplayAuthority = errors.New("sender response lost outbound replay authority")
	errOutboundNotDelivered    = errors.New("sender response was not delivered")
)

type outboundLaneAttempt func(
	lane selectedLane,
	permit protocolsession.OutboundReplayPermit,
) (protocolsession.SendReceipt, error)

type outboundTransaction struct {
	responses        *Responses
	runtime          *Runtime
	operationID      protocolsession.OperationID
	route            *operationLaneRoute
	lane             selectedLane
	authority        protocolsession.OutboundOperationPermit
	generation       protocolsession.OperationGeneration
	lease            *protocolsession.OutboundOperationLease
	responseSequence uint64
	responseKind     protocolsession.MessageKind
}

func beginOutboundTransaction(
	responses *Responses,
	ctx context.Context,
	operationID protocolsession.OperationID,
) (*outboundTransaction, error) {
	runtime := responses.runtime
	route, err := outboundRoute(ctx, operationID)
	if err != nil {
		return nil, err
	}
	authority, ok := protocolsession.OutboundOperationPermitFromContext(ctx, operationID)
	if !ok {
		return nil, sessioncontract.ErrOperationMissing
	}
	generation, ok := protocolsession.OperationGenerationFromContext(ctx, operationID)
	if !ok || !generation.Same(authority.Generation()) {
		return nil, sessioncontract.ErrOperationMissing
	}
	route, lane, err := runtime.routes.beginSend(runtime.lanes, operationID, route)
	if err != nil {
		return nil, err
	}
	// Validate and lock the exact route before pinning its generation. Otherwise
	// a stale handler could keep refreshing an expired generation without ever
	// owning a live route or performing a send attempt.
	lease, err := authority.AcquireLease()
	if err != nil {
		route.sendMu.Unlock()
		return nil, err
	}
	return &outboundTransaction{
		runtime: runtime, responses: responses, operationID: operationID, route: route, lane: lane,
		authority: authority, generation: generation, lease: lease,
	}, nil
}

func (transaction *outboundTransaction) Close() {
	if transaction.lease != nil {
		transaction.lease.Release()
		transaction.lease = nil
	}
	transaction.route.sendMu.Unlock()
}

func (transaction *outboundTransaction) retainPendingAttempt(
	receipt protocolsession.SendReceipt,
	id protocolsession.SendAttemptIdentity,
) {
	// Caller cancellation ends the wait, but the writer still owns the send and
	// must keep its generation pinned until physical settlement.
	if transaction.lease != nil {
		receipt.ReleaseLeaseOnSettlement(transaction.lease)
		transaction.lease = nil
	}
	if !transaction.responses.settlements.IsZero() {
		receipt.ObserveSettlement(protocolsession.SendAttemptCorrelation{
			ProtocolSessionID: transaction.runtime.sessionID, OperationID: transaction.operationID,
			RequestKind: transaction.route.requestKind, ResponseKind: transaction.responseKind, Identity: id,
		}, transaction.responses.settlements)
	}
}

func (transaction *outboundTransaction) awaitAttempt(
	ctx context.Context,
	id protocolsession.SendAttemptIdentity,
	receipt protocolsession.SendReceipt,
	admissionErr error,
) (protocolsession.SendCompletion, protocolsession.SendAttemptSnapshot, error) {
	if admissionErr != nil {
		snapshot, err := protocolsession.NewRejectedSendAttempt(id, admissionErr)
		return protocolsession.SendCompletion{
			Outcome:             protocolsession.SendOutcomeDropped,
			RetryableAcrossLane: retryableLaneAdmissionError(admissionErr), Err: admissionErr,
		}, snapshot, err
	}
	completion := receipt.Await(ctx)
	if completion.Settled {
		snapshot, err := protocolsession.NewSettledSendAttempt(id, completion)
		return completion, snapshot, err
	}
	snapshot, err := protocolsession.NewPendingSendAttempt(id, completion)
	if err == nil {
		transaction.retainPendingAttempt(receipt, id)
	}
	return completion, snapshot, err
}

func (transaction *outboundTransaction) Run(
	ctx context.Context,
	attempt outboundLaneAttempt,
) (protocolsession.ResponseSendResult, error) {
	if transaction.responseSequence == 0 {
		transaction.responseSequence = transaction.responses.sequence.Add(1)
	}
	excluded := make(map[sessioncontract.LaneIdentity]struct{}, protocolsession.DefaultMaxLogicalLanes)
	var permit protocolsession.OutboundReplayPermit
	var combined error
	var history [protocolsession.DefaultMaxLogicalLanes]protocolsession.SendAttemptSnapshot
	count := 0
	finish := func(end protocolsession.ResponseSendEnd, err error) (protocolsession.ResponseSendResult, error) {
		var result protocolsession.ResponseSendResult
		var resultErr error
		if count > 0 && history[count-1].End() == protocolsession.SendAttemptEndWaitingEnded {
			result, resultErr = protocolsession.NewResponseSendWaitingEnded(end, history[:count])
		} else if count > 0 {
			result, resultErr = protocolsession.NewResponseSendReturned(end, history[:count])
		}
		return result, errors.Join(err, resultErr)
	}
	for index := range protocolsession.DefaultMaxLogicalLanes {
		// Route migration changes future selection only; this identity remains pinned
		// to the writer whose receipt supplies the attempt evidence.
		lane := transaction.lane
		id := protocolsession.SendAttemptIdentity{ResponseSequence: transaction.responseSequence, AttemptSequence: uint32(index + 1), LaneID: lane.identity.ID, LaneEpoch: lane.identity.Epoch}
		receipt, err := attempt(lane, permit)
		completion, snapshot, snapshotErr := transaction.awaitAttempt(ctx, id, receipt, err)
		err = completion.Err
		if snapshotErr != nil {
			failure := transaction.runtime.failMissingReplayAuthority(transaction.operationID, transaction.route, transaction.generation)
			return finish(protocolsession.ResponseSendEndInvalidReceipt, errors.Join(combined, err, snapshotErr, failure))
		}
		// Accumulate physical evidence before any authority or policy decision.
		history[count] = snapshot
		count++
		if !completion.Replay.IsZero() {
			permit = completion.Replay
		}
		if authorityErr := transaction.admissionAuthorityError(completion, permit, err); authorityErr != nil {
			return finish(protocolsession.ResponseSendEndAuthorityLost, errors.Join(combined, err, authorityErr))
		}
		if _, authorityErr := transaction.unknownAuthorityError(completion); authorityErr != nil {
			return finish(protocolsession.ResponseSendEndAuthorityLost, errors.Join(combined, err, authorityErr))
		}
		if err == nil && completion.Outcome == protocolsession.SendOutcomeTransportConfirmed {
			return finish(protocolsession.ResponseSendEndTransportConfirmed, nil)
		}
		if err == nil && !completion.Admitted && completion.Outcome == protocolsession.SendOutcomeDropped {
			return finish(protocolsession.ResponseSendEndPolicySuppressed, nil)
		}
		combined = errors.Join(combined, err)
		if end, interruptionErr := transaction.interruption(ctx); interruptionErr != nil {
			return finish(end, errors.Join(combined, interruptionErr))
		}
		if snapshot.End() == protocolsession.SendAttemptEndWaitingEnded {
			// Await only returns pending when caller cancellation won its wait.
			return finish(protocolsession.ResponseSendEndCallerCanceled, errors.Join(combined, errOutboundNotDelivered))
		}
		if !completion.RetryableAcrossLane {
			return finish(protocolsession.ResponseSendEndRetryDisallowed, errors.Join(combined, errOutboundNotDelivered))
		}
		excluded[lane.identity] = struct{}{}
		candidate, routeErr := transaction.runtime.routes.migrate(transaction.runtime.lanes, transaction.operationID, transaction.route, excluded)
		if routeErr != nil {
			return finish(protocolsession.ResponseSendEndNoUsableLane, errors.Join(combined, routeErr))
		}
		transaction.lane = candidate
	}
	return finish(protocolsession.ResponseSendEndAttemptsExhausted, errors.Join(combined, errOutboundNotDelivered))
}

func (transaction *outboundTransaction) interruption(ctx context.Context) (protocolsession.ResponseSendEnd, error) {
	// Runtime shutdown takes precedence because it ends every caller's ability
	// to retry, even when this caller's wait was also canceled.
	if transaction.runtime.ctx.Err() != nil {
		return protocolsession.ResponseSendEndRuntimeStopped, errors.Join(sessioncontract.ErrRuntimeClosed, transaction.runtime.Err())
	}
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return protocolsession.ResponseSendEndDeadlineExceeded, err
		}
		return protocolsession.ResponseSendEndCallerCanceled, err
	}
	return protocolsession.ResponseSendEndUninitialized, nil
}

func (transaction *outboundTransaction) admissionAuthorityError(
	completion protocolsession.SendCompletion,
	permit protocolsession.OutboundReplayPermit,
	err error,
) error {
	if completion.Admitted && permit.IsZero() {
		return transaction.runtime.failMissingReplayAuthority(
			transaction.operationID, transaction.route, transaction.generation,
		)
	}
	// The writer can reject replay authority before creating a receipt, so this
	// contract failure cannot depend on physical settlement.
	if errors.Is(err, protocolsession.ErrOutboundReplayPermit) {
		return transaction.runtime.failMissingReplayAuthority(
			transaction.operationID, transaction.route, transaction.generation,
		)
	}
	return nil
}

func (transaction *outboundTransaction) unknownAuthorityError(
	completion protocolsession.SendCompletion,
) (bool, error) {
	if completion.Outcome != protocolsession.SendOutcomeUnknown {
		return false, nil
	}
	if !completion.Settled || !completion.Replay.IsZero() {
		return true, nil
	}
	return true, transaction.runtime.failMissingReplayAuthority(
		transaction.operationID, transaction.route, transaction.generation,
	)
}

func (runtime *Runtime) abandonOutboundOperation(
	operationID protocolsession.OperationID,
	route *operationLaneRoute,
	generation protocolsession.OperationGeneration,
) error {
	err := runtime.routes.retireRoute(operationID, route, func() error {
		return runtime.operations.CancelGeneration(generation)
	})
	if err == nil || errors.Is(err, sessioncontract.ErrOperationMissing) {
		return err
	}
	// Without a cancellation tombstone the active operation could retain a slot
	// after its handler has abandoned all output. Terminalizing the shared table
	// is safer than continuing with ambiguous at-most-once authority.
	_ = runtime.router.TerminateLocal()
	runtime.terminateRuntimeFailed(err)
	return err
}

func (runtime *Runtime) abandonBoundOutboundOperation(
	ctx context.Context,
	operationID protocolsession.OperationID,
) error {
	route, err := outboundRoute(ctx, operationID)
	if err != nil {
		return err
	}
	route.sendMu.Lock()
	defer route.sendMu.Unlock()
	generation, ok := protocolsession.OperationGenerationFromContext(ctx, operationID)
	if !ok {
		// The route capability is exact, so it can be released safely, but the
		// missing generation makes operation-table retirement unknowable. Fail the
		// session after releasing the route instead of leaking ambiguous authority.
		runtime.routes.releaseRoute(operationID, route)
		_ = runtime.router.TerminateLocal()
		runtime.terminateRuntimeFailed(sessioncontract.ErrOperationMissing)
		return sessioncontract.ErrOperationMissing
	}
	return runtime.abandonOutboundOperation(operationID, route, generation)
}

func (runtime *Runtime) failMissingReplayAuthority(
	operationID protocolsession.OperationID,
	route *operationLaneRoute,
	generation protocolsession.OperationGeneration,
) error {
	err := errors.Join(
		errOutboundReplayAuthority,
		runtime.abandonOutboundOperation(operationID, route, generation),
	)
	_ = runtime.router.TerminateLocal()
	runtime.terminateRuntimeFailed(err)
	return err
}

func retryableLaneAdmissionError(err error) bool {
	return errors.Is(err, protocolsession.ErrWriterStopped) ||
		errors.Is(err, protocolsession.ErrControlQueueFull) ||
		errors.Is(err, protocolsession.ErrDataQueueFull)
}
