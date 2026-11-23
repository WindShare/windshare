package sendersession

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	"github.com/windshare/windshare/core/session/protocolsession"
)

// PeerAdmissionControl owns the attempt's one-shot transition from
// admission-waiting to admission-settling. Core calls it only after WS2A has
// authenticated and before it consumes/rejects a grant or sends signed evidence.
type PeerAdmissionControl interface {
	BeginAuthenticatedSettlement(protocolsession.OperationID, session.LaneIdentity) bool
}

type PeerAdmissionControlFunc func(protocolsession.OperationID, session.LaneIdentity) bool

func (function PeerAdmissionControlFunc) BeginAuthenticatedSettlement(
	operationID protocolsession.OperationID,
	lane session.LaneIdentity,
) bool {
	return function != nil && function(operationID, lane)
}

type PeerAdmissionDisposition uint8

const (
	PeerAdmissionSilentClose PeerAdmissionDisposition = iota + 1
	PeerAdmissionAccepted
	PeerAdmissionRejected
)

type PeerResponseDelivery uint8

const (
	PeerResponseNotAttempted PeerResponseDelivery = iota + 1
	PeerResponseDelivered
	PeerResponseDeliveryFailed
)

type PeerLaneAttachment uint8

const (
	PeerLaneAttachmentNotAttempted PeerLaneAttachment = iota + 1
	PeerLaneAttached
	PeerLaneAttachmentFailed
)

// PeerAdmissionResult is safe settlement evidence: it deliberately
// excludes WS2A/WS2B/WS2N bytes, nonces, proofs, traffic keys, and raw errors.
// The authenticated disposition remains authoritative when delivery or local
// attachment later fails.
type PeerAdmissionResult struct {
	SettlementBegan  bool
	GrantOperationID protocolsession.OperationID
	Lane             session.LaneIdentity
	Disposition      PeerAdmissionDisposition
	Rejection        protocolsession.LaneRejection
	ResponseDelivery PeerResponseDelivery
	LaneAttachment   PeerLaneAttachment
}

func silentSenderPeerAdmission() PeerAdmissionResult {
	return PeerAdmissionResult{
		Disposition:      PeerAdmissionSilentClose,
		ResponseDelivery: PeerResponseNotAttempted,
		LaneAttachment:   PeerLaneAttachmentNotAttempted,
	}
}

// PeerSession is the transport-neutral authority granted to one
// connectivity-owned peer handler. Operation-scoped signaling is restricted to
// answers and candidates. Session-scoped path controls are required so
// wrappers cannot silently disable path maintenance.
// AdmitPeerChannel admits only into the ProtocolSession that created this
// authority and consumes the candidate channel: core closes it exactly once on
// failure, while successful attachment transfers close authority to the lane.
type PeerSession interface {
	ShareInstance() catalog.ShareInstance
	ProtocolSessionID() protocolsession.ProtocolSessionID
	SendPeerControl(
		context.Context,
		protocolsession.Operation,
		protocolsession.MessageKind,
		[]byte,
	) (protocolsession.OperationDisposition, error)
	FailPeerOperation(context.Context, protocolsession.Operation, uint16, string) error
	AdmitPeerChannel(
		context.Context,
		protocolsession.FrameChannel,
		PeerAdmissionControl,
	) (PeerAdmissionResult, error)
	// RetireReplacedLane closes the predecessor only while this exact successor is usable.
	RetireReplacedLane(previous, successor session.LaneIdentity) bool

	// Passing nil removes the path-control callback for the handler's Run lifetime.
	SetPeerPathControlHandler(func(context.Context, []byte) error)
	SetTURNControlHandler(func(context.Context, []byte) error)
	SendPeerPathControl(context.Context, []byte) error
	SendTURNControl(context.Context, []byte) error
}

// PeerHandler owns provider policy, SDP/ICE interpretation, and physical
// PeerConnection lifetime outside core. Run must synchronously close all owned
// attempts before returning so ProtocolSession termination cannot leak peers.
type PeerHandler interface {
	protocolsession.MessageHandler
	Cancel(context.Context, protocolsession.Operation) error
	Run(context.Context) error
}

// PeerHandlerFactory creates one isolated signaling owner per
// ProtocolSession. Implementations must not start asynchronous work before Run.
type PeerHandlerFactory interface {
	protocolsession.OperationContinuationClassifier
	NewSenderPeerHandler(PeerSession) (PeerHandler, error)
}

type PeerHandlerFactoryFunc func(PeerSession) (PeerHandler, error)

func (function PeerHandlerFactoryFunc) NewSenderPeerHandler(
	peer PeerSession,
) (PeerHandler, error) {
	if function == nil {
		return nil, session.ErrRuntimeConfig
	}
	return function(peer)
}

func (function PeerHandlerFactoryFunc) BeginOperationContinuation(
	protocolsession.MessageKind,
	[]byte,
) (protocolsession.OperationContinuationContract, bool, error) {
	if function == nil {
		return nil, false, session.ErrRuntimeConfig
	}
	// This adapter is intended for handlers that reject or never receive peer
	// operations. Schema-owning production providers must supply a real authority.
	return nil, false, nil
}

func (function PeerHandlerFactoryFunc) ClassifyUnboundOperationContinuation(
	protocolsession.MessageKind,
	[]byte,
) (protocolsession.OperationContinuationScope, bool, error) {
	if function == nil {
		return protocolsession.OperationContinuationScope{}, false, session.ErrRuntimeConfig
	}
	return protocolsession.OperationContinuationScope{}, false, nil
}

type senderPeerSession struct {
	runtime  *Runtime
	outbound *sessioncore.Responses
}

func (peer senderPeerSession) ShareInstance() catalog.ShareInstance {
	if peer.runtime == nil {
		return catalog.ShareInstance{}
	}
	return peer.runtime.core.ShareInstance()
}

func (peer senderPeerSession) ProtocolSessionID() protocolsession.ProtocolSessionID {
	if peer.runtime == nil {
		return protocolsession.ProtocolSessionID{}
	}
	return peer.runtime.ProtocolSessionID()
}

func (peer senderPeerSession) SendPeerControl(
	ctx context.Context,
	op protocolsession.Operation,
	kind protocolsession.MessageKind,
	body []byte,
) (protocolsession.OperationDisposition, error) {
	if peer.runtime == nil || op.IsZero() ||
		(kind != protocolsession.MessagePeerAnswer && kind != protocolsession.MessagePeerCandidate) {
		return protocolsession.OperationDrop, session.ErrRuntimeConfig
	}
	err := peer.outbound.SendControl(ctx, op, kind, body)
	if errors.Is(err, protocolsession.ErrOperationSuppressed) {
		return protocolsession.OperationDrop, nil
	}
	if protocolsession.IsNotSent(err) {
		return protocolsession.OperationDrop, err
	}
	return protocolsession.OperationDeliver, err
}

func (peer senderPeerSession) AdmitPeerChannel(
	ctx context.Context,
	channel protocolsession.FrameChannel,
	control PeerAdmissionControl,
) (PeerAdmissionResult, error) {
	if peer.runtime == nil {
		if channel != nil {
			_ = channel.Close()
		}
		return silentSenderPeerAdmission(), session.ErrRuntimeClosed
	}
	return peer.runtime.AdmitPeerChannel(ctx, channel, control)
}

func (peer senderPeerSession) RetireReplacedLane(previous, successor session.LaneIdentity) bool {
	return peer.runtime != nil && peer.runtime.core.RetireLaneIfCovered(previous, &successor)
}

func (peer senderPeerSession) FailPeerOperation(
	ctx context.Context,
	op protocolsession.Operation,
	code uint16,
	message string,
) error {
	if peer.runtime == nil || op.IsZero() {
		return session.ErrRuntimeConfig
	}
	binding, ok := op.PeerAttemptBinding()
	if !ok {
		return session.ErrOperationMissing
	}
	return peer.outbound.SendOperationError(ctx, op, protocolsession.OperationFailure{
		Scope: protocolsession.OperationScopePeer, Code: code, Message: message, PeerAttempt: &binding,
	})
}
