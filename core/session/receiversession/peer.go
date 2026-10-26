package receiversession

import (
	"bytes"
	"context"

	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	"github.com/windshare/windshare/core/session/protocolsession"
	"reflect"
	"slices"
	"sync"
)

// PeerControl is an authenticated sender signaling value. The runtime
// exposes semantic bytes only after the lane-bound signature and the injected
// peer schema validator have both succeeded.
type PeerControl struct {
	kind protocolsession.MessageKind
	body []byte
}

func (control PeerControl) Kind() protocolsession.MessageKind { return control.kind }

func (control PeerControl) Body() []byte { return bytes.Clone(control.body) }

type PeerTerminalAuthority uint8

const (
	receiverPeerTerminalAuthorityInvalid PeerTerminalAuthority = iota
	PeerTerminalAuthorityLocal
	PeerTerminalAuthorityRemote
	PeerTerminalAuthorityRuntime
)

type PeerTerminalSeverity uint8

const (
	receiverPeerTerminalSeverityInvalid PeerTerminalSeverity = iota
	PeerTerminalOperationOnly
	PeerTerminalSessionUnavailable
	PeerTerminalSessionUnsafe
)

type PeerTerminalProvenance uint16

const (
	receiverPeerTerminalProvenanceInvalid PeerTerminalProvenance = iota
	PeerProvenanceLocalExplicitStop
	PeerProvenanceLocalContextEnded
	PeerProvenanceLocalOperationContract
	PeerProvenanceRemoteOperationRejected
	PeerProvenanceRemoteUnknownControl
	PeerProvenanceRemoteControlMalformed
	PeerProvenanceRemoteFailureMalformed
	PeerProvenanceRemoteFailureScopeViolation
	PeerProvenanceRemoteAnswerConflict
	PeerProvenanceRemoteFinalConflict
	PeerProvenanceRemoteContinuationViolation
	PeerProvenanceRuntimeStopping
	PeerProvenanceRemoteSessionRejected
)

type PeerDiagnosticCode uint16

const (
	PeerDiagnosticOpaqueFailure PeerDiagnosticCode = iota + 1
	PeerDiagnosticContextCanceled
	PeerDiagnosticOperationMissing
	PeerDiagnosticRuntimeClosed
	PeerDiagnosticOperationOverflow
	PeerDiagnosticUnknownControl
	PeerDiagnosticControlMalformed
	PeerDiagnosticRemoteOperationRejected
	PeerDiagnosticRemoteFailureMalformed
	PeerDiagnosticRemoteFailureScopeViolation
	PeerDiagnosticRemoteAnswerConflict
	PeerDiagnosticRemoteFinalConflict
	PeerDiagnosticRemoteContinuationViolation
	PeerDiagnosticCleanupFailed
	PeerDiagnosticTruncated
)

const maximumReceiverPeerDiagnostics = 16

type PeerDiagnostic struct {
	code      PeerDiagnosticCode
	remote    RemoteOperationFailureSnapshot
	hasRemote bool
}

func (diagnostic PeerDiagnostic) Code() PeerDiagnosticCode { return diagnostic.code }

func (diagnostic PeerDiagnostic) RemoteFailure() (
	RemoteOperationFailureSnapshot,
	bool,
) {
	return diagnostic.remote, diagnostic.hasRemote
}

type PeerDiagnosticSnapshot struct {
	components [maximumReceiverPeerDiagnostics]PeerDiagnostic
	count      uint8
	truncated  bool
}

func (snapshot PeerDiagnosticSnapshot) Components() []PeerDiagnostic {
	return append([]PeerDiagnostic(nil), snapshot.components[:snapshot.count]...)
}

func (snapshot PeerDiagnosticSnapshot) Truncated() bool { return snapshot.truncated }

// A non-zero-sized token is required because Go may coalesce pointers to
// distinct zero-sized values, which would destroy exact-operation identity.
type receiverPeerOperationToken byte

type receiverPeerTerminalTransition struct {
	authority  PeerTerminalAuthority
	provenance PeerTerminalProvenance
}

type receiverPeerTerminalConsequence struct {
	severity   PeerTerminalSeverity
	provenance PeerTerminalProvenance
}

// PeerTermination is a sealed value. Diagnostics cannot manufacture
// authority, and a termination is valid only for the exact operation token
// that published it after receive and cleanup joined.
type PeerTermination struct {
	operationToken *receiverPeerOperationToken
	transition     receiverPeerTerminalTransition
	consequence    receiverPeerTerminalConsequence
	diagnostics    PeerDiagnosticSnapshot
}

func (termination PeerTermination) Authority() PeerTerminalAuthority {
	return termination.transition.authority
}

func (termination PeerTermination) TransitionProvenance() PeerTerminalProvenance {
	return termination.transition.provenance
}

func (termination PeerTermination) Severity() PeerTerminalSeverity {
	return termination.consequence.severity
}

func (termination PeerTermination) ConsequenceProvenance() PeerTerminalProvenance {
	return termination.consequence.provenance
}

func (termination PeerTermination) Diagnostics() PeerDiagnosticSnapshot {
	return termination.diagnostics
}

type receiverPeerReceiveResultKind uint8

const (
	receiverPeerReceiveResultInvalid receiverPeerReceiveResultKind = iota
	receiverPeerReceiveResultControl
	receiverPeerReceiveResultTermination
)

// PeerReceiveResult is an immutable sum: invalid control/terminal
// combinations cannot be assembled with a public literal or mutable pointer.
type PeerReceiveResult struct {
	kind        receiverPeerReceiveResultKind
	control     PeerControl
	termination PeerTermination
}

func (result PeerReceiveResult) Control() (PeerControl, bool) {
	return result.control, result.kind == receiverPeerReceiveResultControl
}

func (result PeerReceiveResult) Termination() (PeerTermination, bool) {
	return result.termination, result.kind == receiverPeerReceiveResultTermination
}

// PeerOperation owns one long-lived PEER_OFFER operation. Answer and
// candidates are fragments; a remote final, explicit Terminate, or runtime
// shutdown ends the exact operation object.
type PeerOperation struct {
	rpc              requestClient
	id               protocolsession.OperationID
	call             *sessioncore.Call
	token            *receiverPeerOperationToken
	protocolTerminal *sessioncore.TerminalOwner

	maximumContinuations int
	hasContinuationLimit bool

	mu        sync.Mutex
	closed    bool
	receiving bool

	terminalTransition  receiverPeerTerminalTransition
	terminalConsequence receiverPeerTerminalConsequence
	terminalDiagnostics PeerDiagnosticSnapshot
	terminalDone        chan struct{}
	terminalCleanupDone bool
	terminalPublished   bool
}

type receiverPeerTerminalEvidence struct {
	transition  receiverPeerTerminalTransition
	consequence receiverPeerTerminalConsequence
	diagnostics PeerDiagnosticSnapshot
}

func newReceiverPeerTerminalEvidence(
	authority PeerTerminalAuthority,
	provenance PeerTerminalProvenance,
	severity PeerTerminalSeverity,
	diagnostics ...PeerDiagnostic,
) receiverPeerTerminalEvidence {
	evidence := receiverPeerTerminalEvidence{
		transition:  receiverPeerTerminalTransition{authority: authority, provenance: provenance},
		consequence: receiverPeerTerminalConsequence{severity: severity, provenance: provenance},
	}
	for _, diagnostic := range diagnostics {
		evidence.diagnostics.append(diagnostic)
	}
	return evidence
}

func receiverPeerDiagnostic(code PeerDiagnosticCode) PeerDiagnostic {
	return PeerDiagnostic{code: code}
}

func receiverPeerRemoteDiagnostic(
	code PeerDiagnosticCode,
	failure RemoteOperationFailureSnapshot,
) PeerDiagnostic {
	return PeerDiagnostic{code: code, remote: failure, hasRemote: true}
}

func (snapshot *PeerDiagnosticSnapshot) append(diagnostic PeerDiagnostic) {
	if diagnostic.code == 0 {
		return
	}
	if slices.Contains(snapshot.components[:snapshot.count], diagnostic) {
		return
	}
	if snapshot.truncated {
		return
	}
	if diagnostic.code == PeerDiagnosticTruncated ||
		int(snapshot.count) == len(snapshot.components)-1 {
		snapshot.components[snapshot.count] = receiverPeerDiagnostic(PeerDiagnosticTruncated)
		snapshot.count++
		snapshot.truncated = true
		return
	}
	snapshot.components[snapshot.count] = diagnostic
	snapshot.count++
}

func (snapshot *PeerDiagnosticSnapshot) merge(other PeerDiagnosticSnapshot) {
	for _, diagnostic := range other.components[:other.count] {
		snapshot.append(diagnostic)
	}
	if other.truncated {
		snapshot.append(receiverPeerDiagnostic(PeerDiagnosticTruncated))
	}
}

func strongerReceiverPeerTerminalSeverity(
	current PeerTerminalSeverity,
	candidate PeerTerminalSeverity,
) bool {
	switch candidate {
	case PeerTerminalSessionUnsafe:
		return current != PeerTerminalSessionUnsafe
	case PeerTerminalSessionUnavailable:
		return current == receiverPeerTerminalSeverityInvalid ||
			current == PeerTerminalOperationOnly
	case PeerTerminalOperationOnly:
		return current == receiverPeerTerminalSeverityInvalid
	default:
		return false
	}
}

func receiverPeerTerminalResult(terminal PeerTermination) PeerReceiveResult {
	return PeerReceiveResult{kind: receiverPeerReceiveResultTermination, termination: terminal}
}

func (operation *PeerOperation) classifyReceiveError(
	ctx context.Context,
	err error,
) receiverPeerTerminalEvidence {
	operation.mu.Lock()
	hasTransition := operation.terminalTransition.authority != receiverPeerTerminalAuthorityInvalid
	operation.mu.Unlock()
	runtimeTrigger := err
	if hasTransition && isExactReceiverPeerCause(err, sessioncontract.ErrOperationMissing) {
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
	if hasTransition && isExactReceiverPeerCause(err, sessioncontract.ErrOperationMissing) {
		// Closing the exact call is only the wakeup for an already-owned terminal
		// transition; it is not a second failure cause.
		return receiverPeerTerminalEvidence{}
	}
	if isExactReceiverPeerCause(err, sessioncontract.ErrRuntimeClosed) {
		return receiverPeerRuntimeEvidence(err)
	}
	if ctx != nil && ctx.Err() != nil && isExactReceiverPeerCause(err, ctx.Err()) {
		return receiverPeerLocalEvidence(PeerProvenanceLocalContextEnded, err)
	}
	return receiverPeerLocalEvidence(PeerProvenanceLocalOperationContract, err)
}

func (operation *PeerOperation) runtimeTerminalEvidence(
	trigger error,
) (receiverPeerTerminalEvidence, bool) {
	if operation == nil || operation.rpc == nil || operation.rpc.Runtime() == nil {
		return receiverPeerRuntimeEvidence(trigger), true
	}
	runtime := operation.rpc.Runtime()
	if runtime.Lifetime() == nil || runtime.Done() == nil {
		return receiverPeerRuntimeEvidence(trigger), true
	}
	if runtime.Lifetime().Err() != nil {
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
	provenance PeerTerminalProvenance,
	cause error,
) receiverPeerTerminalEvidence {
	evidence := newReceiverPeerTerminalEvidence(
		PeerTerminalAuthorityLocal,
		provenance,
		PeerTerminalOperationOnly,
	)
	evidence.diagnostics.append(receiverPeerDiagnosticForCause(cause, PeerDiagnosticOpaqueFailure))
	return evidence
}

func receiverPeerRuntimeEvidence(trigger error) receiverPeerTerminalEvidence {
	evidence := newReceiverPeerTerminalEvidence(
		PeerTerminalAuthorityRuntime,
		PeerProvenanceRuntimeStopping,
		PeerTerminalSessionUnavailable,
		receiverPeerDiagnostic(PeerDiagnosticRuntimeClosed),
	)
	evidence.diagnostics.append(receiverPeerDiagnosticForCause(trigger, PeerDiagnosticOpaqueFailure))
	return evidence
}

func receiverPeerRemoteFailureEvidence(
	message protocolsession.Message,
) receiverPeerTerminalEvidence {
	failure, err := decodeRemoteOperationFailure(message)
	if err != nil {
		return newReceiverPeerTerminalEvidence(
			PeerTerminalAuthorityRemote,
			PeerProvenanceRemoteFailureMalformed,
			PeerTerminalSessionUnsafe,
			receiverPeerDiagnostic(PeerDiagnosticRemoteFailureMalformed),
		)
	}
	if failure.Scope() != protocolsession.OperationScopePeer {
		return newReceiverPeerTerminalEvidence(
			PeerTerminalAuthorityRemote,
			PeerProvenanceRemoteFailureScopeViolation,
			PeerTerminalSessionUnsafe,
			receiverPeerRemoteDiagnostic(PeerDiagnosticRemoteFailureScopeViolation, failure),
		)
	}
	if protocolsession.PeerFailureScope(failure.Code()) == protocolsession.PeerFailureSessionTerminal {
		return newReceiverPeerTerminalEvidence(PeerTerminalAuthorityRemote, PeerProvenanceRemoteSessionRejected, PeerTerminalSessionUnsafe, receiverPeerRemoteDiagnostic(PeerDiagnosticRemoteOperationRejected, failure))
	}
	return newReceiverPeerTerminalEvidence(
		PeerTerminalAuthorityRemote,
		PeerProvenanceRemoteOperationRejected,
		PeerTerminalOperationOnly,
		receiverPeerRemoteDiagnostic(PeerDiagnosticRemoteOperationRejected, failure),
	)
}

func receiverPeerDiagnosticForCause(
	cause error,
	fallback PeerDiagnosticCode,
) PeerDiagnostic {
	switch {
	case cause == nil:
		return PeerDiagnostic{}
	case isExactReceiverPeerCause(cause, context.Canceled):
		return receiverPeerDiagnostic(PeerDiagnosticContextCanceled)
	case isExactReceiverPeerCause(cause, sessioncontract.ErrOperationMissing):
		return receiverPeerDiagnostic(PeerDiagnosticOperationMissing)
	case isExactReceiverPeerCause(cause, sessioncontract.ErrRuntimeClosed):
		return receiverPeerDiagnostic(PeerDiagnosticRuntimeClosed)
	case isExactReceiverPeerCause(cause, sessioncontract.ErrOperationOverflow):
		return receiverPeerDiagnostic(PeerDiagnosticOperationOverflow)
	case isExactReceiverPeerCause(cause, protocolsession.ErrUnknownMessageKind):
		return receiverPeerDiagnostic(PeerDiagnosticUnknownControl)
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
