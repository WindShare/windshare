package v2peer

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/windshare/windshare/core/session/protocolsession"
)

// receiverBoundSignalingOperation keeps lifecycle authority on one exact core
// operation object. OperationID remains correlation data; it is never used to
// resolve cleanup after the same wire identity is admitted again.
type receiverBoundSignalingOperation struct {
	operation       ReceiverSignalingOperation
	operationID     protocolsession.OperationID
	localGeneration uint64
	binding         ReceiverSignalingOperationBinding

	terminateOnce sync.Once
	terminateDone chan struct{}

	terminationMu        sync.Mutex
	termination          ReceiverSignalingTermination
	terminationSet       bool
	receiving            bool
	terminationRequested bool
	terminationCallDone  bool
	terminationPublished bool
}

var receiverSignalingGenerations atomic.Uint64

func newReceiverSignalingOperationBinding() ReceiverSignalingOperationBinding {
	generation := receiverSignalingGenerations.Add(1)
	if generation == 0 {
		generation = receiverSignalingGenerations.Add(1)
	}
	return ReceiverSignalingOperationBinding{
		token:           &receiverSignalingOperationToken{marker: 1},
		localGeneration: generation,
	}
}

func (operation *receiverBoundSignalingOperation) receive(
	ctx context.Context,
) ReceiverSignalingReceiveResult {
	if operation == nil || operation.operation == nil {
		return ReceiverSignalingReceiveResult{}
	}
	operation.terminationMu.Lock()
	if operation.terminationRequested {
		done := operation.terminateDone
		operation.terminationMu.Unlock()
		<-done
		return NewReceiverSignalingTerminationResult(operation.terminationResult())
	}
	operation.receiving = true
	operation.terminationMu.Unlock()

	result := operation.operation.Receive(ctx)
	control, hasControl := result.Control()
	termination, hasTermination := result.Termination()

	operation.terminationMu.Lock()
	if hasTermination {
		operation.mergeTerminationLocked(termination)
	} else if !hasControl {
		operation.mergeTerminationLocked(receiverSignalingAdapterFailure(operation.binding, nil))
	}
	operation.receiving = false
	operation.publishTerminationLocked()
	terminationRequested := operation.terminationRequested
	current := operation.currentTerminationLocked()
	operation.terminationMu.Unlock()

	if terminationRequested {
		<-operation.terminateDone
		return NewReceiverSignalingTerminationResult(operation.terminationResult())
	}
	if hasTermination || !hasControl {
		return NewReceiverSignalingTerminationResult(current)
	}
	return NewReceiverSignalingControlResult(control)
}

func (operation *receiverBoundSignalingOperation) recordAdapterFailure(cause error) {
	if operation == nil {
		return
	}
	operation.terminationMu.Lock()
	operation.mergeTerminationLocked(receiverSignalingAdapterFailure(operation.binding, cause))
	operation.terminationMu.Unlock()
}

func (operation *receiverBoundSignalingOperation) terminateExact() ReceiverSignalingTermination {
	if operation == nil || operation.operation == nil {
		return ReceiverSignalingTermination{}
	}
	operation.terminateOnce.Do(func() {
		operation.terminationMu.Lock()
		operation.terminationRequested = true
		operation.terminationMu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), failureDeliveryTimeout)
		termination := operation.operation.Terminate(ctx)
		cancel()

		operation.terminationMu.Lock()
		// Terminate is a join input, not replacement authority. Waiting for an
		// already-running Receive before publication prevents scheduler order from
		// hiding a stronger exact-operation consequence that Receive already proved.
		operation.mergeTerminationLocked(termination)
		operation.terminationCallDone = true
		operation.publishTerminationLocked()
		operation.terminationMu.Unlock()
	})
	<-operation.terminateDone
	return operation.terminationResult()
}

func (operation *receiverBoundSignalingOperation) mergeTerminationLocked(
	candidate ReceiverSignalingTermination,
) {
	if operation.terminationPublished {
		return
	}
	if !candidate.ownedBy(operation.binding) {
		candidate = receiverSignalingAdapterFailure(operation.binding, nil)
	}
	if !operation.terminationSet {
		operation.termination = candidate
		operation.terminationSet = true
		return
	}
	operation.termination.decision = mergeReceiverAttemptDecisions(
		operation.termination.decision,
		candidate.decision,
	)
	operation.termination.diagnostics = joinReceiverResiduals([]error{
		operation.termination.diagnostics,
		candidate.diagnostics,
	})
	operation.termination.diagnosticsTruncated =
		operation.termination.diagnosticsTruncated || candidate.diagnosticsTruncated
}

func (operation *receiverBoundSignalingOperation) publishTerminationLocked() {
	if operation.terminationPublished || !operation.terminationCallDone || operation.receiving {
		return
	}
	if !operation.terminationSet {
		operation.mergeTerminationLocked(receiverSignalingAdapterFailure(operation.binding, nil))
	}
	operation.terminationPublished = true
	close(operation.terminateDone)
}

func (operation *receiverBoundSignalingOperation) currentTerminationLocked() ReceiverSignalingTermination {
	if operation == nil || !operation.terminationSet {
		return ReceiverSignalingTermination{}
	}
	return operation.termination
}

func (operation *receiverBoundSignalingOperation) terminationResult() ReceiverSignalingTermination {
	if operation == nil {
		return ReceiverSignalingTermination{}
	}
	operation.terminationMu.Lock()
	defer operation.terminationMu.Unlock()
	if !operation.terminationSet {
		return ReceiverSignalingTermination{}
	}
	return operation.termination
}

func newReceiverBoundSignalingOperation(
	operation ReceiverSignalingOperation,
	operationID protocolsession.OperationID,
	binding ReceiverSignalingOperationBinding,
) *receiverBoundSignalingOperation {
	return &receiverBoundSignalingOperation{
		operation: operation, operationID: operationID,
		localGeneration: binding.localGeneration, binding: binding,
		terminateDone: make(chan struct{}),
	}
}

func (attempt *ReceiverAttempt) bindOperation(
	bound *receiverBoundSignalingOperation,
) bool {
	attempt.signalingMu.Lock()
	attempt.operation = bound
	shutdownRequested := attempt.shutdownRequested
	attempt.signalingMu.Unlock()
	return !shutdownRequested
}

func (attempt *ReceiverAttempt) requestShutdown() {
	attempt.signalingMu.Lock()
	attempt.shutdownRequested = true
	operation := attempt.operation
	if operation == nil && attempt.shutdownDecision.transitionOwner == "" {
		attempt.shutdownDecision = receiverOperationDecision(
			ReceiverTerminalLocal,
			ReceiverProvenanceLocalExplicitStop,
		)
	}
	attempt.signalingMu.Unlock()
	if operation == nil {
		attempt.cancel(context.Canceled)
		return
	}
	termination := operation.terminateExact()
	if !termination.ownedBy(operation.binding) {
		attempt.cancel(ErrProtocol)
		return
	}
	switch termination.decision.transitionOwner {
	case ReceiverTerminalLocal:
		attempt.cancel(context.Canceled)
	case ReceiverTerminalUnbound:
		attempt.cancel(ErrProtocol)
	}
}

func (attempt *ReceiverAttempt) preOperationCompletionDecision(
	cause error,
) receiverAttemptDecision {
	attempt.signalingMu.Lock()
	shutdownDecision := attempt.shutdownDecision
	attempt.signalingMu.Unlock()
	if shutdownDecision.transitionOwner != "" {
		return shutdownDecision
	}
	if errors.Is(cause, errAttemptTimeout) {
		return receiverOperationDecision(
			ReceiverTerminalLocal,
			ReceiverProvenanceLocalAttemptTimeout,
		)
	}
	return receiverOperationDecision(
		ReceiverTerminalLocal,
		ReceiverProvenanceLocalContextEnded,
	)
}
