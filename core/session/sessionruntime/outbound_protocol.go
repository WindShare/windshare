package sessionruntime

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

func newReceiverSenderControlValidator(
	peer protocolsession.SenderControlSemanticValidator,
) (protocolsession.SenderControlSemanticValidator, error) {
	validatePeer := protocolsession.SenderControlSemanticValidatorFunc(func(
		kind protocolsession.MessageKind,
		operationID protocolsession.OperationID,
		semantic []byte,
	) error {
		if peer == nil {
			// A relay-only receiver must fail closed if a sender unexpectedly
			// introduces peer signaling that no connectivity owner can decode.
			return protocolsession.ErrControlSemantic
		}
		return peer.ValidateSenderControl(kind, operationID, semantic)
	})
	return protocolsession.NewSenderControlSemanticRegistry(
		protocolsession.SenderControlSemanticRule{
			Kind: protocolsession.MessageCatalogResult,
			Validate: func(_ protocolsession.MessageKind, _ protocolsession.OperationID, semantic []byte) error {
				_, err := catalogflow.DecodeCatalogResult(semantic)
				return err
			},
		},
		protocolsession.SenderControlSemanticRule{
			Kind: protocolsession.MessageOpenResults,
			Validate: func(_ protocolsession.MessageKind, _ protocolsession.OperationID, semantic []byte) error {
				return contentflow.ValidateOpenResults(semantic)
			},
		},
		protocolsession.SenderControlSemanticRule{
			Kind: protocolsession.MessageOperationComplete,
			Validate: func(_ protocolsession.MessageKind, _ protocolsession.OperationID, semantic []byte) error {
				_, err := contentflow.DecodeOperationComplete(semantic)
				return err
			},
		},
		protocolsession.SenderControlSemanticRule{
			Kind: protocolsession.MessageLeaseResult,
			Validate: func(_ protocolsession.MessageKind, _ protocolsession.OperationID, semantic []byte) error {
				return contentflow.ValidateLeaseResult(semantic)
			},
		},
		protocolsession.SenderControlSemanticRule{
			Kind: protocolsession.MessageLaneAttach,
			Validate: func(_ protocolsession.MessageKind, operationID protocolsession.OperationID, semantic []byte) error {
				_, err := decodeLaneGrant(semantic, operationID)
				return err
			},
		},
		protocolsession.SenderControlSemanticRule{Kind: protocolsession.MessagePeerAnswer, Validate: validatePeer},
		protocolsession.SenderControlSemanticRule{Kind: protocolsession.MessagePeerCandidate, Validate: validatePeer},
	)
}

type RemoteOperationFailureSnapshot struct {
	scope      uint8
	code       uint16
	retryable  bool
	retryAfter time.Duration
	message    string
}

func (failure RemoteOperationFailureSnapshot) Scope() uint8              { return failure.scope }
func (failure RemoteOperationFailureSnapshot) Code() uint16              { return failure.code }
func (failure RemoteOperationFailureSnapshot) Retryable() bool           { return failure.retryable }
func (failure RemoteOperationFailureSnapshot) RetryAfter() time.Duration { return failure.retryAfter }
func (failure RemoteOperationFailureSnapshot) Message() string           { return failure.message }

type RemoteOperationError struct {
	failure RemoteOperationFailureSnapshot
}

func (RemoteOperationError) Error() string { return "sender rejected the operation" }

func (err RemoteOperationError) Failure() RemoteOperationFailureSnapshot { return err.failure }

// NewRemoteOperationError materializes an immutable diagnostic value from an
// already-owned snapshot. It carries no terminal authority; consumers obtain
// session consequences only from a sealed operation termination.
func NewRemoteOperationError(failure RemoteOperationFailureSnapshot) RemoteOperationError {
	return RemoteOperationError{failure: failure}
}

func decodeRemoteOperationFailure(
	message protocolsession.Message,
) (RemoteOperationFailureSnapshot, error) {
	body, err := protocolsession.SenderControlSemanticBody(message)
	if err != nil {
		return RemoteOperationFailureSnapshot{}, err
	}
	failure, err := protocolsession.DecodeOperationFailure(body)
	if err != nil {
		return RemoteOperationFailureSnapshot{}, err
	}
	return RemoteOperationFailureSnapshot{
		scope: failure.Scope, code: failure.Code, retryable: failure.Retryable,
		retryAfter: failure.RetryAfter, message: strings.Clone(failure.Message),
	}, nil
}

func remoteOperationError(message protocolsession.Message) error {
	failure, err := decodeRemoteOperationFailure(message)
	if err != nil {
		return err
	}
	return RemoteOperationError{failure: failure}
}

func remoteRevisionOperationError(message protocolsession.Message) error {
	failure, err := decodeRemoteOperationFailure(message)
	if err != nil {
		return sessionProtocolBoundaryError(err)
	}
	if failure.Scope() != protocolsession.OperationScopeRevision {
		return sessionProtocolBoundaryError(protocolsession.ErrInvalidOperationFailure)
	}
	return revisionOperationError(failure)
}

func remoteDirectoryOperationError(message protocolsession.Message) error {
	failure, err := decodeRemoteOperationFailure(message)
	if err != nil {
		return sessionProtocolBoundaryError(err)
	}
	if failure.Scope() != protocolsession.OperationScopeDirectory {
		return sessionProtocolBoundaryError(protocolsession.ErrInvalidOperationFailure)
	}
	return catalogDirectoryBoundaryError(NewRemoteOperationError(failure))
}

func blockRequestOperationError(message protocolsession.Message) error {
	failure, err := decodeRemoteOperationFailure(message)
	if err != nil {
		return sessionProtocolBoundaryError(err)
	}
	remote := NewRemoteOperationError(failure)
	switch failure.Scope() {
	case protocolsession.OperationScopeBlock:
		return isolatedBlockOperationError(remote)
	case protocolsession.OperationScopeRevision:
		return revisionOperationError(failure)
	default:
		return sessionProtocolBoundaryError(protocolsession.ErrInvalidOperationFailure)
	}
}

func isolatedBlockOperationError(remote RemoteOperationError) error {
	// FetchBlock reaches this branch only after the transport operation has
	// terminally rejected this block demand. The peer session remains valid;
	// bind the failure to the current file so sibling revisions can continue.
	return sourceBoundaryError(transferfault.SourceUnavailable, remote)
}

func revisionOperationError(failure RemoteOperationFailureSnapshot) error {
	return classifyRevisionFailure(NewRemoteOperationError(failure), failure.Code(), failure.Retryable())
}

func remoteRevisionFailureError(remote *RemoteRevisionError) error {
	if remote == nil {
		return sessionProtocolBoundaryError(protocolsession.ErrInvalidOperationFailure)
	}
	failure := remote.Failure()
	return classifyRevisionFailure(
		remote, failure.Code, failure.Retryable,
	)
}

func classifyRevisionFailure(diagnostic error, code uint16, retryable bool) error {
	cause, ok := revisionOperationCause(code)
	if !ok {
		return sessionProtocolBoundaryError(errors.Join(protocolsession.ErrInvalidOperationFailure, diagnostic))
	}
	revisionFailure := errors.Join(diagnostic, cause)
	if code == contentflow.RevisionCodeDrift {
		return sourceBoundaryError(transferfault.SourceRevisionInvalidated, revisionFailure)
	}
	if !retryable && permanentRevisionOperationCode(code) {
		return sourceBoundaryError(transferfault.SourcePermanent, revisionFailure)
	}
	if code == contentflow.RevisionCodeStale {
		return sourceBoundaryError(transferfault.SourceRevisionChanged, revisionFailure)
	}
	return sourceBoundaryError(transferfault.SourceUnavailable, revisionFailure)
}

func sessionProtocolBoundaryError(cause error) error {
	value, _ := transferfault.NewSession(
		transferfault.ScopeSessionTerminal, transferfault.SessionProtocol,
	)
	return transferfault.Wrap(value, cause)
}

func sessionTransportBoundaryError(cause error) error {
	value, _ := transferfault.NewSession(
		transferfault.ScopeSessionTerminal, transferfault.SessionTransport,
	)
	return transferfault.Wrap(value, cause)
}

func dependencyBoundaryError(cause error) error {
	return transferfault.Wrap(transferfault.DependencyContractFault(), cause)
}

func catalogDirectoryBoundaryError(cause error) error {
	value, _ := transferfault.NewCatalog(
		transferfault.ScopeDirectoryLocal, transferfault.CatalogUnavailable,
	)
	return transferfault.Wrap(value, cause)
}

func sourceBoundaryError(code transferfault.SourceCode, cause error) error {
	value, _ := transferfault.NewSource(transferfault.ScopeFileLocal, code)
	return transferfault.Wrap(value, cause)
}

func permanentRevisionOperationCode(code uint16) bool {
	switch code {
	case contentflow.RevisionCodeStale,
		contentflow.RevisionCodeNotFound,
		contentflow.RevisionCodeUnreadable,
		contentflow.RevisionCodeUnsupportedStability:
		return true
	default:
		return false
	}
}

func revisionOperationCause(code uint16) (error, bool) {
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
	runtime          *runtimeCore
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
	if transaction.runtime.protocolOperationTracingEnabled() {
		receipt.ObserveSettlement(protocolsession.SendAttemptCorrelation{
			ProtocolSessionID: transaction.runtime.sessionID, OperationID: transaction.operationID,
			RequestKind: transaction.route.requestKind, ResponseKind: transaction.responseKind, Identity: id,
		}, transaction.runtime.receiptObservations)
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
		transaction.responseSequence = transaction.runtime.responseSequence.Add(1)
	}
	excluded := make(map[LaneIdentity]struct{}, protocolsession.DefaultMaxLogicalLanes)
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
		return protocolsession.ResponseSendEndRuntimeStopped, errors.Join(ErrRuntimeClosed, transaction.runtime.Err())
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
	runtime.terminateRuntimeFailed(err)
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
		runtime.terminateRuntimeFailed(ErrOperationMissing)
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
	runtime.terminateRuntimeFailed(err)
	return err
}

func retryableLaneAdmissionError(err error) bool {
	return errors.Is(err, protocolsession.ErrWriterStopped) ||
		errors.Is(err, protocolsession.ErrControlQueueFull) ||
		errors.Is(err, protocolsession.ErrDataQueueFull)
}
