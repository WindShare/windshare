package v2peer

import (
	"context"
	"errors"
	"fmt"
	"sync"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

type receiverExecutionResult struct {
	workflow                receiverWorkflowResult
	workflowContextCanceled bool
	termination             ReceiverSignalingTermination
	teardown                peerTransportTeardown
	localGeneration         uint64
}

type receiverWorkflowResult struct {
	cause                error
	decision             receiverAttemptDecision
	diagnosticsTruncated bool
}

func receiverWorkflowDiagnostic(cause error) receiverWorkflowResult {
	return receiverWorkflowResult{cause: cause}
}

func receiverPreOperationFailure(
	cause error,
	provenance ReceiverTerminalProvenance,
) receiverWorkflowResult {
	return receiverWorkflowResult{
		cause: cause,
		decision: receiverOperationDecision(
			ReceiverTerminalLocal,
			provenance,
		),
	}
}

func receiverPreOperationAdapterFailure(cause error) receiverWorkflowResult {
	return receiverPreOperationFailure(
		errors.Join(ErrProtocol, cause),
		ReceiverProvenanceSignalingAdapterContract,
	)
}

func receiverWorkflowUnsafe(
	cause error,
	provenance ReceiverTerminalProvenance,
) receiverWorkflowResult {
	return receiverWorkflowResult{
		cause:    cause,
		decision: receiverUnsafeConsequence(provenance),
	}
}

func (attempt *ReceiverAttempt) execute() receiverExecutionResult {
	execution := newReceiverExecution(attempt)
	workflow := execution.openSignaling()
	if workflow.cause == nil && execution.operation != nil {
		execution.startWorkers()
		workflow = execution.runEvents()
	}
	var workflowContextCanceled bool
	completionDecision := receiverAttemptDecision{}
	if execution.operation == nil && attempt.ctx.Err() != nil {
		completionDecision = attempt.preOperationCompletionDecision(context.Cause(attempt.ctx))
	}
	workflow, workflowContextCanceled = receiverWorkflowCompletion(
		workflow,
		attempt.ctx,
		completionDecision,
	)
	workflow.cause, workflow.diagnosticsTruncated = snapshotReceiverCauseWithStatus(workflow.cause)
	termination, teardown := execution.close(workflow.cause)
	teardownCause, teardownTruncated := snapshotReceiverCauseWithStatus(teardown.cause())
	workflow.cause = joinReceiverResiduals([]error{workflow.cause, teardownCause})
	workflow.diagnosticsTruncated = workflow.diagnosticsTruncated || teardownTruncated
	return receiverExecutionResult{
		workflow: workflow, workflowContextCanceled: workflowContextCanceled,
		termination: termination, teardown: teardown,
		localGeneration: execution.binding.localGeneration,
	}
}

func receiverWorkflowCompletion(
	workflow receiverWorkflowResult,
	ctx context.Context,
	preOperationDecision receiverAttemptDecision,
) (receiverWorkflowResult, bool) {
	if ctx == nil || ctx.Err() == nil {
		return workflow, false
	}
	// A competing event may win runEvents after timeout/capacity cancellation.
	// Joining the captured cancel cause preserves that genuine workflow failure;
	// the residual classifier removes only an exact local context.Canceled leaf.
	workflow.cause = errors.Join(workflow.cause, context.Cause(ctx))
	if preOperationDecision.transitionOwner != "" {
		// An established runtime ending outranks the local wakeup that exposed it.
		// Otherwise the observed pre-operation context end owns the transition.
		if workflow.decision.disposition == ReceiverDispositionSessionUnavailable {
			workflow.decision = mergeReceiverAttemptDecisions(workflow.decision, preOperationDecision)
		} else {
			workflow.decision = mergeReceiverAttemptDecisions(preOperationDecision, workflow.decision)
		}
	}
	return workflow, true
}

type receiverExecution struct {
	attempt                *ReceiverAttempt
	operation              *receiverBoundSignalingOperation
	binding                ReceiverSignalingOperationBinding
	children               sync.WaitGroup
	attachmentMu           sync.Mutex
	attachmentResult       receiverEvent
	attachmentDone         chan struct{}
	attachmentTerminalOwns bool

	answerSeen       bool
	channelOpened    bool
	attachStarted    bool
	localCandidates  int
	remoteCandidates int
	queuedCandidates []v2signal.Candidate
}

func newReceiverExecution(attempt *ReceiverAttempt) *receiverExecution {
	return &receiverExecution{
		attempt: attempt, binding: newReceiverSignalingOperationBinding(),
		attachmentDone: make(chan struct{}),
	}
}

func (execution *receiverExecution) openSignaling() receiverWorkflowResult {
	offer, err := execution.attempt.peer.CreateOffer(nil)
	if err != nil {
		return receiverPreOperationFailure(
			errors.Join(ErrNegotiation, fmt.Errorf("create local offer: %w", err)),
			ReceiverProvenanceLocalNegotiationFailure,
		)
	}
	if err := execution.attempt.peer.SetLocalDescription(offer); err != nil {
		return receiverPreOperationFailure(
			errors.Join(ErrNegotiation, fmt.Errorf("set local offer: %w", err)),
			ReceiverProvenanceLocalNegotiationFailure,
		)
	}
	localOffer := execution.attempt.peer.LocalDescription()
	if localOffer == nil || localOffer.Type != pion.SDPTypeOffer {
		return receiverPreOperationFailure(
			errors.Join(ErrNegotiation, errors.New("PeerConnection did not retain the local offer")),
			ReceiverProvenanceLocalNegotiationFailure,
		)
	}
	body, err := v2signal.EncodeOffer(v2signal.Offer{
		Binding: execution.attempt.binding, SDP: localOffer.SDP,
	})
	if err != nil {
		return receiverPreOperationFailure(
			errors.Join(ErrNegotiation, err),
			ReceiverProvenanceLocalNegotiationFailure,
		)
	}
	operation, err := execution.attempt.signaling.OpenPeerOperation(
		execution.attempt.phaseContext,
		execution.binding,
		body,
	)
	if operation == nil {
		return execution.classifySignalingOpenFailure(err)
	}
	operationID := operation.OperationID()
	boundOperation := newReceiverBoundSignalingOperation(operation, operationID, execution.binding)
	execution.operation = boundOperation
	validationErr := err
	var failure *receiverSignalingOpenFailure
	if errors.As(err, &failure) && failure.ownedBy(execution.binding) {
		validationErr = failure.diagnostics
	}
	if operationID.IsZero() {
		validationErr = errors.Join(validationErr, ErrNegotiation)
	} else {
		maximumContinuations, authorityOK := operation.MaximumContinuations()
		if !authorityOK || execution.attempt.factory.maxCandidates != maximumContinuations {
			validationErr = errors.Join(validationErr, ErrConfig, protocolsession.ErrContinuationAuthority)
		}
	}
	if validationErr != nil {
		boundOperation.recordAdapterFailure(validationErr)
	}
	if !execution.attempt.bindOperation(boundOperation) {
		_ = boundOperation.terminateExact()
		return receiverWorkflowDiagnostic(errors.Join(validationErr, context.Canceled))
	}
	return receiverWorkflowDiagnostic(validationErr)
}

func (execution *receiverExecution) classifySignalingOpenFailure(err error) receiverWorkflowResult {
	if failure, ok := errors.AsType[*receiverSignalingOpenFailure](err); ok {
		if !failure.ownedBy(execution.binding) {
			return receiverPreOperationAdapterFailure(nil)
		}
		return receiverWorkflowResult{
			cause: failure.diagnostics, decision: failure.decision,
			diagnosticsTruncated: failure.diagnosticsTruncated,
		}
	}
	if err == nil {
		return receiverPreOperationAdapterFailure(nil)
	}
	phaseCause := context.Cause(execution.attempt.phaseContext)
	combined := errors.Join(err, phaseCause, context.Cause(execution.attempt.ctx))
	provenance := ReceiverProvenanceLocalNegotiationFailure
	if errors.Is(phaseCause, ErrPeerNegotiationTimeout) {
		provenance = ReceiverProvenanceLocalNegotiationTimeout
	}
	workflow := receiverPreOperationFailure(combined, provenance)
	if phaseCause == nil && execution.attempt.ctx.Err() == nil {
		workflow.cause = errors.Join(ErrNegotiation, combined)
	}
	return workflow
}

func (execution *receiverExecution) startWorkers() {
	execution.children.Add(2)
	go func() {
		defer execution.children.Done()
		for {
			result := execution.operation.receive(execution.attempt.ctx)
			if termination, ok := result.Termination(); ok {
				execution.attempt.push(receiverEvent{
					kind: receiverSignalingTerminated, terminal: termination,
				})
				return
			}
			if control, ok := result.Control(); ok {
				execution.attempt.push(receiverEvent{kind: receiverControl, control: control})
				continue
			}
			execution.operation.recordAdapterFailure(nil)
			execution.attempt.push(receiverEvent{kind: receiverSignalingTerminated})
			return
		}
	}()
	go func() {
		defer execution.children.Done()
		select {
		case <-execution.attempt.ctx.Done():
			return
		case <-execution.attempt.channel.Opened():
			execution.attempt.push(receiverEvent{kind: receiverChannelOpened})
		}
		select {
		case <-execution.attempt.ctx.Done():
		case <-execution.attempt.channel.Done():
			execution.attempt.push(receiverEvent{
				kind: receiverChannelDone, err: execution.attempt.channel.Err(),
			})
		}
	}()
}

func (execution *receiverExecution) close(
	result error,
) (ReceiverSignalingTermination, peerTransportTeardown) {
	var termination ReceiverSignalingTermination
	if execution.operation != nil {
		termination = execution.operation.terminateExact()
	}
	// Terminate is the exact-object join barrier. Running it before outer context
	// cancellation keeps that context from becoming a competing lifecycle signal
	// and makes its returned cause include receive and cleanup failures.
	execution.attempt.cancel(result)
	execution.attempt.phases.terminate(result)
	var teardown peerTransportTeardown
	if execution.attempt.transport == nil {
		teardown = teardownPeerTransport(execution.attempt.peer, nil)
	} else {
		_ = execution.attempt.transport.closeIfUnconsumed()
	}
	execution.children.Wait()
	if execution.attempt.transport != nil {
		teardown = execution.attempt.transport.teardownSnapshot()
	}
	return termination, teardown
}

func (execution *receiverExecution) runEvents() receiverWorkflowResult {
	for {
		var terminal bool
		var result receiverWorkflowResult
		select {
		case <-execution.attempt.ctx.Done():
			terminal, result = execution.settleAttemptContext()
		case expiration := <-execution.attempt.phases.expirationEvents():
			terminal, result = execution.handlePhaseExpiration(expiration)
		case event := <-execution.attempt.events:
			terminal, result = execution.settleAttemptEvent(event)
		}
		if terminal {
			return result
		}
	}
}

func (execution *receiverExecution) settleAttemptContext() (bool, receiverWorkflowResult) {
	cause := context.Cause(execution.attempt.ctx)
	if owned, admitted, result := execution.settleAttachmentAtTerminal(cause); owned {
		if admitted && execution.attempt.ctx.Err() == nil {
			return false, receiverWorkflowResult{}
		}
		return true, result
	}
	return true, receiverWorkflowResult{cause: cause}
}

func (execution *receiverExecution) handlePhaseExpiration(
	expiration peerPhaseExpiration,
) (bool, receiverWorkflowResult) {
	if expiration.phase == PeerAttemptPhaseNegotiation {
		reconciled, reconcileErr := execution.reconcileOpen()
		if reconcileErr != nil {
			return true, receiverWorkflowResult{cause: reconcileErr}
		}
		if reconciled {
			return false, receiverWorkflowResult{}
		}
	}
	expired, settling := execution.attempt.phases.expire(expiration)
	if !expired && !settling {
		return false, receiverWorkflowResult{}
	}
	if owned, admitted, result := execution.settleAttachmentAtTerminal(expiration.cause); owned {
		if admitted && execution.attempt.ctx.Err() == nil {
			return false, receiverWorkflowResult{}
		}
		return true, result
	}
	return true, receiverPreOperationFailure(
		expiration.cause,
		receiverTimeoutProvenance(expiration.phase),
	)
}

func (execution *receiverExecution) settleAttemptEvent(
	event receiverEvent,
) (bool, receiverWorkflowResult) {
	done, result := execution.handleEvent(event)
	if result.cause == nil && !done {
		return false, receiverWorkflowResult{}
	}
	if execution.attachmentTerminalOwns {
		execution.attachmentTerminalOwns = false
		return true, result
	}
	if owned, _, settled := execution.settleAttachmentAtTerminal(result.cause); owned {
		return true, settled
	}
	return true, result
}

func (execution *receiverExecution) handleEvent(event receiverEvent) (bool, receiverWorkflowResult) {
	switch event.kind {
	case receiverLocalCandidate:
		return false, receiverWorkflowResult{cause: execution.sendLocalCandidate(event.candidate)}
	case receiverControl:
		return false, execution.acceptControl(event.control)
	case receiverSignalingTerminated:
		if execution.operation == nil || !event.terminal.ownedBy(execution.operation.binding) {
			if execution.operation != nil {
				execution.operation.recordAdapterFailure(nil)
			}
			return true, receiverWorkflowResult{cause: ErrProtocol}
		}
		return true, receiverWorkflowResult{}
	case receiverConnectionFailed:
		return true, receiverWorkflowResult{cause: event.err}
	case receiverChannelOpened:
		return false, receiverWorkflowResult{cause: execution.startAttachment()}
	case receiverChannelDone:
		return true, receiverWorkflowResult{cause: execution.channelClosed(event.err)}
	case receiverAttached:
		settled, admitted, result := execution.finishAttachment(event)
		if !settled {
			return false, receiverWorkflowResult{}
		}
		execution.attachmentTerminalOwns = !admitted
		return !admitted, result
	case receiverUnexpectedDataChannel:
		return true, receiverWorkflowResult{cause: errors.Join(errChannelAdmission, event.err)}
	default:
		return true, receiverWorkflowResult{cause: ErrProtocol}
	}
}

func (execution *receiverExecution) sendLocalCandidate(candidate v2signal.Candidate) error {
	body, err := v2signal.EncodeCandidate(candidate)
	if err != nil {
		return err
	}
	disposition, err := execution.operation.operation.SendCandidate(execution.attempt.ctx, body)
	if err != nil || disposition == protocolsession.OperationDrop {
		return err
	}
	execution.localCandidates++
	if execution.localCandidates > execution.attempt.factory.maxCandidates {
		return errCandidateLimit
	}
	return nil
}

func (execution *receiverExecution) acceptControl(control ReceiverControl) receiverWorkflowResult {
	if control == nil {
		return receiverWorkflowDiagnostic(ErrProtocol)
	}
	switch control.Kind() {
	case protocolsession.MessagePeerAnswer:
		return execution.acceptAnswer(control.Body())
	case protocolsession.MessagePeerCandidate:
		return execution.acceptRemoteCandidate(control.Body())
	default:
		return receiverWorkflowDiagnostic(ErrProtocol)
	}
}

func (execution *receiverExecution) acceptAnswer(body []byte) receiverWorkflowResult {
	if execution.answerSeen {
		return receiverWorkflowUnsafe(
			errors.New("sender returned more than one peer answer"),
			ReceiverProvenanceAuthenticatedSecondAnswer,
		)
	}
	answer, err := v2signal.DecodeAnswer(body)
	if err != nil {
		return receiverWorkflowDiagnostic(err)
	}
	if err := execution.attempt.binding.RequireSame(answer.Binding); err != nil {
		return receiverWorkflowUnsafe(
			err,
			ReceiverProvenanceAuthenticatedAnswerBindingMismatch,
		)
	}
	if err := execution.attempt.peer.SetRemoteDescription(pion.SessionDescription{
		Type: pion.SDPTypeAnswer, SDP: answer.SDP,
	}); err != nil {
		return receiverWorkflowDiagnostic(fmt.Errorf("set remote answer: %w", err))
	}
	execution.answerSeen = true
	for _, candidate := range execution.queuedCandidates {
		if err := execution.attempt.peer.AddICECandidate(candidateInit(candidate)); err != nil {
			return receiverWorkflowDiagnostic(fmt.Errorf("add queued remote ICE candidate: %w", err))
		}
	}
	execution.queuedCandidates = nil
	return receiverWorkflowResult{}
}

func (execution *receiverExecution) startAttachment() error {
	if execution.channelOpened {
		return nil
	}
	if execution.attachStarted {
		return errors.Join(errChannelAdmission, errors.New("peer DataChannel admission started twice"))
	}
	route, err := selectedPeerRoute(execution.attempt.peer)
	if err != nil {
		return err
	}
	phaseContext, transitioned, err := execution.attempt.phases.beginAdmission(execution.attempt.ctx)
	if err != nil {
		return err
	}
	if !transitioned {
		return nil
	}
	execution.channelOpened = true
	execution.attachStarted = true
	execution.attempt.phaseContext = phaseContext
	execution.children.Go(func() {
		grant, err := execution.attempt.lanes.RequestLane(phaseContext, 0)
		admission := sessionruntime.ReceiverLaneAdmissionResult{
			Disposition:      sessionruntime.ReceiverLaneAdmissionUnverified,
			LaneInstallation: sessionruntime.ReceiverLaneInstallationNotAttempted,
		}
		if err == nil {
			if !execution.attempt.transport.consume() {
				err = ErrProtocol
			} else {
				admission, err = execution.attempt.lanes.AttachLane(
					phaseContext, grant, execution.attempt.transport, route,
				)
			}
		}
		execution.resolveAttachment(receiverEvent{
			kind: receiverAttached, grant: grant, admission: admission, err: err,
		})
	})
	return nil
}

func (execution *receiverExecution) resolveAttachment(event receiverEvent) {
	execution.attachmentMu.Lock()
	execution.attachmentResult = event
	close(execution.attachmentDone)
	execution.attachmentMu.Unlock()
	execution.attempt.push(event)
}

func (execution *receiverExecution) finishAttachment(
	event receiverEvent,
) (bool, bool, receiverWorkflowResult) {
	settlement, err := receiverAttachmentSettlement(event.grant, event.admission, event.err)
	admitted := settlement == receiverAdmissionInstalled
	if !execution.attempt.phases.settleReceiverAdmission(settlement) {
		return false, false, receiverWorkflowResult{}
	}
	if !admitted {
		result := receiverWorkflowResult{cause: errors.Join(errChannelAdmission, err)}
		if settlement == receiverAdmissionAcceptedInstallationFailed {
			result.decision = receiverOperationDecision(
				ReceiverTerminalLocal,
				ReceiverProvenanceLocalOperationContract,
			)
		}
		return true, false, result
	}
	execution.attempt.resultMu.Lock()
	execution.attempt.lane = event.admission.Lane
	execution.attempt.resultMu.Unlock()
	close(execution.attempt.ready)
	return true, true, receiverWorkflowResult{}
}

func (execution *receiverExecution) settleAttachmentAtTerminal(
	cause error,
) (bool, bool, receiverWorkflowResult) {
	if !execution.attachStarted {
		execution.attempt.phases.terminate(cause)
		return false, false, receiverWorkflowResult{}
	}
	execution.attempt.phases.terminate(cause)
	<-execution.attachmentDone
	execution.attachmentMu.Lock()
	event := execution.attachmentResult
	execution.attachmentMu.Unlock()
	settled, admitted, result := execution.finishAttachment(event)
	return settled, admitted, result
}

func (execution *receiverExecution) reconcileOpen() (bool, error) {
	if execution.channelOpened {
		return true, nil
	}
	select {
	case <-execution.attempt.channel.Opened():
		if err := execution.startAttachment(); err != nil {
			return false, err
		}
		return true, nil
	default:
		return false, nil
	}
}

func (execution *receiverExecution) channelClosed(channelErr error) error {
	if execution.attempt.ctx.Err() != nil {
		return context.Cause(execution.attempt.ctx)
	}
	if execution.attempt.phases.admitted() {
		return nil
	}
	return errors.Join(errChannelAdmission, channelErr, errors.New("peer DataChannel closed"))
}
