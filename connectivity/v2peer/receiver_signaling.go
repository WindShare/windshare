package v2peer

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

type receiverSignalingOperationToken struct{ marker byte }

// ReceiverSignalingOperationBinding is an opaque, attempt-issued capability.
// Adapters receive it before Open so every later terminal can retain the exact
// local operation identity instead of relying on the reusable wire ID.
type ReceiverSignalingOperationBinding struct {
	token           *receiverSignalingOperationToken
	localGeneration uint64
}

func (binding ReceiverSignalingOperationBinding) valid() bool {
	return binding.token != nil && binding.localGeneration != 0
}

type ReceiverSignalingOperation interface {
	OperationID() protocolsession.OperationID
	MaximumContinuations() (int, bool)
	SendCandidate(context.Context, []byte) (protocolsession.OperationDisposition, error)
	Receive(context.Context) ReceiverSignalingReceiveResult
	Terminate(context.Context) ReceiverSignalingTermination
}

type ReceiverControl interface {
	Kind() protocolsession.MessageKind
	Body() []byte
}

type receiverSignalingReceiveResultKind uint8

const (
	receiverSignalingReceiveInvalid receiverSignalingReceiveResultKind = iota
	receiverSignalingReceiveControl
	receiverSignalingReceiveTermination
)

// ReceiverSignalingReceiveResult keeps the test seam mutually exclusive. Its
// public constructors cannot create a session-unsafe consequence; only the
// production exact-core validation boundary can import that authority.
type ReceiverSignalingReceiveResult struct {
	kind        receiverSignalingReceiveResultKind
	control     ReceiverControl
	termination ReceiverSignalingTermination
}

func NewReceiverSignalingControlResult(control ReceiverControl) ReceiverSignalingReceiveResult {
	if control == nil {
		return ReceiverSignalingReceiveResult{}
	}
	return ReceiverSignalingReceiveResult{kind: receiverSignalingReceiveControl, control: control}
}

func NewReceiverSignalingTerminationResult(
	termination ReceiverSignalingTermination,
) ReceiverSignalingReceiveResult {
	if !termination.valid {
		return ReceiverSignalingReceiveResult{}
	}
	return ReceiverSignalingReceiveResult{
		kind: receiverSignalingReceiveTermination, termination: termination,
	}
}

func (result ReceiverSignalingReceiveResult) Control() (ReceiverControl, bool) {
	return result.control, result.kind == receiverSignalingReceiveControl
}

func (result ReceiverSignalingReceiveResult) Termination() (ReceiverSignalingTermination, bool) {
	return result.termination, result.kind == receiverSignalingReceiveTermination
}

type ReceiverSignalingTermination struct {
	operationToken       *receiverSignalingOperationToken
	valid                bool
	decision             receiverAttemptDecision
	diagnostics          error
	diagnosticsTruncated bool
}

func (termination ReceiverSignalingTermination) ownedBy(
	binding ReceiverSignalingOperationBinding,
) bool {
	return binding.valid() && termination.valid &&
		termination.operationToken == binding.token &&
		validReceiverBoundDecision(termination.decision)
}

// NewReceiverSignalingLocalTermination supports consumer-side test adapters
// without allowing them to mint authenticated session authority.
func NewReceiverSignalingLocalTermination(
	binding ReceiverSignalingOperationBinding,
	cause error,
) ReceiverSignalingTermination {
	if !binding.valid() {
		return ReceiverSignalingTermination{}
	}
	diagnostics, truncated := snapshotReceiverCauseWithStatus(cause)
	return ReceiverSignalingTermination{
		operationToken: binding.token,
		valid:          true,
		decision: receiverOperationDecision(
			ReceiverTerminalLocal,
			ReceiverProvenanceLocalExplicitStop,
		),
		diagnostics: diagnostics, diagnosticsTruncated: truncated,
	}
}

// NewReceiverSignalingRemoteTermination represents an ordinary remote attempt
// rejection for non-production test adapters. Unsafe remote facts are sealed at
// the core adapter boundary and have no public constructor.
func NewReceiverSignalingRemoteTermination(
	binding ReceiverSignalingOperationBinding,
	cause error,
) ReceiverSignalingTermination {
	if !binding.valid() {
		return ReceiverSignalingTermination{}
	}
	diagnostics, truncated := snapshotReceiverCauseWithStatus(cause)
	return ReceiverSignalingTermination{
		operationToken: binding.token,
		valid:          true,
		decision: receiverOperationDecision(
			ReceiverTerminalRemote,
			ReceiverProvenanceRemoteOperationRejected,
		),
		diagnostics: diagnostics, diagnosticsTruncated: truncated,
	}
}

type ReceiverSignaling interface {
	OpenPeerOperation(
		context.Context,
		ReceiverSignalingOperationBinding,
		[]byte,
	) (ReceiverSignalingOperation, error)
}

type receiverSignalingOpenFailure struct {
	operationToken       *receiverSignalingOperationToken
	decision             receiverAttemptDecision
	diagnostics          error
	diagnosticsTruncated bool
}

func (failure *receiverSignalingOpenFailure) Error() string {
	if failure == nil || failure.diagnostics == nil {
		return "receiver signaling open failed"
	}
	return failure.diagnostics.Error()
}

func (failure *receiverSignalingOpenFailure) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.diagnostics
}

func (failure *receiverSignalingOpenFailure) ownedBy(
	binding ReceiverSignalingOperationBinding,
) bool {
	return failure != nil && binding.valid() && failure.operationToken == binding.token &&
		validReceiverBoundDecision(failure.decision)
}

func newReceiverSignalingOpenFailure(
	binding ReceiverSignalingOperationBinding,
	decision receiverAttemptDecision,
	cause error,
) error {
	diagnostics, truncated := snapshotReceiverCauseWithStatus(cause)
	return &receiverSignalingOpenFailure{
		operationToken: binding.token,
		decision:       decision,
		diagnostics:    diagnostics, diagnosticsTruncated: truncated,
	}
}

type RuntimeReceiverSignaling struct {
	runtime *sessionruntime.ReceiverRuntime
}

func NewRuntimeReceiverSignaling(runtime *sessionruntime.ReceiverRuntime) (*RuntimeReceiverSignaling, error) {
	if runtime == nil {
		return nil, ErrConfig
	}
	return &RuntimeReceiverSignaling{runtime: runtime}, nil
}

func (signaling *RuntimeReceiverSignaling) OpenPeerOperation(
	ctx context.Context,
	binding ReceiverSignalingOperationBinding,
	offer []byte,
) (ReceiverSignalingOperation, error) {
	if signaling == nil || signaling.runtime == nil || !binding.valid() {
		return nil, ErrConfig
	}
	operation, err := signaling.runtime.OpenPeerOperation(ctx, offer)
	if err != nil {
		decision := receiverOperationDecision(
			ReceiverTerminalLocal,
			ReceiverProvenanceLocalNegotiationFailure,
		)
		if signaling.runtime.Stopping() {
			// Runtime lifecycle is structural producer evidence. Seal it here so
			// neither adapter error graphs nor bounded diagnostics control admission.
			decision = receiverUnavailableDecision()
		} else if ctx != nil && ctx.Err() != nil {
			decision = receiverOperationDecision(
				ReceiverTerminalLocal,
				ReceiverProvenanceLocalContextEnded,
			)
		}
		return nil, newReceiverSignalingOpenFailure(binding, decision, err)
	}
	return runtimeReceiverOperation{operation: operation, binding: binding}, nil
}

type runtimeReceiverOperation struct {
	operation *sessionruntime.ReceiverPeerOperation
	binding   ReceiverSignalingOperationBinding
}

func (operation runtimeReceiverOperation) OperationID() protocolsession.OperationID {
	return operation.operation.OperationID()
}

func (operation runtimeReceiverOperation) MaximumContinuations() (int, bool) {
	return operation.operation.MaximumContinuations()
}

func (operation runtimeReceiverOperation) SendCandidate(
	ctx context.Context,
	body []byte,
) (protocolsession.OperationDisposition, error) {
	return operation.operation.SendCandidate(ctx, body)
}

func (operation runtimeReceiverOperation) Receive(ctx context.Context) ReceiverSignalingReceiveResult {
	result := operation.operation.Receive(ctx)
	if terminal, ok := result.Termination(); ok {
		return NewReceiverSignalingTerminationResult(
			receiverSignalingTerminationFromCore(operation.binding, operation.operation, terminal),
		)
	}
	if control, ok := result.Control(); ok {
		return NewReceiverSignalingControlResult(control)
	}
	return NewReceiverSignalingTerminationResult(receiverSignalingAdapterFailure(operation.binding, nil))
}

func (operation runtimeReceiverOperation) Terminate(
	ctx context.Context,
) ReceiverSignalingTermination {
	return receiverSignalingTerminationFromCore(
		operation.binding,
		operation.operation,
		operation.operation.Terminate(ctx),
	)
}

func receiverSignalingTerminationFromCore(
	binding ReceiverSignalingOperationBinding,
	operation *sessionruntime.ReceiverPeerOperation,
	termination sessionruntime.ReceiverPeerTermination,
) ReceiverSignalingTermination {
	if !binding.valid() || operation == nil || !operation.OwnsTermination(termination) {
		return receiverSignalingAdapterFailure(binding, nil)
	}
	decision, ok := receiverDecisionFromCoreTermination(termination)
	if !ok {
		return receiverSignalingAdapterFailure(binding, nil)
	}
	diagnostics, truncated := receiverCoreTerminalDiagnostics(termination.Diagnostics())
	return ReceiverSignalingTermination{
		operationToken: binding.token,
		valid:          true, decision: decision,
		diagnostics: diagnostics, diagnosticsTruncated: truncated,
	}
}

func receiverSignalingAdapterFailure(
	binding ReceiverSignalingOperationBinding,
	cause error,
) ReceiverSignalingTermination {
	if !binding.valid() {
		return ReceiverSignalingTermination{}
	}
	diagnostics, truncated := snapshotReceiverCauseWithStatus(errors.Join(ErrProtocol, cause))
	return ReceiverSignalingTermination{
		operationToken: binding.token,
		valid:          true,
		decision: receiverOperationDecision(
			ReceiverTerminalLocal,
			ReceiverProvenanceSignalingAdapterContract,
		),
		diagnostics: diagnostics, diagnosticsTruncated: truncated,
	}
}

func receiverDecisionFromCoreTermination(
	termination sessionruntime.ReceiverPeerTermination,
) (receiverAttemptDecision, bool) {
	owner, ownerOK := receiverTerminalOwner(termination.Authority())
	transition, transitionOK := receiverProvenanceFromCore(termination.TransitionProvenance())
	consequence, consequenceOK := receiverProvenanceFromCore(termination.ConsequenceProvenance())
	disposition, dispositionOK := receiverDispositionFromCore(termination.Severity())
	if !ownerOK || !transitionOK || !consequenceOK || !dispositionOK {
		return receiverAttemptDecision{}, false
	}
	return receiverAttemptDecision{
		transitionOwner: owner, transitionProvenance: transition,
		disposition: disposition, consequenceProvenance: consequence,
	}, true
}

func receiverTerminalOwner(
	authority sessionruntime.ReceiverPeerTerminalAuthority,
) (ReceiverTerminalOwner, bool) {
	switch authority {
	case sessionruntime.ReceiverPeerTerminalAuthorityLocal:
		return ReceiverTerminalLocal, true
	case sessionruntime.ReceiverPeerTerminalAuthorityRemote:
		return ReceiverTerminalRemote, true
	case sessionruntime.ReceiverPeerTerminalAuthorityRuntime:
		return ReceiverTerminalRuntime, true
	default:
		return ReceiverTerminalUnbound, false
	}
}

func receiverDispositionFromCore(
	severity sessionruntime.ReceiverPeerTerminalSeverity,
) (ReceiverAttemptDisposition, bool) {
	switch severity {
	case sessionruntime.ReceiverPeerTerminalOperationOnly:
		return ReceiverDispositionFallbackAllowed, true
	case sessionruntime.ReceiverPeerTerminalSessionUnavailable:
		return ReceiverDispositionSessionUnavailable, true
	case sessionruntime.ReceiverPeerTerminalSessionUnsafe:
		return ReceiverDispositionSessionUnsafe, true
	default:
		return "", false
	}
}

func receiverProvenanceFromCore(
	provenance sessionruntime.ReceiverPeerTerminalProvenance,
) (ReceiverTerminalProvenance, bool) {
	switch provenance {
	case sessionruntime.ReceiverPeerProvenanceLocalExplicitStop:
		return ReceiverProvenanceLocalExplicitStop, true
	case sessionruntime.ReceiverPeerProvenanceLocalContextEnded:
		return ReceiverProvenanceLocalContextEnded, true
	case sessionruntime.ReceiverPeerProvenanceLocalOperationContract:
		return ReceiverProvenanceLocalOperationContract, true
	case sessionruntime.ReceiverPeerProvenanceRemoteOperationRejected:
		return ReceiverProvenanceRemoteOperationRejected, true
	case sessionruntime.ReceiverPeerProvenanceRemoteUnknownControl:
		return ReceiverProvenanceRemoteUnknownControl, true
	case sessionruntime.ReceiverPeerProvenanceRemoteControlMalformed:
		return ReceiverProvenanceRemoteControlMalformed, true
	case sessionruntime.ReceiverPeerProvenanceRemoteFailureMalformed:
		return ReceiverProvenanceRemoteFailureMalformed, true
	case sessionruntime.ReceiverPeerProvenanceRemoteFailureScopeViolation:
		return ReceiverProvenanceRemoteFailureScopeViolation, true
	case sessionruntime.ReceiverPeerProvenanceRemoteAnswerConflict:
		return ReceiverProvenanceAuthenticatedSecondAnswer, true
	case sessionruntime.ReceiverPeerProvenanceRemoteFinalConflict:
		return ReceiverProvenanceAuthenticatedFinalConflict, true
	case sessionruntime.ReceiverPeerProvenanceRemoteContinuationAuthorityViolation:
		return ReceiverProvenanceAuthenticatedContinuationAuthority, true
	case sessionruntime.ReceiverPeerProvenanceRuntimeStopping:
		return ReceiverProvenanceRuntimeStopping, true
	default:
		return ReceiverProvenanceUnbound, false
	}
}

func receiverCoreTerminalDiagnostics(
	snapshot sessionruntime.ReceiverPeerDiagnosticSnapshot,
) (error, bool) {
	causes := make([]error, 0, len(snapshot.Components())+1)
	for _, diagnostic := range snapshot.Components() {
		switch diagnostic.Code() {
		case sessionruntime.ReceiverPeerDiagnosticOpaqueFailure,
			sessionruntime.ReceiverPeerDiagnosticCleanupFailed:
			causes = append(causes, errReceiverOpaqueCause)
		case sessionruntime.ReceiverPeerDiagnosticContextCanceled:
			causes = append(causes, context.Canceled)
		case sessionruntime.ReceiverPeerDiagnosticOperationMissing:
			causes = append(causes, sessionruntime.ErrOperationMissing)
		case sessionruntime.ReceiverPeerDiagnosticRuntimeClosed:
			causes = append(causes, sessionruntime.ErrRuntimeClosed)
		case sessionruntime.ReceiverPeerDiagnosticOperationOverflow:
			causes = append(causes, sessionruntime.ErrOperationOverflow)
		case sessionruntime.ReceiverPeerDiagnosticUnknownControl:
			causes = append(causes, protocolsession.ErrUnknownMessageKind)
		case sessionruntime.ReceiverPeerDiagnosticControlMalformed:
			causes = append(causes, protocolsession.ErrControlSemantic)
		case sessionruntime.ReceiverPeerDiagnosticRemoteOperationRejected:
			if failure, ok := diagnostic.RemoteFailure(); ok {
				causes = append(causes, sessionruntime.NewRemoteOperationError(failure))
			}
		case sessionruntime.ReceiverPeerDiagnosticRemoteFailureMalformed:
			causes = append(causes, protocolsession.ErrInvalidOperationFailure)
		case sessionruntime.ReceiverPeerDiagnosticRemoteFailureScopeViolation:
			causes = append(causes, protocolsession.ErrInvalidOperationFailure)
			if failure, ok := diagnostic.RemoteFailure(); ok {
				causes = append(causes, sessionruntime.NewRemoteOperationError(failure))
			}
		case sessionruntime.ReceiverPeerDiagnosticRemoteAnswerConflict,
			sessionruntime.ReceiverPeerDiagnosticRemoteFinalConflict:
			causes = append(causes, protocolsession.ErrAuthenticatedOperationViolation)
		case sessionruntime.ReceiverPeerDiagnosticRemoteContinuationAuthorityViolation:
			causes = append(causes, protocolsession.ErrConflictingContinuation)
		case sessionruntime.ReceiverPeerDiagnosticTruncated:
			causes = append(causes, errReceiverOpaqueCause)
		default:
			causes = append(causes, errReceiverOpaqueCause)
		}
	}
	if snapshot.Truncated() {
		causes = append(causes, errReceiverOpaqueCause)
	}
	return joinReceiverResiduals(causes), snapshot.Truncated()
}
