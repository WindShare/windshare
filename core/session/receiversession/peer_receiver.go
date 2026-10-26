package receiversession

import (
	"context"
	"errors"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func (runtime *Runtime) OpenPeerOperation(
	ctx context.Context,
	offer []byte,
) (*PeerOperation, error) {
	if runtime == nil || runtime.core == nil || runtime.rpc == nil ||
		runtime.rpc.Runtime() != runtime.core {
		return nil, session.ErrRuntimeClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	call, err := runtime.rpc.Begin(ctx, protocolsession.MessagePeerOffer, offer)
	if err != nil {
		return nil, err
	}
	maximumContinuations, hasContinuationLimit := call.ContinuationLimit()
	if !hasContinuationLimit {
		cleanupErr := runtime.rpc.CancelAndEnd(call, contentflow.CancelReasonOutputAbort)
		if runtime.Lifetime().Err() != nil {
			return nil, errors.Join(session.ErrRuntimeClosed, runtime.Err(), cleanupErr)
		}
		return nil, errors.Join(runtime.core.FailOperationAuthority(), cleanupErr)
	}
	operation := &PeerOperation{
		rpc: runtime.rpc, id: call.ID(), call: call,
		token:                new(receiverPeerOperationToken),
		maximumContinuations: maximumContinuations, hasContinuationLimit: true,
		terminalDone: make(chan struct{}),
	}
	if err := call.BindOperationObserver(peerOperationObserver{operation}); err != nil {
		cleanupErr := runtime.rpc.CancelAndEnd(call, contentflow.CancelReasonOutputAbort)
		if runtime.Lifetime().Err() != nil {
			return nil, errors.Join(session.ErrRuntimeClosed, runtime.Err(), err, cleanupErr)
		}
		return nil, errors.Join(runtime.core.FailOperationAuthority(), err, cleanupErr)
	}
	return operation, nil
}

func (operation *PeerOperation) observeAuthenticatedOperationViolation(
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
	cleanupErr := operation.rpc.CancelAndEnd(call, contentflow.CancelReasonOutputAbort)
	operation.completeTerminalCleanup(cleanupErr)
}

func receiverPeerAuthenticatedViolationEvidence(
	violation protocolsession.AuthenticatedOperationViolation,
) (receiverPeerTerminalEvidence, bool) {
	var provenance PeerTerminalProvenance
	var diagnostic PeerDiagnosticCode
	severity := PeerTerminalSessionUnsafe
	switch violation.Code() {
	case protocolsession.AuthenticatedOperationViolationMalformedFailure:
		provenance = PeerProvenanceRemoteFailureMalformed
		diagnostic = PeerDiagnosticRemoteFailureMalformed
	case protocolsession.AuthenticatedOperationViolationMalformedPeerControl:
		provenance = PeerProvenanceRemoteControlMalformed
		diagnostic = PeerDiagnosticControlMalformed
		severity = PeerTerminalOperationOnly
	case protocolsession.AuthenticatedOperationViolationConflictingPeerAnswer:
		provenance = PeerProvenanceRemoteAnswerConflict
		diagnostic = PeerDiagnosticRemoteAnswerConflict
	case protocolsession.AuthenticatedOperationViolationConflictingFinal:
		provenance = PeerProvenanceRemoteFinalConflict
		diagnostic = PeerDiagnosticRemoteFinalConflict
	case protocolsession.AuthenticatedOperationViolationContinuationContract:
		provenance = PeerProvenanceRemoteContinuationViolation
		diagnostic = PeerDiagnosticRemoteContinuationViolation
	default:
		return receiverPeerTerminalEvidence{}, false
	}
	return newReceiverPeerTerminalEvidence(
		PeerTerminalAuthorityRemote,
		provenance,
		severity,
		receiverPeerDiagnostic(diagnostic),
	), true
}

func (operation *PeerOperation) OperationID() protocolsession.OperationID {
	if operation == nil {
		return protocolsession.OperationID{}
	}
	operation.mu.Lock()
	defer operation.mu.Unlock()
	if operation.id.IsZero() && operation.call != nil {
		operation.id = operation.call.ID()
	}
	return operation.id
}

func (operation *PeerOperation) MaximumContinuations() (int, bool) {
	if operation == nil || !operation.hasContinuationLimit {
		return 0, false
	}
	return operation.maximumContinuations, true
}

func (operation *PeerOperation) SendCandidate(
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
	outcome, err := operation.rpc.SendContinuation(ctx, call, protocolsession.MessagePeerCandidate, body)
	if outcome == protocolsession.SendOutcomeDropped {
		return protocolsession.OperationDrop, err
	}
	return protocolsession.OperationDeliver, err
}

func (operation *PeerOperation) Receive(ctx context.Context) PeerReceiveResult {
	if operation == nil || operation.rpc == nil || operation.rpc.Runtime() == nil {
		return PeerReceiveResult{}
	}
	call, terminal, beginErr := operation.beginReceive()
	if terminal != nil {
		return receiverPeerTerminalResult(*terminal)
	}
	if beginErr != nil {
		return operation.terminateWithoutReceive(
			receiverPeerLocalEvidence(
				PeerProvenanceLocalOperationContract,
				beginErr,
			),
			func(call *sessioncore.Call) error {
				return operation.rpc.CancelAndEnd(call, contentflow.CancelReasonOutputAbort)
			},
		)
	}
	message, err := operation.rpc.AwaitPeer(ctx, call)
	if err != nil {
		return operation.terminateFromReceive(call, operation.classifyReceiveError(ctx, err))
	}
	switch message.Kind() {
	case protocolsession.MessagePeerAnswer, protocolsession.MessagePeerCandidate:
		body, err := protocolsession.SenderControlSemanticBody(message)
		if err != nil {
			return operation.terminateFromReceive(call, newReceiverPeerTerminalEvidence(
				PeerTerminalAuthorityRemote,
				PeerProvenanceRemoteControlMalformed,
				PeerTerminalOperationOnly,
				receiverPeerDiagnostic(PeerDiagnosticControlMalformed),
			))
		}
		return operation.completeControlReceive(PeerControl{kind: message.Kind(), body: body})
	case protocolsession.MessageOperationError:
		evidence := receiverPeerRemoteFailureEvidence(message)
		result := operation.terminateFromReceive(call, evidence)
		if evidence.consequence.provenance == PeerProvenanceRemoteSessionRejected {
			operation.rpc.Runtime().Fail(protocolsession.ErrControlSemantic)
		}
		return result
	default:
		return operation.terminateFromReceive(call, newReceiverPeerTerminalEvidence(
			PeerTerminalAuthorityRemote,
			PeerProvenanceRemoteUnknownControl,
			PeerTerminalOperationOnly,
			receiverPeerDiagnostic(PeerDiagnosticUnknownControl),
		))
	}
}

func (operation *PeerOperation) beginReceive() (
	*sessioncore.Call,
	*PeerTermination,
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
		return nil, nil, session.ErrOperationMissing
	}
	if operation.receiving {
		operation.mu.Unlock()
		return nil, nil, session.ErrOperationOverflow
	}
	operation.receiving = true
	call := operation.call
	operation.mu.Unlock()
	return call, nil, nil
}

func (operation *PeerOperation) completeControlReceive(
	control PeerControl,
) PeerReceiveResult {
	operation.mu.Lock()
	operation.receiving = false
	operation.publishTerminalLocked()
	if operation.terminalTransition.authority == receiverPeerTerminalAuthorityInvalid {
		operation.mu.Unlock()
		return PeerReceiveResult{kind: receiverPeerReceiveResultControl, control: control}
	}
	done := operation.terminalDoneLocked()
	operation.mu.Unlock()
	return receiverPeerTerminalResult(operation.awaitTerminal(done))
}

func (operation *PeerOperation) terminateFromReceive(
	_ *sessioncore.Call,
	evidence receiverPeerTerminalEvidence,
) PeerReceiveResult {
	call, claimed, done := operation.claimTerminal(evidence)
	if claimed {
		cleanupErr := operation.rpc.CancelAndEnd(call, contentflow.CancelReasonOutputAbort)
		operation.completeTerminalCleanup(cleanupErr)
	}
	operation.endReceive()
	return receiverPeerTerminalResult(operation.awaitTerminal(done))
}

// Terminate atomically claims local ownership when the exact operation is still
// active. If Receive or runtime shutdown already won, it joins that outcome
// instead. Every caller observes the same accumulated cause after in-flight
// Receive and exact-call cleanup complete.
func (operation *PeerOperation) Terminate(ctx context.Context) PeerTermination {
	if operation == nil {
		return PeerTermination{}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if done, started := operation.startedTerminal(); started {
		return operation.awaitTerminal(done)
	}
	evidence := newReceiverPeerTerminalEvidence(
		PeerTerminalAuthorityLocal,
		PeerProvenanceLocalExplicitStop,
		PeerTerminalOperationOnly,
	)
	if runtimeEvidence, stopping := operation.runtimeTerminalEvidence(nil); stopping {
		evidence = runtimeEvidence
	}
	result := operation.terminateWithoutReceive(evidence, func(call *sessioncore.Call) error {
		if operation.rpc == nil || operation.rpc.Runtime() == nil {
			if call != nil {
				call.Close()
			}
			return nil
		}
		if evidence.transition.authority == PeerTerminalAuthorityLocal {
			return operation.cancelExact(ctx, call)
		}
		return operation.rpc.CancelAndEnd(call, contentflow.CancelReasonOutputAbort)
	})
	termination, _ := result.Termination()
	return termination
}

func (operation *PeerOperation) terminateWithoutReceive(
	evidence receiverPeerTerminalEvidence,
	cleanup func(*sessioncore.Call) error,
) PeerReceiveResult {
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

func (operation *PeerOperation) cancelExact(ctx context.Context, call *sessioncore.Call) error {
	if call == nil {
		return nil
	}
	body, err := contentflow.EncodeCancelReason(contentflow.CancelReasonSuperseded)
	outcome := protocolsession.SendOutcomeDropped
	if err == nil {
		outcome, err = operation.rpc.SendContinuation(ctx, call, protocolsession.MessageCancel, body)
	}
	if outcome == protocolsession.SendOutcomeDropped {
		generation, _ := call.OperationAuthority()
		err = errors.Join(err, sessioncore.FinalizeLocalCancelIfDropped(
			operation.rpc.Runtime(), generation, outcome,
		))
	}
	operation.rpc.End(call)
	return err
}

func (operation *PeerOperation) claimTerminal(
	evidence receiverPeerTerminalEvidence,
) (*sessioncore.Call, bool, <-chan struct{}) {
	operation.mu.Lock()
	defer operation.mu.Unlock()
	done := operation.terminalDoneLocked()
	if operation.terminalTransition.authority != receiverPeerTerminalAuthorityInvalid {
		operation.mergeTerminalEvidenceLocked(evidence)
		return nil, false, done
	}
	if evidence.transition.authority == receiverPeerTerminalAuthorityInvalid {
		evidence = receiverPeerLocalEvidence(
			PeerProvenanceLocalOperationContract,
			session.ErrOperationMissing,
		)
	}
	operation.terminalTransition = evidence.transition
	operation.terminalConsequence = evidence.consequence
	operation.terminalDiagnostics.merge(evidence.diagnostics)
	operation.closed = true
	call := operation.call
	if operation.id.IsZero() && call != nil {
		operation.id = call.ID()
	}
	// Keep the exact call's observation until receive and cleanup have joined.
	// closed already revokes new sends and receives independently of this reference.
	return call, true, done
}

func (operation *PeerOperation) completeTerminalCleanup(cause error) {
	operation.mu.Lock()
	if !operation.terminalPublished {
		operation.terminalDiagnostics.append(receiverPeerDiagnosticForCause(
			cause,
			PeerDiagnosticCleanupFailed,
		))
		if runtimeEvidence, stopping := operation.runtimeTerminalEvidence(nil); stopping {
			operation.mergeTerminalEvidenceLocked(runtimeEvidence)
		}
	}
	operation.terminalCleanupDone = true
	operation.publishTerminalLocked()
	operation.mu.Unlock()
}

func (operation *PeerOperation) endReceive() {
	operation.mu.Lock()
	operation.receiving = false
	operation.publishTerminalLocked()
	operation.mu.Unlock()
}

func (operation *PeerOperation) startedTerminal() (<-chan struct{}, bool) {
	operation.mu.Lock()
	defer operation.mu.Unlock()
	if operation.terminalTransition.authority == receiverPeerTerminalAuthorityInvalid {
		return nil, false
	}
	return operation.terminalDoneLocked(), true
}

func (operation *PeerOperation) terminalDoneLocked() chan struct{} {
	if operation.terminalDone == nil {
		operation.terminalDone = make(chan struct{})
	}
	return operation.terminalDone
}

func (operation *PeerOperation) mergeTerminalEvidenceLocked(
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

func (operation *PeerOperation) publishTerminalLocked() {
	if operation.terminalPublished ||
		operation.terminalTransition.authority == receiverPeerTerminalAuthorityInvalid ||
		!operation.terminalCleanupDone || operation.receiving {
		return
	}
	operation.terminalPublished = true
	if operation.rpc != nil && operation.rpc.Runtime() != nil {
		runtime := operation.rpc.Runtime()
		termination := operation.terminationLocked()
		stage, cause := receiverPeerProtocolOutcome(termination, sessionobs.ProtocolOperationCauseNone)
		operation.protocolTerminal.Finish(runtime, sessioncore.OperationOutcome{Stage: stage, Cause: cause})
	}
	operation.protocolTerminal = nil
	operation.call = nil
	close(operation.terminalDoneLocked())
}

func (operation *PeerOperation) awaitTerminal(done <-chan struct{}) PeerTermination {
	<-done
	operation.mu.Lock()
	defer operation.mu.Unlock()
	return operation.terminationLocked()
}

func (operation *PeerOperation) terminationLocked() PeerTermination {
	return PeerTermination{
		operationToken: operation.token,
		transition:     operation.terminalTransition,
		consequence:    operation.terminalConsequence,
		diagnostics:    operation.terminalDiagnostics,
	}
}

func (operation *PeerOperation) OwnsTermination(
	termination PeerTermination,
) bool {
	return operation != nil && operation.token != nil &&
		termination.operationToken == operation.token &&
		validReceiverPeerTransition(termination.transition) &&
		validReceiverPeerConsequence(termination.consequence)
}

func validReceiverPeerTransition(transition receiverPeerTerminalTransition) bool {
	switch transition.authority {
	case PeerTerminalAuthorityLocal:
		switch transition.provenance {
		case PeerProvenanceLocalExplicitStop,
			PeerProvenanceLocalContextEnded,
			PeerProvenanceLocalOperationContract:
			return true
		}
	case PeerTerminalAuthorityRemote:
		switch transition.provenance {
		case PeerProvenanceRemoteSessionRejected, PeerProvenanceRemoteOperationRejected,
			PeerProvenanceRemoteUnknownControl,
			PeerProvenanceRemoteControlMalformed,
			PeerProvenanceRemoteFailureMalformed,
			PeerProvenanceRemoteFailureScopeViolation,
			PeerProvenanceRemoteAnswerConflict,
			PeerProvenanceRemoteFinalConflict,
			PeerProvenanceRemoteContinuationViolation:
			return true
		}
	case PeerTerminalAuthorityRuntime:
		return transition.provenance == PeerProvenanceRuntimeStopping
	}
	return false
}

func validReceiverPeerConsequence(consequence receiverPeerTerminalConsequence) bool {
	switch consequence.severity {
	case PeerTerminalOperationOnly:
		switch consequence.provenance {
		case PeerProvenanceLocalExplicitStop,
			PeerProvenanceLocalContextEnded,
			PeerProvenanceLocalOperationContract,
			PeerProvenanceRemoteOperationRejected,
			PeerProvenanceRemoteUnknownControl,
			PeerProvenanceRemoteControlMalformed:
			return true
		}
	case PeerTerminalSessionUnavailable:
		return consequence.provenance == PeerProvenanceRuntimeStopping
	case PeerTerminalSessionUnsafe:
		return consequence.provenance == PeerProvenanceRemoteSessionRejected || consequence.provenance == PeerProvenanceRemoteFailureMalformed ||
			consequence.provenance == PeerProvenanceRemoteFailureScopeViolation ||
			consequence.provenance == PeerProvenanceRemoteAnswerConflict ||
			consequence.provenance == PeerProvenanceRemoteFinalConflict ||
			consequence.provenance == PeerProvenanceRemoteContinuationViolation
	}
	return false
}

func (operation *PeerOperation) activeCall() (*sessioncore.Call, error) {
	if operation == nil || operation.rpc == nil || operation.rpc.Runtime() == nil {
		return nil, session.ErrRuntimeClosed
	}
	operation.mu.Lock()
	defer operation.mu.Unlock()
	if operation.closed || operation.call == nil {
		return nil, session.ErrOperationMissing
	}
	return operation.call, nil
}
