package sessionruntime

import (
	"context"
	"errors"
	"reflect"

	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func (runtime *ReceiverRuntime) OpenPeerOperation(
	ctx context.Context,
	offer []byte,
) (*ReceiverPeerOperation, error) {
	if runtime == nil || runtime.runtimeCore == nil || runtime.rpc == nil ||
		runtime.rpc.runtime != runtime.runtimeCore {
		return nil, ErrRuntimeClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	call, err := runtime.rpc.begin(ctx, protocolsession.MessagePeerOffer, offer)
	if err != nil {
		return nil, err
	}
	maximumContinuations, hasContinuationLimit := call.continuationLimit()
	if !hasContinuationLimit {
		cleanupErr := runtime.rpc.cancelAndEnd(call, contentflow.CancelReasonOutputAbort)
		if runtime.ctx.Err() != nil {
			return nil, errors.Join(ErrRuntimeClosed, runtime.Err(), cleanupErr)
		}
		return nil, errors.Join(runtime.failRPCOperationAuthority(), cleanupErr)
	}
	operation := &ReceiverPeerOperation{
		rpc: runtime.rpc, id: call.id, call: call,
		token:                new(receiverPeerOperationToken),
		maximumContinuations: maximumContinuations, hasContinuationLimit: true,
		terminalDone: make(chan struct{}),
	}
	if err := call.registerAuthenticatedOperationViolationHandler(
		operation.observeAuthenticatedOperationViolation,
	); err != nil {
		cleanupErr := runtime.rpc.cancelAndEnd(call, contentflow.CancelReasonOutputAbort)
		if runtime.ctx.Err() != nil {
			return nil, errors.Join(ErrRuntimeClosed, runtime.Err(), err, cleanupErr)
		}
		return nil, errors.Join(runtime.failRPCOperationAuthority(), err, cleanupErr)
	}
	return operation, nil
}

func (operation *ReceiverPeerOperation) observeAuthenticatedOperationViolation(
	violation protocolsession.AuthenticatedOperationViolation,
) {
	evidence, ok := receiverPeerAuthenticatedViolationEvidence(violation)
	if !ok || operation == nil {
		return
	}
	call, claimed, _ := operation.claimTerminal(evidence)
	if !claimed {
		return
	}
	// Notification runs before the pump begins generic session shutdown. Exact
	// cleanup therefore wakes an admitted Receive while the unsafe consequence is
	// already fixed at this operation's mutex linearization point.
	cleanupErr := operation.rpc.cancelAndEnd(call, contentflow.CancelReasonOutputAbort)
	operation.completeTerminalCleanup(cleanupErr)
}

func receiverPeerAuthenticatedViolationEvidence(
	violation protocolsession.AuthenticatedOperationViolation,
) (receiverPeerTerminalEvidence, bool) {
	var provenance ReceiverPeerTerminalProvenance
	var diagnostic ReceiverPeerDiagnosticCode
	severity := ReceiverPeerTerminalSessionUnsafe
	switch violation.Code() {
	case protocolsession.AuthenticatedOperationViolationMalformedFailure:
		provenance = ReceiverPeerProvenanceRemoteFailureMalformed
		diagnostic = ReceiverPeerDiagnosticRemoteFailureMalformed
	case protocolsession.AuthenticatedOperationViolationMalformedPeerControl:
		provenance = ReceiverPeerProvenanceRemoteControlMalformed
		diagnostic = ReceiverPeerDiagnosticControlMalformed
		severity = ReceiverPeerTerminalOperationOnly
	case protocolsession.AuthenticatedOperationViolationConflictingPeerAnswer:
		provenance = ReceiverPeerProvenanceRemoteAnswerConflict
		diagnostic = ReceiverPeerDiagnosticRemoteAnswerConflict
	case protocolsession.AuthenticatedOperationViolationConflictingFinal:
		provenance = ReceiverPeerProvenanceRemoteFinalConflict
		diagnostic = ReceiverPeerDiagnosticRemoteFinalConflict
	case protocolsession.AuthenticatedOperationViolationContinuationAuthority:
		provenance = ReceiverPeerProvenanceRemoteContinuationAuthorityViolation
		diagnostic = ReceiverPeerDiagnosticRemoteContinuationAuthorityViolation
	default:
		return receiverPeerTerminalEvidence{}, false
	}
	return newReceiverPeerTerminalEvidence(
		ReceiverPeerTerminalAuthorityRemote,
		provenance,
		severity,
		receiverPeerDiagnostic(diagnostic),
	), true
}

func (operation *ReceiverPeerOperation) OperationID() protocolsession.OperationID {
	if operation == nil {
		return protocolsession.OperationID{}
	}
	operation.mu.Lock()
	defer operation.mu.Unlock()
	if operation.id.IsZero() && operation.call != nil {
		operation.id = operation.call.id
	}
	return operation.id
}

func (operation *ReceiverPeerOperation) MaximumContinuations() (int, bool) {
	if operation == nil || !operation.hasContinuationLimit {
		return 0, false
	}
	return operation.maximumContinuations, true
}

func (operation *ReceiverPeerOperation) SendCandidate(
	ctx context.Context,
	body []byte,
) (protocolsession.OperationDisposition, error) {
	call, err := operation.activeCall()
	if err != nil {
		return protocolsession.OperationDrop, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	outcome, err := operation.rpc.sendContinuation(ctx, call, protocolsession.MessagePeerCandidate, body)
	if outcome == protocolsession.SendOutcomeDropped {
		return protocolsession.OperationDrop, err
	}
	return protocolsession.OperationDeliver, err
}

func (operation *ReceiverPeerOperation) Receive(ctx context.Context) ReceiverPeerReceiveResult {
	if operation == nil || operation.rpc == nil || operation.rpc.runtime == nil {
		return ReceiverPeerReceiveResult{}
	}
	call, terminal, beginErr := operation.beginReceive()
	if terminal != nil {
		return receiverPeerTerminalResult(*terminal)
	}
	if beginErr != nil {
		return operation.terminateWithoutReceive(
			receiverPeerLocalEvidence(
				ReceiverPeerProvenanceLocalOperationContract,
				beginErr,
			),
			func(call *operationCall) error {
				return operation.rpc.cancelAndEnd(call, contentflow.CancelReasonOutputAbort)
			},
		)
	}
	message, err := operation.rpc.awaitPeer(ctx, call)
	if err != nil {
		return operation.terminateFromReceive(call, operation.classifyReceiveError(ctx, err))
	}
	switch message.Kind() {
	case protocolsession.MessagePeerAnswer, protocolsession.MessagePeerCandidate:
		body, err := protocolsession.SenderControlSemanticBody(message)
		if err != nil {
			return operation.terminateFromReceive(call, newReceiverPeerTerminalEvidence(
				ReceiverPeerTerminalAuthorityRemote,
				ReceiverPeerProvenanceRemoteControlMalformed,
				ReceiverPeerTerminalOperationOnly,
				receiverPeerDiagnostic(ReceiverPeerDiagnosticControlMalformed),
			))
		}
		return operation.completeControlReceive(ReceiverPeerControl{kind: message.Kind(), body: body})
	case protocolsession.MessageOperationError:
		return operation.terminateFromReceive(call, receiverPeerRemoteFailureEvidence(message))
	default:
		return operation.terminateFromReceive(call, newReceiverPeerTerminalEvidence(
			ReceiverPeerTerminalAuthorityRemote,
			ReceiverPeerProvenanceRemoteUnknownControl,
			ReceiverPeerTerminalOperationOnly,
			receiverPeerDiagnostic(ReceiverPeerDiagnosticUnknownControl),
		))
	}
}

func receiverPeerTerminalResult(terminal ReceiverPeerTermination) ReceiverPeerReceiveResult {
	return ReceiverPeerReceiveResult{kind: receiverPeerReceiveResultTermination, termination: terminal}
}

func (operation *ReceiverPeerOperation) beginReceive() (
	*operationCall,
	*ReceiverPeerTermination,
	error,
) {
	operation.mu.Lock()
	if operation.terminalTransition.authority != receiverPeerTerminalAuthorityInvalid {
		done := operation.terminalDoneLocked()
		operation.mu.Unlock()
		terminal := operation.awaitTerminal(done)
		return nil, &terminal, nil
	}
	if operation.closed || operation.call == nil {
		operation.mu.Unlock()
		return nil, nil, ErrOperationMissing
	}
	if operation.receiving {
		operation.mu.Unlock()
		return nil, nil, ErrOperationOverflow
	}
	operation.receiving = true
	call := operation.call
	operation.mu.Unlock()
	return call, nil, nil
}

func (operation *ReceiverPeerOperation) classifyReceiveError(
	ctx context.Context,
	err error,
) receiverPeerTerminalEvidence {
	operation.mu.Lock()
	hasTransition := operation.terminalTransition.authority != receiverPeerTerminalAuthorityInvalid
	operation.mu.Unlock()
	runtimeTrigger := err
	if hasTransition && isExactReceiverPeerCause(err, ErrOperationMissing) {
		// An established owner closes the exact response sink to wake Receive. A
		// concurrent runtime stop remains relevant, but that synthetic wakeup does not.
		runtimeTrigger = nil
	}
	if evidence, stopping := operation.runtimeTerminalEvidence(runtimeTrigger); stopping {
		// Runtime lifecycle cancellation precedes Done publication because finalizers
		// run in between. Observing the lifecycle avoids attributing a call-close
		// wakeup to the caller during that intentional shutdown window.
		return evidence
	}
	if hasTransition && isExactReceiverPeerCause(err, ErrOperationMissing) {
		// Closing the exact call is only the wakeup for an already-owned terminal
		// transition; it is not a second failure cause.
		return receiverPeerTerminalEvidence{}
	}
	if isExactReceiverPeerCause(err, ErrRuntimeClosed) {
		return receiverPeerRuntimeEvidence(err)
	}
	if ctx != nil && ctx.Err() != nil && isExactReceiverPeerCause(err, ctx.Err()) {
		return receiverPeerLocalEvidence(ReceiverPeerProvenanceLocalContextEnded, err)
	}
	return receiverPeerLocalEvidence(ReceiverPeerProvenanceLocalOperationContract, err)
}

func (operation *ReceiverPeerOperation) runtimeTerminalEvidence(
	trigger error,
) (receiverPeerTerminalEvidence, bool) {
	if operation == nil || operation.rpc == nil || operation.rpc.runtime == nil {
		return receiverPeerRuntimeEvidence(trigger), true
	}
	runtime := operation.rpc.runtime
	if runtime.ctx == nil || runtime.done == nil {
		return receiverPeerRuntimeEvidence(trigger), true
	}
	if runtime.ctx.Err() != nil {
		return receiverPeerRuntimeEvidence(trigger), true
	}
	select {
	case <-runtime.Done():
		return receiverPeerRuntimeEvidence(trigger), true
	default:
		return receiverPeerTerminalEvidence{}, false
	}
}

func receiverPeerLocalEvidence(
	provenance ReceiverPeerTerminalProvenance,
	cause error,
) receiverPeerTerminalEvidence {
	evidence := newReceiverPeerTerminalEvidence(
		ReceiverPeerTerminalAuthorityLocal,
		provenance,
		ReceiverPeerTerminalOperationOnly,
	)
	evidence.diagnostics.append(receiverPeerDiagnosticForCause(cause, ReceiverPeerDiagnosticOpaqueFailure))
	return evidence
}

func receiverPeerRuntimeEvidence(trigger error) receiverPeerTerminalEvidence {
	evidence := newReceiverPeerTerminalEvidence(
		ReceiverPeerTerminalAuthorityRuntime,
		ReceiverPeerProvenanceRuntimeStopping,
		ReceiverPeerTerminalSessionUnavailable,
		receiverPeerDiagnostic(ReceiverPeerDiagnosticRuntimeClosed),
	)
	evidence.diagnostics.append(receiverPeerDiagnosticForCause(trigger, ReceiverPeerDiagnosticOpaqueFailure))
	return evidence
}

func receiverPeerRemoteFailureEvidence(
	message protocolsession.Message,
) receiverPeerTerminalEvidence {
	failure, err := decodeRemoteOperationFailure(message)
	if err != nil {
		return newReceiverPeerTerminalEvidence(
			ReceiverPeerTerminalAuthorityRemote,
			ReceiverPeerProvenanceRemoteFailureMalformed,
			ReceiverPeerTerminalSessionUnsafe,
			receiverPeerDiagnostic(ReceiverPeerDiagnosticRemoteFailureMalformed),
		)
	}
	if failure.Scope() != protocolsession.OperationScopePeer {
		return newReceiverPeerTerminalEvidence(
			ReceiverPeerTerminalAuthorityRemote,
			ReceiverPeerProvenanceRemoteFailureScopeViolation,
			ReceiverPeerTerminalSessionUnsafe,
			receiverPeerRemoteDiagnostic(ReceiverPeerDiagnosticRemoteFailureScopeViolation, failure),
		)
	}
	return newReceiverPeerTerminalEvidence(
		ReceiverPeerTerminalAuthorityRemote,
		ReceiverPeerProvenanceRemoteOperationRejected,
		ReceiverPeerTerminalOperationOnly,
		receiverPeerRemoteDiagnostic(ReceiverPeerDiagnosticRemoteOperationRejected, failure),
	)
}

func receiverPeerDiagnosticForCause(
	cause error,
	fallback ReceiverPeerDiagnosticCode,
) ReceiverPeerDiagnostic {
	switch {
	case cause == nil:
		return ReceiverPeerDiagnostic{}
	case isExactReceiverPeerCause(cause, context.Canceled):
		return receiverPeerDiagnostic(ReceiverPeerDiagnosticContextCanceled)
	case isExactReceiverPeerCause(cause, ErrOperationMissing):
		return receiverPeerDiagnostic(ReceiverPeerDiagnosticOperationMissing)
	case isExactReceiverPeerCause(cause, ErrRuntimeClosed):
		return receiverPeerDiagnostic(ReceiverPeerDiagnosticRuntimeClosed)
	case isExactReceiverPeerCause(cause, ErrOperationOverflow):
		return receiverPeerDiagnostic(ReceiverPeerDiagnosticOperationOverflow)
	case isExactReceiverPeerCause(cause, protocolsession.ErrUnknownMessageKind):
		return receiverPeerDiagnostic(ReceiverPeerDiagnosticUnknownControl)
	default:
		return receiverPeerDiagnostic(fallback)
	}
}

func isExactReceiverPeerCause(cause, sentinel error) bool {
	causeValue := reflect.ValueOf(cause)
	sentinelValue := reflect.ValueOf(sentinel)
	return causeValue.IsValid() && sentinelValue.IsValid() &&
		causeValue.Type() == sentinelValue.Type() &&
		causeValue.Comparable() && causeValue.Equal(sentinelValue)
}

func (operation *ReceiverPeerOperation) completeControlReceive(
	control ReceiverPeerControl,
) ReceiverPeerReceiveResult {
	operation.mu.Lock()
	operation.receiving = false
	operation.publishTerminalLocked()
	if operation.terminalTransition.authority == receiverPeerTerminalAuthorityInvalid {
		operation.mu.Unlock()
		return ReceiverPeerReceiveResult{kind: receiverPeerReceiveResultControl, control: control}
	}
	done := operation.terminalDoneLocked()
	operation.mu.Unlock()
	return receiverPeerTerminalResult(operation.awaitTerminal(done))
}

func (operation *ReceiverPeerOperation) terminateFromReceive(
	_ *operationCall,
	evidence receiverPeerTerminalEvidence,
) ReceiverPeerReceiveResult {
	call, claimed, done := operation.claimTerminal(evidence)
	if claimed {
		cleanupErr := operation.rpc.cancelAndEnd(call, contentflow.CancelReasonOutputAbort)
		operation.completeTerminalCleanup(cleanupErr)
	}
	operation.endReceive()
	return receiverPeerTerminalResult(operation.awaitTerminal(done))
}

// Terminate atomically claims local ownership when the exact operation is still
// active. If Receive or runtime shutdown already won, it joins that outcome
// instead. Every caller observes the same accumulated cause after in-flight
// Receive and exact-call cleanup complete.
func (operation *ReceiverPeerOperation) Terminate(ctx context.Context) ReceiverPeerTermination {
	if operation == nil {
		return ReceiverPeerTermination{}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if done, started := operation.startedTerminal(); started {
		return operation.awaitTerminal(done)
	}
	evidence := newReceiverPeerTerminalEvidence(
		ReceiverPeerTerminalAuthorityLocal,
		ReceiverPeerProvenanceLocalExplicitStop,
		ReceiverPeerTerminalOperationOnly,
	)
	if runtimeEvidence, stopping := operation.runtimeTerminalEvidence(nil); stopping {
		evidence = runtimeEvidence
	}
	result := operation.terminateWithoutReceive(evidence, func(call *operationCall) error {
		if operation.rpc == nil || operation.rpc.runtime == nil {
			if call != nil {
				call.close()
			}
			return nil
		}
		if evidence.transition.authority == ReceiverPeerTerminalAuthorityLocal {
			return operation.cancelExact(ctx, call)
		}
		return operation.rpc.cancelAndEnd(call, contentflow.CancelReasonOutputAbort)
	})
	termination, _ := result.Termination()
	return termination
}

func (operation *ReceiverPeerOperation) terminateWithoutReceive(
	evidence receiverPeerTerminalEvidence,
	cleanup func(*operationCall) error,
) ReceiverPeerReceiveResult {
	call, claimed, done := operation.claimTerminal(evidence)
	if claimed {
		var cleanupErr error
		if cleanup != nil {
			cleanupErr = cleanup(call)
		}
		operation.completeTerminalCleanup(cleanupErr)
	}
	return receiverPeerTerminalResult(operation.awaitTerminal(done))
}

func (operation *ReceiverPeerOperation) cancelExact(ctx context.Context, call *operationCall) error {
	if call == nil {
		return nil
	}
	body, err := contentflow.EncodeCancelReason(contentflow.CancelReasonSuperseded)
	outcome := protocolsession.SendOutcomeDropped
	if err == nil {
		outcome, err = operation.rpc.sendContinuation(ctx, call, protocolsession.MessageCancel, body)
	}
	if outcome == protocolsession.SendOutcomeDropped {
		generation, _ := call.operationAuthority()
		err = errors.Join(err, finalizeLocalCancelIfDropped(
			operation.rpc.runtime, generation, outcome,
		))
	}
	operation.rpc.end(call)
	return err
}

func (operation *ReceiverPeerOperation) claimTerminal(
	evidence receiverPeerTerminalEvidence,
) (*operationCall, bool, <-chan struct{}) {
	operation.mu.Lock()
	defer operation.mu.Unlock()
	done := operation.terminalDoneLocked()
	if operation.terminalTransition.authority != receiverPeerTerminalAuthorityInvalid {
		operation.mergeTerminalEvidenceLocked(evidence)
		return nil, false, done
	}
	if evidence.transition.authority == receiverPeerTerminalAuthorityInvalid {
		evidence = receiverPeerLocalEvidence(
			ReceiverPeerProvenanceLocalOperationContract,
			ErrOperationMissing,
		)
	}
	operation.terminalTransition = evidence.transition
	operation.terminalConsequence = evidence.consequence
	operation.terminalDiagnostics.merge(evidence.diagnostics)
	operation.closed = true
	call := operation.call
	if operation.id.IsZero() && call != nil {
		operation.id = call.id
	}
	operation.call = nil
	return call, true, done
}

func (operation *ReceiverPeerOperation) completeTerminalCleanup(cause error) {
	operation.mu.Lock()
	if !operation.terminalPublished {
		operation.terminalDiagnostics.append(receiverPeerDiagnosticForCause(
			cause,
			ReceiverPeerDiagnosticCleanupFailed,
		))
		if runtimeEvidence, stopping := operation.runtimeTerminalEvidence(nil); stopping {
			operation.mergeTerminalEvidenceLocked(runtimeEvidence)
		}
	}
	operation.terminalCleanupDone = true
	operation.publishTerminalLocked()
	operation.mu.Unlock()
}

func (operation *ReceiverPeerOperation) endReceive() {
	operation.mu.Lock()
	operation.receiving = false
	operation.publishTerminalLocked()
	operation.mu.Unlock()
}

func (operation *ReceiverPeerOperation) startedTerminal() (<-chan struct{}, bool) {
	operation.mu.Lock()
	defer operation.mu.Unlock()
	if operation.terminalTransition.authority == receiverPeerTerminalAuthorityInvalid {
		return nil, false
	}
	return operation.terminalDoneLocked(), true
}

func (operation *ReceiverPeerOperation) terminalDoneLocked() chan struct{} {
	if operation.terminalDone == nil {
		operation.terminalDone = make(chan struct{})
	}
	return operation.terminalDone
}

func (operation *ReceiverPeerOperation) mergeTerminalEvidenceLocked(
	evidence receiverPeerTerminalEvidence,
) {
	if operation.terminalPublished {
		return
	}
	if strongerReceiverPeerTerminalSeverity(
		operation.terminalConsequence.severity,
		evidence.consequence.severity,
	) {
		operation.terminalConsequence = evidence.consequence
	}
	operation.terminalDiagnostics.merge(evidence.diagnostics)
}

func (operation *ReceiverPeerOperation) publishTerminalLocked() {
	if operation.terminalPublished ||
		operation.terminalTransition.authority == receiverPeerTerminalAuthorityInvalid ||
		!operation.terminalCleanupDone || operation.receiving {
		return
	}
	operation.terminalPublished = true
	close(operation.terminalDoneLocked())
}

func (operation *ReceiverPeerOperation) awaitTerminal(done <-chan struct{}) ReceiverPeerTermination {
	<-done
	operation.mu.Lock()
	defer operation.mu.Unlock()
	return ReceiverPeerTermination{
		operationToken: operation.token,
		transition:     operation.terminalTransition,
		consequence:    operation.terminalConsequence,
		diagnostics:    operation.terminalDiagnostics,
	}
}

func (operation *ReceiverPeerOperation) OwnsTermination(
	termination ReceiverPeerTermination,
) bool {
	return operation != nil && operation.token != nil &&
		termination.operationToken == operation.token &&
		validReceiverPeerTransition(termination.transition) &&
		validReceiverPeerConsequence(termination.consequence)
}

func validReceiverPeerTransition(transition receiverPeerTerminalTransition) bool {
	switch transition.authority {
	case ReceiverPeerTerminalAuthorityLocal:
		switch transition.provenance {
		case ReceiverPeerProvenanceLocalExplicitStop,
			ReceiverPeerProvenanceLocalContextEnded,
			ReceiverPeerProvenanceLocalOperationContract:
			return true
		}
	case ReceiverPeerTerminalAuthorityRemote:
		switch transition.provenance {
		case ReceiverPeerProvenanceRemoteOperationRejected,
			ReceiverPeerProvenanceRemoteUnknownControl,
			ReceiverPeerProvenanceRemoteControlMalformed,
			ReceiverPeerProvenanceRemoteFailureMalformed,
			ReceiverPeerProvenanceRemoteFailureScopeViolation,
			ReceiverPeerProvenanceRemoteAnswerConflict,
			ReceiverPeerProvenanceRemoteFinalConflict,
			ReceiverPeerProvenanceRemoteContinuationAuthorityViolation:
			return true
		}
	case ReceiverPeerTerminalAuthorityRuntime:
		return transition.provenance == ReceiverPeerProvenanceRuntimeStopping
	}
	return false
}

func validReceiverPeerConsequence(consequence receiverPeerTerminalConsequence) bool {
	switch consequence.severity {
	case ReceiverPeerTerminalOperationOnly:
		switch consequence.provenance {
		case ReceiverPeerProvenanceLocalExplicitStop,
			ReceiverPeerProvenanceLocalContextEnded,
			ReceiverPeerProvenanceLocalOperationContract,
			ReceiverPeerProvenanceRemoteOperationRejected,
			ReceiverPeerProvenanceRemoteUnknownControl,
			ReceiverPeerProvenanceRemoteControlMalformed:
			return true
		}
	case ReceiverPeerTerminalSessionUnavailable:
		return consequence.provenance == ReceiverPeerProvenanceRuntimeStopping
	case ReceiverPeerTerminalSessionUnsafe:
		return consequence.provenance == ReceiverPeerProvenanceRemoteFailureMalformed ||
			consequence.provenance == ReceiverPeerProvenanceRemoteFailureScopeViolation ||
			consequence.provenance == ReceiverPeerProvenanceRemoteAnswerConflict ||
			consequence.provenance == ReceiverPeerProvenanceRemoteFinalConflict ||
			consequence.provenance == ReceiverPeerProvenanceRemoteContinuationAuthorityViolation
	}
	return false
}

func (operation *ReceiverPeerOperation) activeCall() (*operationCall, error) {
	if operation == nil || operation.rpc == nil || operation.rpc.runtime == nil {
		return nil, ErrRuntimeClosed
	}
	operation.mu.Lock()
	defer operation.mu.Unlock()
	if operation.closed || operation.call == nil {
		return nil, ErrOperationMissing
	}
	return operation.call, nil
}
