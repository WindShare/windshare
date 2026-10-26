package v2peer

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/session/protocolsession"

	sessioncontract "github.com/windshare/windshare/core/session"
	receiversession "github.com/windshare/windshare/core/session/receiversession"
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
	runtime *receiversession.Runtime
}

func NewRuntimeReceiverSignaling(runtime *receiversession.Runtime) (*RuntimeReceiverSignaling, error) {
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
	operation *receiversession.PeerOperation
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
	operation *receiversession.PeerOperation,
	termination receiversession.PeerTermination,
) ReceiverSignalingTermination {
	if !binding.valid() || operation == nil || !operation.OwnsTermination(termination) {
		return receiverSignalingAdapterFailure(binding, nil)
	}
	decision, ok := receiverDecisionFromCoreTermination(termination)
	if !ok {
		return receiverSignalingAdapterFailure(binding, nil)
	}
	if decision.transitionProvenance == ReceiverProvenanceRemoteOperationRejected {
		decision.recoveryScope = protocolsession.PeerFailurePathTerminal
		for _, diagnostic := range termination.Diagnostics().Components() {
			if diagnostic.Code() == receiversession.PeerDiagnosticRemoteOperationRejected {
				if failure, ok := diagnostic.RemoteFailure(); ok {
					decision.recoveryScope = protocolsession.PeerFailureScope(failure.Code())
				}
			}
		}
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
	termination receiversession.PeerTermination,
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
	authority receiversession.PeerTerminalAuthority,
) (ReceiverTerminalOwner, bool) {
	switch authority {
	case receiversession.PeerTerminalAuthorityLocal:
		return ReceiverTerminalLocal, true
	case receiversession.PeerTerminalAuthorityRemote:
		return ReceiverTerminalRemote, true
	case receiversession.PeerTerminalAuthorityRuntime:
		return ReceiverTerminalRuntime, true
	default:
		return ReceiverTerminalUnbound, false
	}
}

func receiverDispositionFromCore(
	severity receiversession.PeerTerminalSeverity,
) (ReceiverAttemptDisposition, bool) {
	switch severity {
	case receiversession.PeerTerminalOperationOnly:
		return ReceiverDispositionFallbackAllowed, true
	case receiversession.PeerTerminalSessionUnavailable:
		return ReceiverDispositionSessionUnavailable, true
	case receiversession.PeerTerminalSessionUnsafe:
		return ReceiverDispositionSessionUnsafe, true
	default:
		return "", false
	}
}

func receiverProvenanceFromCore(
	provenance receiversession.PeerTerminalProvenance,
) (ReceiverTerminalProvenance, bool) {
	switch provenance {
	case receiversession.PeerProvenanceLocalExplicitStop:
		return ReceiverProvenanceLocalExplicitStop, true
	case receiversession.PeerProvenanceLocalContextEnded:
		return ReceiverProvenanceLocalContextEnded, true
	case receiversession.PeerProvenanceLocalOperationContract:
		return ReceiverProvenanceLocalOperationContract, true
	case receiversession.PeerProvenanceRemoteOperationRejected:
		return ReceiverProvenanceRemoteOperationRejected, true
	case receiversession.PeerProvenanceRemoteSessionRejected:
		return ReceiverProvenanceRemoteSessionRejected, true
	case receiversession.PeerProvenanceRemoteUnknownControl:
		return ReceiverProvenanceRemoteUnknownControl, true
	case receiversession.PeerProvenanceRemoteControlMalformed:
		return ReceiverProvenanceRemoteControlMalformed, true
	case receiversession.PeerProvenanceRemoteFailureMalformed:
		return ReceiverProvenanceRemoteFailureMalformed, true
	case receiversession.PeerProvenanceRemoteFailureScopeViolation:
		return ReceiverProvenanceRemoteFailureScopeViolation, true
	case receiversession.PeerProvenanceRemoteAnswerConflict:
		return ReceiverProvenanceAuthenticatedSecondAnswer, true
	case receiversession.PeerProvenanceRemoteFinalConflict:
		return ReceiverProvenanceAuthenticatedFinalConflict, true
	case receiversession.PeerProvenanceRemoteContinuationViolation:
		return ReceiverProvenanceAuthenticatedContinuationViolation, true
	case receiversession.PeerProvenanceRuntimeStopping:
		return ReceiverProvenanceRuntimeStopping, true
	default:
		return ReceiverProvenanceUnbound, false
	}
}

func receiverCoreTerminalDiagnostics(
	snapshot receiversession.PeerDiagnosticSnapshot,
) (error, bool) {
	causes := make([]error, 0, len(snapshot.Components())+1)
	for _, diagnostic := range snapshot.Components() {
		switch diagnostic.Code() {
		case receiversession.PeerDiagnosticOpaqueFailure,
			receiversession.PeerDiagnosticCleanupFailed:
			causes = append(causes, errReceiverOpaqueCause)
		case receiversession.PeerDiagnosticContextCanceled:
			causes = append(causes, context.Canceled)
		case receiversession.PeerDiagnosticOperationMissing:
			causes = append(causes, sessioncontract.ErrOperationMissing)
		case receiversession.PeerDiagnosticRuntimeClosed:
			causes = append(causes, sessioncontract.ErrRuntimeClosed)
		case receiversession.PeerDiagnosticOperationOverflow:
			causes = append(causes, sessioncontract.ErrOperationOverflow)
		case receiversession.PeerDiagnosticUnknownControl:
			causes = append(causes, protocolsession.ErrUnknownMessageKind)
		case receiversession.PeerDiagnosticControlMalformed:
			causes = append(causes, protocolsession.ErrControlSemantic)
		case receiversession.PeerDiagnosticRemoteOperationRejected:
			if failure, ok := diagnostic.RemoteFailure(); ok {
				causes = append(causes, receiversession.NewRemoteOperationError(failure))
			}
		case receiversession.PeerDiagnosticRemoteFailureMalformed:
			causes = append(causes, protocolsession.ErrInvalidOperationFailure)
		case receiversession.PeerDiagnosticRemoteFailureScopeViolation:
			causes = append(causes, protocolsession.ErrInvalidOperationFailure)
			if failure, ok := diagnostic.RemoteFailure(); ok {
				causes = append(causes, receiversession.NewRemoteOperationError(failure))
			}
		case receiversession.PeerDiagnosticRemoteAnswerConflict,
			receiversession.PeerDiagnosticRemoteFinalConflict:
			causes = append(causes, protocolsession.ErrAuthenticatedOperationViolation)
		case receiversession.PeerDiagnosticRemoteContinuationViolation:
			causes = append(causes, protocolsession.ErrConflictingContinuation)
		case receiversession.PeerDiagnosticTruncated:
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
