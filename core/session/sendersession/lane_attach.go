package sendersession

import (
	"context"
	"errors"
	"fmt"
	"github.com/windshare/windshare/core/framechannel"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	"github.com/windshare/windshare/core/session/protocolsession"
	"io"
	"sync"
)

type laneGrantHandler struct {
	registry *protocolsession.LaneRegistry
	outbound *sessioncore.Responses
	random   io.Reader
	queue    chan laneGrantOperation
	queueMu  sync.Mutex
	stopping bool
}

type laneGrantOperation struct {
	ctx     context.Context
	message protocolsession.Message
}

func newLaneGrantHandler(
	registry *protocolsession.LaneRegistry,
	outbound *sessioncore.Responses,
	random io.Reader,
) *laneGrantHandler {
	return &laneGrantHandler{
		registry: registry, outbound: outbound, random: random,
		queue: make(chan laneGrantOperation, sessioncore.LaneGrantQueueLimit),
	}
}

func (handler *laneGrantHandler) HandleMessage(ctx context.Context, message protocolsession.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if message.Kind() != protocolsession.MessageLaneAttach {
		return sessioncontract.ErrRuntimeConfig
	}
	operationID, ok := message.OperationID()
	if !ok {
		return sessioncontract.ErrOperationMissing
	}
	generation, ok := protocolsession.OperationGenerationFromContext(ctx, operationID)
	if !ok || generation.IsZero() {
		return sessioncontract.ErrOperationMissing
	}
	if !generation.IsActive() {
		return nil
	}
	handler.queueMu.Lock()
	defer handler.queueMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if handler.stopping {
		return sessioncontract.ErrRuntimeClosed
	}
	select {
	case handler.queue <- laneGrantOperation{ctx: ctx, message: message}:
		return nil
	default:
		return sessioncontract.ErrOperationOverflow
	}
}

func (handler *laneGrantHandler) Run(ctx context.Context) error {
	defer handler.stopAndDrain()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case queued := <-handler.queue:
			operationContext := protocolsession.RetainMessageContext(ctx, queued.ctx)
			operationID, _ := queued.message.OperationID()
			generation, ok := protocolsession.OperationGenerationFromContext(operationContext, operationID)
			if !ok || !generation.IsActive() {
				continue
			}
			if err := handler.process(operationContext, queued.message); err != nil {
				return fmt.Errorf("issue authenticated lane grant: %w", err)
			}
		}
	}
}

func (handler *laneGrantHandler) stopAndDrain() {
	handler.queueMu.Lock()
	defer handler.queueMu.Unlock()
	handler.stopping = true
	for {
		select {
		case <-handler.queue:
		default:
			return
		}
	}
}

func (handler *laneGrantHandler) process(ctx context.Context, message protocolsession.Message) error {
	request, err := protocolsession.DecodeLaneAttachRequest(message.Body())
	if err != nil {
		return err
	}
	operationID, ok := message.OperationID()
	if !ok {
		return sessioncontract.ErrRuntimeConfig
	}
	nonce, err := sessioncore.ReadNonzeroLaneNonce(handler.random, protocolsession.LaneAttachNonceBytes)
	if err != nil {
		return err
	}
	grant, err := handler.registry.IssueGrant(request.RequestedLaneID, operationID, nonce)
	if err != nil {
		return err
	}
	retainGrant := false
	defer func() {
		if !retainGrant {
			handler.registry.RevokeGrant(grant)
		}
	}()
	body, err := protocolsession.EncodeLaneGrant(grant)
	if err != nil {
		return err
	}
	outcome, err := handler.outbound.SendControl(ctx, protocolsession.MessageLaneAttach, operationID, body)
	// Delivered and Unknown both permit peer ownership; only a proven pre-wire
	// drop leaves the exact grant exclusively owned by this handler.
	retainGrant = outcome.Evidence() != protocolsession.ResponseSendEvidenceDefinitelyNotSent
	return err
}

// Attach routes an untrusted WS2A only far enough to find its live
// ProtocolSession. Unknown/malformed candidates are closed without a response;
// all signed responses remain behind LaneRegistry's traffic-key proof.
func (factory *Factory) Attach(
	ctx context.Context,
	channel protocolsession.FrameChannel,
) (sessioncontract.LaneIdentity, error) {
	if factory == nil || channel == nil || ctx == nil {
		if channel != nil {
			_ = channel.Close()
		}
		return sessioncontract.LaneIdentity{}, sessioncontract.ErrRuntimeConfig
	}
	owner := sessioncore.NewCandidate(channel)
	transferred := false
	defer func() {
		if !transferred {
			_ = owner.Close()
		}
	}()
	admissionContext, endAdmission, ok := factory.beginAdmission(ctx)
	if !ok {
		return sessioncontract.LaneIdentity{}, sessioncontract.ErrRuntimeClosed
	}
	defer endAdmission()
	ctx = admissionContext
	encoded, err := sessioncore.ReceiveHandshake(ctx, owner.Recv())
	if err != nil {
		return sessioncontract.LaneIdentity{}, err
	}
	identity, err := factory.attachCandidate(ctx, owner, owner.Recv(), encoded)
	if err != nil {
		return sessioncontract.LaneIdentity{}, err
	}
	transferred = true
	return identity, nil
}

// AdmitPeerChannel binds a connectivity-owned channel to this exact
// ProtocolSession before parsing its lane proof. A peer negotiation therefore
// cannot use the factory-wide untrusted route lookup to attach to a sibling
// receiver session.
func (runtime *Runtime) AdmitPeerChannel(
	ctx context.Context,
	channel protocolsession.FrameChannel,
	control PeerAdmissionControl,
) (PeerAdmissionResult, error) {
	if runtime == nil || channel == nil || ctx == nil {
		if channel != nil {
			_ = channel.Close()
		}
		return silentSenderPeerAdmission(), sessioncontract.ErrRuntimeConfig
	}
	owner := sessioncore.NewCandidate(channel)
	transferred := false
	defer func() {
		if !transferred {
			_ = owner.Close()
		}
	}()
	if control == nil {
		return silentSenderPeerAdmission(), sessioncontract.ErrRuntimeConfig
	}
	admissionContext, endAdmission, err := runtime.core.BeginExternalAdmission(ctx)
	if err != nil {
		return silentSenderPeerAdmission(), err
	}
	defer endAdmission()
	ctx = admissionContext
	encoded, err := sessioncore.ReceiveHandshake(ctx, owner.Recv())
	if err != nil {
		return silentSenderPeerAdmission(), err
	}
	share, sessionID, err := protocolsession.UntrustedLaneHelloRoute(encoded)
	if err != nil || share != runtime.core.ShareInstance() || sessionID != runtime.ProtocolSessionID() || !runtime.core.HasUsableLanes() {
		return silentSenderPeerAdmission(), errors.Join(sessioncontract.ErrHandshake, err)
	}
	result, err := runtime.acceptCandidate(ctx, owner, encoded, control)
	transferred = result.LaneAttachment == PeerLaneAttached
	return result, err
}

func (factory *Factory) attachCandidate(
	ctx context.Context,
	channel protocolsession.FrameChannel,
	receive <-chan framechannel.Frame,
	encoded []byte,
) (sessioncontract.LaneIdentity, error) {
	owner := sessioncore.NewCandidateWithReceive(channel, receive)
	transferred := false
	defer func() {
		if !transferred {
			_ = owner.Close()
		}
	}()
	result, err := factory.settleCandidate(ctx, owner, encoded)
	if err != nil {
		return sessioncontract.LaneIdentity{}, err
	}
	transferred = result.LaneAttachment == PeerLaneAttached
	return result.Lane, nil
}

func (factory *Factory) settleCandidate(
	ctx context.Context,
	channel protocolsession.FrameChannel,
	encoded []byte,
) (PeerAdmissionResult, error) {
	share, sessionID, err := protocolsession.UntrustedLaneHelloRoute(encoded)
	if err != nil || share != factory.share {
		return silentSenderPeerAdmission(), errors.Join(sessioncontract.ErrHandshake, err)
	}
	factory.mu.Lock()
	runtime := factory.sessions[sessionID]
	stopping := factory.stopping
	factory.mu.Unlock()
	if runtime == nil || stopping || !runtime.core.HasUsableLanes() {
		return silentSenderPeerAdmission(), sessioncontract.ErrHandshake
	}
	admissionContext, endAdmission, err := runtime.core.BeginExternalAdmission(ctx)
	if err != nil {
		return silentSenderPeerAdmission(), err
	}
	defer endAdmission()
	return runtime.acceptCandidate(
		admissionContext, channel, encoded,
		PeerAdmissionControlFunc(func(protocolsession.OperationID, sessioncontract.LaneIdentity) bool {
			return admissionContext.Err() == nil
		}),
	)
}

func (runtime *Runtime) acceptCandidate(
	ctx context.Context,
	channel protocolsession.FrameChannel,
	encoded []byte,
	control PeerAdmissionControl,
) (PeerAdmissionResult, error) {
	owner := sessioncore.NewCandidate(channel)
	transferred := false
	defer func() {
		if !transferred {
			_ = owner.Close()
		}
	}()
	channel = owner
	senderNonce, err := runtime.core.NewLaneNonce()
	if err != nil {
		return silentSenderPeerAdmission(), err
	}
	admission, err := runtime.lanesRegistry.AdmitCandidate(
		encoded,
		senderNonce,
		protocolsession.LaneAdmissionSettlementGateFunc(func(
			operationID protocolsession.OperationID,
			lane protocolsession.LaneAdmissionIdentity,
		) bool {
			if ctx.Err() != nil {
				return false
			}
			return control.BeginAuthenticatedSettlement(
				operationID,
				sessioncontract.LaneIdentity{ID: lane.LaneID, Epoch: lane.LaneEpoch},
			)
		}),
	)
	result := senderPeerAdmissionFromRegistry(admission)
	if admission.Disposition == protocolsession.LaneAdmissionSilentClose || err != nil {
		return result, errors.Join(sessioncontract.ErrHandshake, err)
	}
	if sendErr := channel.Send(ctx, framechannel.Frame(admission.Response)); sendErr != nil {
		result.ResponseDelivery = PeerResponseDeliveryFailed
		if admission.Disposition == protocolsession.LaneAdmissionAccepted {
			runtime.lanesRegistry.Release(admission.LaneID, admission.LaneEpoch)
		}
		if admission.Disposition == protocolsession.LaneAdmissionRejected {
			return result, errors.Join(&sessioncontract.LaneRejectedError{Rejection: admission.Rejection}, sendErr)
		}
		return result, sendErr
	}
	result.ResponseDelivery = PeerResponseDelivered
	if admission.Disposition == protocolsession.LaneAdmissionRejected {
		return result, &sessioncontract.LaneRejectedError{Rejection: admission.Rejection}
	}
	identity := sessioncontract.LaneIdentity{ID: admission.LaneID, Epoch: admission.LaneEpoch}
	authenticator := protocolsession.InboundMessageAuthenticatorFunc(
		func(uint64, protocolsession.Message) (protocolsession.InboundAuthenticationResult, error) {
			return protocolsession.InboundAuthenticationResult{}, nil
		},
	)
	if err := runtime.core.InstallLane(identity, channel, authenticator, nil, nil); err != nil {
		runtime.lanesRegistry.Release(identity.ID, identity.Epoch)
		result.LaneAttachment = PeerLaneAttachmentFailed
		return result, err
	}
	if err := sessioncore.ConfirmChannelAdmission(ctx, channel); err != nil {
		runtime.core.DetachLane(identity)
		result.LaneAttachment = PeerLaneAttachmentFailed
		return result, err
	}
	result.LaneAttachment = PeerLaneAttached
	transferred = true
	return result, nil
}

func senderPeerAdmissionFromRegistry(admission protocolsession.LaneAdmission) PeerAdmissionResult {
	result := silentSenderPeerAdmission()
	result.SettlementBegan = admission.SettlementBegan
	result.GrantOperationID = admission.OperationID
	result.Lane = sessioncontract.LaneIdentity{ID: admission.LaneID, Epoch: admission.LaneEpoch}
	result.Rejection = admission.Rejection
	switch admission.Disposition {
	case protocolsession.LaneAdmissionAccepted:
		result.Disposition = PeerAdmissionAccepted
	case protocolsession.LaneAdmissionRejected:
		result.Disposition = PeerAdmissionRejected
	}
	return result
}

func (runtime *Runtime) DetachLane(identity sessioncontract.LaneIdentity) bool {
	return runtime != nil && runtime.core.DetachLane(identity)
}

func (runtime *Runtime) AttachedLanes() int {
	if runtime == nil {
		return 0
	}
	return runtime.core.AttachedLanes()
}
