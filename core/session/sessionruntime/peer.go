package sessionruntime

import (
	"bytes"
	"context"
	"slices"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/session/protocolsession"
)

// ReceiverPeerControl is an authenticated sender signaling value. The runtime
// exposes semantic bytes only after the lane-bound signature and the injected
// peer schema validator have both succeeded.
type ReceiverPeerControl struct {
	kind protocolsession.MessageKind
	body []byte
}

func (control ReceiverPeerControl) Kind() protocolsession.MessageKind { return control.kind }
func (control ReceiverPeerControl) Body() []byte                      { return bytes.Clone(control.body) }

type ReceiverPeerTerminalAuthority uint8

const (
	receiverPeerTerminalAuthorityInvalid ReceiverPeerTerminalAuthority = iota
	ReceiverPeerTerminalAuthorityLocal
	ReceiverPeerTerminalAuthorityRemote
	ReceiverPeerTerminalAuthorityRuntime
)

type ReceiverPeerTerminalSeverity uint8

const (
	receiverPeerTerminalSeverityInvalid ReceiverPeerTerminalSeverity = iota
	ReceiverPeerTerminalOperationOnly
	ReceiverPeerTerminalSessionUnavailable
	ReceiverPeerTerminalSessionUnsafe
)

type ReceiverPeerTerminalProvenance uint16

const (
	receiverPeerTerminalProvenanceInvalid ReceiverPeerTerminalProvenance = iota
	ReceiverPeerProvenanceLocalExplicitStop
	ReceiverPeerProvenanceLocalContextEnded
	ReceiverPeerProvenanceLocalOperationContract
	ReceiverPeerProvenanceRemoteOperationRejected
	ReceiverPeerProvenanceRemoteUnknownControl
	ReceiverPeerProvenanceRemoteControlMalformed
	ReceiverPeerProvenanceRemoteFailureMalformed
	ReceiverPeerProvenanceRemoteFailureScopeViolation
	ReceiverPeerProvenanceRemoteAnswerConflict
	ReceiverPeerProvenanceRemoteFinalConflict
	ReceiverPeerProvenanceRemoteContinuationAuthorityViolation
	ReceiverPeerProvenanceRuntimeStopping
)

type ReceiverPeerDiagnosticCode uint16

const (
	ReceiverPeerDiagnosticOpaqueFailure ReceiverPeerDiagnosticCode = iota + 1
	ReceiverPeerDiagnosticContextCanceled
	ReceiverPeerDiagnosticOperationMissing
	ReceiverPeerDiagnosticRuntimeClosed
	ReceiverPeerDiagnosticOperationOverflow
	ReceiverPeerDiagnosticUnknownControl
	ReceiverPeerDiagnosticControlMalformed
	ReceiverPeerDiagnosticRemoteOperationRejected
	ReceiverPeerDiagnosticRemoteFailureMalformed
	ReceiverPeerDiagnosticRemoteFailureScopeViolation
	ReceiverPeerDiagnosticRemoteAnswerConflict
	ReceiverPeerDiagnosticRemoteFinalConflict
	ReceiverPeerDiagnosticRemoteContinuationAuthorityViolation
	ReceiverPeerDiagnosticCleanupFailed
	ReceiverPeerDiagnosticTruncated
)

const maximumReceiverPeerDiagnostics = 16

type ReceiverPeerDiagnostic struct {
	code      ReceiverPeerDiagnosticCode
	remote    RemoteOperationFailureSnapshot
	hasRemote bool
}

func (diagnostic ReceiverPeerDiagnostic) Code() ReceiverPeerDiagnosticCode { return diagnostic.code }

func (diagnostic ReceiverPeerDiagnostic) RemoteFailure() (
	RemoteOperationFailureSnapshot,
	bool,
) {
	return diagnostic.remote, diagnostic.hasRemote
}

type ReceiverPeerDiagnosticSnapshot struct {
	components [maximumReceiverPeerDiagnostics]ReceiverPeerDiagnostic
	count      uint8
	truncated  bool
}

func (snapshot ReceiverPeerDiagnosticSnapshot) Components() []ReceiverPeerDiagnostic {
	return append([]ReceiverPeerDiagnostic(nil), snapshot.components[:snapshot.count]...)
}

func (snapshot ReceiverPeerDiagnosticSnapshot) Truncated() bool { return snapshot.truncated }

// A non-zero-sized token is required because Go may coalesce pointers to
// distinct zero-sized values, which would destroy exact-operation identity.
type receiverPeerOperationToken byte

type receiverPeerTerminalTransition struct {
	authority  ReceiverPeerTerminalAuthority
	provenance ReceiverPeerTerminalProvenance
}

type receiverPeerTerminalConsequence struct {
	severity   ReceiverPeerTerminalSeverity
	provenance ReceiverPeerTerminalProvenance
}

// ReceiverPeerTermination is a sealed value. Diagnostics cannot manufacture
// authority, and a termination is valid only for the exact operation token
// that published it after receive and cleanup joined.
type ReceiverPeerTermination struct {
	operationToken *receiverPeerOperationToken
	transition     receiverPeerTerminalTransition
	consequence    receiverPeerTerminalConsequence
	diagnostics    ReceiverPeerDiagnosticSnapshot
}

func (termination ReceiverPeerTermination) Authority() ReceiverPeerTerminalAuthority {
	return termination.transition.authority
}

func (termination ReceiverPeerTermination) TransitionProvenance() ReceiverPeerTerminalProvenance {
	return termination.transition.provenance
}

func (termination ReceiverPeerTermination) Severity() ReceiverPeerTerminalSeverity {
	return termination.consequence.severity
}

func (termination ReceiverPeerTermination) ConsequenceProvenance() ReceiverPeerTerminalProvenance {
	return termination.consequence.provenance
}

func (termination ReceiverPeerTermination) Diagnostics() ReceiverPeerDiagnosticSnapshot {
	return termination.diagnostics
}

type receiverPeerReceiveResultKind uint8

const (
	receiverPeerReceiveResultInvalid receiverPeerReceiveResultKind = iota
	receiverPeerReceiveResultControl
	receiverPeerReceiveResultTermination
)

// ReceiverPeerReceiveResult is an immutable sum: invalid control/terminal
// combinations cannot be assembled with a public literal or mutable pointer.
type ReceiverPeerReceiveResult struct {
	kind        receiverPeerReceiveResultKind
	control     ReceiverPeerControl
	termination ReceiverPeerTermination
}

func (result ReceiverPeerReceiveResult) Control() (ReceiverPeerControl, bool) {
	return result.control, result.kind == receiverPeerReceiveResultControl
}

func (result ReceiverPeerReceiveResult) Termination() (ReceiverPeerTermination, bool) {
	return result.termination, result.kind == receiverPeerReceiveResultTermination
}

// ReceiverPeerOperation owns one long-lived PEER_OFFER operation. Answer and
// candidates are fragments; a remote final, explicit Terminate, or runtime
// shutdown ends the exact operation object.
type ReceiverPeerOperation struct {
	rpc   *rpcClient
	id    protocolsession.OperationID
	call  *operationCall
	token *receiverPeerOperationToken

	maximumContinuations int
	hasContinuationLimit bool

	mu        sync.Mutex
	closed    bool
	receiving bool

	terminalTransition  receiverPeerTerminalTransition
	terminalConsequence receiverPeerTerminalConsequence
	terminalDiagnostics ReceiverPeerDiagnosticSnapshot
	terminalDone        chan struct{}
	terminalCleanupDone bool
	terminalPublished   bool
}

type receiverPeerTerminalEvidence struct {
	transition  receiverPeerTerminalTransition
	consequence receiverPeerTerminalConsequence
	diagnostics ReceiverPeerDiagnosticSnapshot
}

func newReceiverPeerTerminalEvidence(
	authority ReceiverPeerTerminalAuthority,
	provenance ReceiverPeerTerminalProvenance,
	severity ReceiverPeerTerminalSeverity,
	diagnostics ...ReceiverPeerDiagnostic,
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

func receiverPeerDiagnostic(code ReceiverPeerDiagnosticCode) ReceiverPeerDiagnostic {
	return ReceiverPeerDiagnostic{code: code}
}

func receiverPeerRemoteDiagnostic(
	code ReceiverPeerDiagnosticCode,
	failure RemoteOperationFailureSnapshot,
) ReceiverPeerDiagnostic {
	return ReceiverPeerDiagnostic{code: code, remote: failure, hasRemote: true}
}

func (snapshot *ReceiverPeerDiagnosticSnapshot) append(diagnostic ReceiverPeerDiagnostic) {
	if diagnostic.code == 0 {
		return
	}
	if slices.Contains(snapshot.components[:snapshot.count], diagnostic) {
		return
	}
	if snapshot.truncated {
		return
	}
	if diagnostic.code == ReceiverPeerDiagnosticTruncated ||
		int(snapshot.count) == len(snapshot.components)-1 {
		snapshot.components[snapshot.count] = receiverPeerDiagnostic(ReceiverPeerDiagnosticTruncated)
		snapshot.count++
		snapshot.truncated = true
		return
	}
	snapshot.components[snapshot.count] = diagnostic
	snapshot.count++
}

func (snapshot *ReceiverPeerDiagnosticSnapshot) merge(other ReceiverPeerDiagnosticSnapshot) {
	for _, diagnostic := range other.components[:other.count] {
		snapshot.append(diagnostic)
	}
	if other.truncated {
		snapshot.append(receiverPeerDiagnostic(ReceiverPeerDiagnosticTruncated))
	}
}

func strongerReceiverPeerTerminalSeverity(
	current ReceiverPeerTerminalSeverity,
	candidate ReceiverPeerTerminalSeverity,
) bool {
	switch candidate {
	case ReceiverPeerTerminalSessionUnsafe:
		return current != ReceiverPeerTerminalSessionUnsafe
	case ReceiverPeerTerminalSessionUnavailable:
		return current == receiverPeerTerminalSeverityInvalid ||
			current == ReceiverPeerTerminalOperationOnly
	case ReceiverPeerTerminalOperationOnly:
		return current == receiverPeerTerminalSeverityInvalid
	default:
		return false
	}
}

// SenderPeerSession is the transport-neutral authority granted to one
// connectivity-owned peer-signaling handler. The handler can emit only the two
// sender signaling controls and can admit a DataChannel only into the exact
// ProtocolSession that created it.
type SenderPeerSession interface {
	ShareInstance() catalog.ShareInstance
	ProtocolSessionID() protocolsession.ProtocolSessionID
	SendPeerControl(
		context.Context,
		protocolsession.MessageKind,
		protocolsession.OperationID,
		[]byte,
	) (protocolsession.OperationDisposition, error)
	FailPeerOperation(context.Context, protocolsession.OperationID, uint16, string) error
	AdmitPeerChannel(context.Context, protocolsession.FrameChannel) (LaneIdentity, error)
}

// SenderPeerHandler owns provider policy, SDP/ICE interpretation, and physical
// PeerConnection lifetime outside core. Run must synchronously close all owned
// attempts before returning so ProtocolSession termination cannot leak peers.
type SenderPeerHandler interface {
	protocolsession.MessageHandler
	Cancel(context.Context, protocolsession.OperationID) error
	Run(context.Context) error
}

// SenderPeerHandlerFactory creates one isolated signaling owner per
// ProtocolSession. Implementations must not start asynchronous work before Run.
type SenderPeerHandlerFactory interface {
	protocolsession.OperationContinuationClassifier
	NewSenderPeerHandler(SenderPeerSession) (SenderPeerHandler, error)
}

type SenderPeerHandlerFactoryFunc func(SenderPeerSession) (SenderPeerHandler, error)

func (function SenderPeerHandlerFactoryFunc) NewSenderPeerHandler(
	session SenderPeerSession,
) (SenderPeerHandler, error) {
	if function == nil {
		return nil, ErrRuntimeConfig
	}
	return function(session)
}

func (function SenderPeerHandlerFactoryFunc) BeginOperationContinuation(
	protocolsession.MessageKind,
	[]byte,
) (protocolsession.OperationContinuationAuthority, bool, error) {
	if function == nil {
		return nil, false, ErrRuntimeConfig
	}
	// This adapter is intended for handlers that reject or never receive peer
	// operations. Schema-owning production providers must supply a real authority.
	return nil, false, nil
}

func (function SenderPeerHandlerFactoryFunc) ClassifyUnboundOperationContinuation(
	protocolsession.MessageKind,
	[]byte,
) (protocolsession.OperationContinuationScope, bool, error) {
	if function == nil {
		return protocolsession.OperationContinuationScope{}, false, ErrRuntimeConfig
	}
	return protocolsession.OperationContinuationScope{}, false, nil
}

type senderPeerSession struct {
	runtime  *SenderRuntime
	outbound senderOutbound
}

func (session senderPeerSession) ShareInstance() catalog.ShareInstance {
	if session.runtime == nil {
		return catalog.ShareInstance{}
	}
	return session.runtime.share
}

func (session senderPeerSession) ProtocolSessionID() protocolsession.ProtocolSessionID {
	if session.runtime == nil {
		return protocolsession.ProtocolSessionID{}
	}
	return session.runtime.ProtocolSessionID()
}

func (session senderPeerSession) SendPeerControl(
	ctx context.Context,
	kind protocolsession.MessageKind,
	operationID protocolsession.OperationID,
	body []byte,
) (protocolsession.OperationDisposition, error) {
	if session.runtime == nil || operationID.IsZero() ||
		(kind != protocolsession.MessagePeerAnswer && kind != protocolsession.MessagePeerCandidate) {
		return protocolsession.OperationDrop, ErrRuntimeConfig
	}
	outcome, err := session.outbound.SendControl(ctx, kind, operationID, body)
	if outcome == protocolsession.SendOutcomeDropped {
		return protocolsession.OperationDrop, err
	}
	return protocolsession.OperationDeliver, err
}

func (session senderPeerSession) AdmitPeerChannel(
	ctx context.Context,
	channel protocolsession.FrameChannel,
) (LaneIdentity, error) {
	if session.runtime == nil {
		return LaneIdentity{}, ErrRuntimeClosed
	}
	return session.runtime.AdmitPeerChannel(ctx, channel)
}

func (session senderPeerSession) FailPeerOperation(
	ctx context.Context,
	operationID protocolsession.OperationID,
	code uint16,
	message string,
) error {
	if session.runtime == nil || operationID.IsZero() {
		return ErrRuntimeConfig
	}
	return session.outbound.SendOperationError(ctx, operationID, protocolsession.OperationFailure{
		Scope: protocolsession.OperationScopePeer, Code: code, Message: message,
	})
}
