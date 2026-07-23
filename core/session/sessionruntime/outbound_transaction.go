package sessionruntime

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/session/protocolsession"
)

var (
	errOutboundReplayAuthority = errors.New("sender response lost outbound replay authority")
	errOutboundNotDelivered    = errors.New("sender response was not delivered")
)

type outboundLaneAttempt func(
	lane selectedLane,
	permit protocolsession.OutboundReplayPermit,
) (protocolsession.SendReceipt, error)

type outboundTransaction struct {
	runtime     *runtimeCore
	operationID protocolsession.OperationID
	route       *operationLaneRoute
	lane        selectedLane
	authority   protocolsession.OutboundOperationPermit
	generation  protocolsession.OperationGeneration
	lease       *protocolsession.OutboundOperationLease
}

func beginOutboundTransaction(
	runtime *runtimeCore,
	ctx context.Context,
	operationID protocolsession.OperationID,
) (*outboundTransaction, error) {
	route, err := outboundRoute(ctx, operationID)
	if err != nil {
		return nil, err
	}
	authority, ok := protocolsession.OutboundOperationPermitFromContext(ctx, operationID)
	if !ok {
		return nil, ErrOperationMissing
	}
	generation, ok := protocolsession.OperationGenerationFromContext(ctx, operationID)
	if !ok || !generation.Same(authority.Generation()) {
		return nil, ErrOperationMissing
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
		runtime: runtime, operationID: operationID, route: route, lane: lane,
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

func (transaction *outboundTransaction) transferLease(receipt protocolsession.SendReceipt) {
	if transaction.lease == nil {
		return
	}
	receipt.ReleaseLeaseOnSettlement(transaction.lease)
	transaction.lease = nil
}

func (transaction *outboundTransaction) Run(
	ctx context.Context,
	attempt outboundLaneAttempt,
) (protocolsession.SendOutcome, error) {
	excluded := make(map[LaneIdentity]struct{}, protocolsession.DefaultMaxLogicalLanes)
	var permit protocolsession.OutboundReplayPermit
	var combined error
	aggregate := protocolsession.SendOutcomeDropped
	for range protocolsession.DefaultMaxLogicalLanes {
		completion, err := transaction.runLaneAttempt(ctx, attempt, permit)
		if !completion.Replay.IsZero() {
			permit = completion.Replay
		}
		if authorityErr := transaction.admissionAuthorityError(completion, permit, err); authorityErr != nil {
			return aggregate, errors.Join(combined, err, authorityErr)
		}
		unknown, authorityErr := transaction.unknownAuthorityError(completion)
		if unknown {
			aggregate = protocolsession.SendOutcomeUnknown
		}
		if authorityErr != nil {
			return aggregate, errors.Join(combined, err, authorityErr)
		}
		if outcome, done := completedOutboundAttempt(completion, err); done {
			return outcome, nil
		}
		combined = errors.Join(combined, err)
		if retryErr := transaction.retryBoundaryError(ctx, completion, combined); retryErr != nil {
			return aggregate, retryErr
		}
		excluded[transaction.lane.identity] = struct{}{}
		transaction.lane, err = transaction.runtime.routes.migrate(
			transaction.runtime.lanes, transaction.operationID, transaction.route, excluded,
		)
		if err != nil {
			return aggregate, errors.Join(combined, err)
		}
	}
	return aggregate, errors.Join(combined, errOutboundNotDelivered)
}

func (transaction *outboundTransaction) runLaneAttempt(
	ctx context.Context,
	attempt outboundLaneAttempt,
	permit protocolsession.OutboundReplayPermit,
) (protocolsession.SendCompletion, error) {
	receipt, err := attempt(transaction.lane, permit)
	if err != nil {
		return protocolsession.SendCompletion{
			Settled: true, Outcome: protocolsession.SendOutcomeDropped,
			RetryableAcrossLane: retryableLaneAdmissionError(err), Err: err,
		}, err
	}
	completion := receipt.Await(ctx)
	if !completion.Settled {
		transaction.transferLease(receipt)
	}
	return completion, completion.Err
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
	if completion.Settled && errors.Is(err, protocolsession.ErrOutboundReplayPermit) {
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

func completedOutboundAttempt(
	completion protocolsession.SendCompletion,
	err error,
) (protocolsession.SendOutcome, bool) {
	if err == nil && completion.Outcome == protocolsession.SendOutcomeDelivered {
		return protocolsession.SendOutcomeDelivered, true
	}
	if err == nil && !completion.Admitted && completion.Outcome == protocolsession.SendOutcomeDropped {
		return protocolsession.SendOutcomeDropped, true
	}
	return protocolsession.SendOutcomeUnknown, false
}

func (transaction *outboundTransaction) retryBoundaryError(
	ctx context.Context,
	completion protocolsession.SendCompletion,
	combined error,
) error {
	if transaction.runtime.ctx.Err() != nil {
		return errors.Join(combined, ErrRuntimeClosed, transaction.runtime.Err())
	}
	if ctx.Err() != nil {
		return errors.Join(combined, ctx.Err())
	}
	if !completion.Settled || !completion.RetryableAcrossLane {
		return errors.Join(combined, errOutboundNotDelivered)
	}
	return nil
}

func (runtime *runtimeCore) abandonOutboundOperation(
	operationID protocolsession.OperationID,
	route *operationLaneRoute,
	generation protocolsession.OperationGeneration,
) error {
	err := runtime.routes.retireRoute(operationID, route, func() error {
		return runtime.operations.CancelGeneration(generation)
	})
	if err == nil || errors.Is(err, ErrOperationMissing) {
		return err
	}
	// Without a cancellation tombstone the active operation could retain a slot
	// after its handler has abandoned all output. Terminalizing the shared table
	// is safer than continuing with ambiguous at-most-once authority.
	_ = runtime.router.TerminateLocal()
	runtime.recordError(err)
	runtime.cancel()
	return err
}

func (runtime *runtimeCore) abandonBoundOutboundOperation(
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
		runtime.recordError(ErrOperationMissing)
		runtime.cancel()
		return ErrOperationMissing
	}
	return runtime.abandonOutboundOperation(operationID, route, generation)
}

func (runtime *runtimeCore) failMissingReplayAuthority(
	operationID protocolsession.OperationID,
	route *operationLaneRoute,
	generation protocolsession.OperationGeneration,
) error {
	err := errors.Join(
		errOutboundReplayAuthority,
		runtime.abandonOutboundOperation(operationID, route, generation),
	)
	_ = runtime.router.TerminateLocal()
	runtime.recordError(err)
	runtime.cancel()
	return err
}

func retryableLaneAdmissionError(err error) bool {
	return errors.Is(err, protocolsession.ErrWriterStopped) ||
		errors.Is(err, protocolsession.ErrControlQueueFull) ||
		errors.Is(err, protocolsession.ErrDataQueueFull)
}
