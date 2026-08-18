package sessionruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/fxamacker/cbor/v2"

	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

const (
	laneAttachBodyVersion = uint64(1)
	laneAttachRequestMode = uint64(0)
	laneAttachGrantMode   = uint64(1)
	laneGrantQueueLimit   = 64
	laneNonceAttempts     = 4
)

var laneAttachDecMode = func() cbor.DecMode {
	mode, err := cbor.DecOptions{
		DupMapKey:       cbor.DupMapKeyEnforcedAPF,
		IndefLength:     cbor.IndefLengthForbidden,
		TagsMd:          cbor.TagsForbidden,
		MaxNestedLevels: 4,
		MaxMapPairs:     16,
	}.DecMode()
	if err != nil {
		panic(err)
	}
	return mode
}()

type laneAttachRequest struct{ requestedLaneID uint32 }

// LaneAttachmentGrant is the receiver-visible signed grant content. Expiry is
// intentionally absent because it is sender-local admission authority and is
// not carried by the wire body.
type LaneAttachmentGrant struct {
	LaneID      uint32
	LaneEpoch   uint32
	OperationID protocolsession.OperationID
	AttachNonce [protocolsession.LaneAttachNonceBytes]byte
}

func encodeLaneAttachRequest(requestedLaneID uint32) ([]byte, error) {
	return protocolsession.EncodeBody(map[uint64]any{
		0: laneAttachBodyVersion,
		1: laneAttachRequestMode,
		2: uint64(requestedLaneID),
	})
}

func decodeLaneAttachRequest(encoded []byte) (laneAttachRequest, error) {
	fields, err := decodeLaneAttachBody(encoded, 3)
	if err != nil || fields[0] != laneAttachBodyVersion || fields[1] != laneAttachRequestMode {
		return laneAttachRequest{}, errors.Join(ErrRuntimeConfig, err)
	}
	requested, ok := fields[2].(uint64)
	if !ok || requested > uint64(^uint32(0)) {
		return laneAttachRequest{}, ErrRuntimeConfig
	}
	return laneAttachRequest{requestedLaneID: uint32(requested)}, nil
}

func encodeLaneGrant(grant protocolsession.LaneGrant) ([]byte, error) {
	if grant.LaneID == 0 || grant.LaneEpoch == 0 || grant.OperationID.IsZero() || !nonzeroBytes(grant.AttachNonce[:]) {
		return nil, ErrRuntimeConfig
	}
	return protocolsession.EncodeBody(map[uint64]any{
		0: laneAttachBodyVersion,
		1: laneAttachGrantMode,
		2: uint64(grant.LaneID),
		3: uint64(grant.LaneEpoch),
		4: grant.AttachNonce[:],
	})
}

func decodeLaneGrant(encoded []byte, operationID protocolsession.OperationID) (LaneAttachmentGrant, error) {
	fields, err := decodeLaneAttachBody(encoded, 5)
	if err != nil || fields[0] != laneAttachBodyVersion || fields[1] != laneAttachGrantMode || operationID.IsZero() {
		return LaneAttachmentGrant{}, errors.Join(ErrRuntimeConfig, err)
	}
	laneID, laneOK := fields[2].(uint64)
	epoch, epochOK := fields[3].(uint64)
	nonce, nonceOK := fields[4].([]byte)
	if !laneOK || !epochOK || !nonceOK || laneID == 0 || laneID > uint64(^uint32(0)) ||
		epoch == 0 || epoch > uint64(^uint32(0)) || len(nonce) != protocolsession.LaneAttachNonceBytes || !nonzeroBytes(nonce) {
		return LaneAttachmentGrant{}, ErrRuntimeConfig
	}
	grant := LaneAttachmentGrant{
		LaneID: uint32(laneID), LaneEpoch: uint32(epoch), OperationID: operationID,
	}
	copy(grant.AttachNonce[:], nonce)
	return grant, nil
}

func decodeLaneAttachBody(encoded []byte, fieldCount int) (map[uint64]any, error) {
	var fields map[uint64]any
	if err := laneAttachDecMode.Unmarshal(encoded, &fields); err != nil || len(fields) != fieldCount {
		return nil, errors.Join(ErrRuntimeConfig, err)
	}
	canonical, err := protocolsession.EncodeBody(fields)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return nil, errors.Join(ErrRuntimeConfig, err)
	}
	return fields, nil
}

// LaneRejectedError is safe to expose only after WS2N's sender signature has
// been verified against the exact LaneHello digest.
type LaneRejectedError struct {
	Rejection protocolsession.LaneRejection
}

func (err *LaneRejectedError) Error() string {
	return fmt.Sprintf("sender rejected lane attachment with code %d", err.Rejection.Code)
}

type laneGrantHandler struct {
	registry *protocolsession.LaneRegistry
	outbound senderOutbound
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
	outbound senderOutbound,
	random io.Reader,
) *laneGrantHandler {
	return &laneGrantHandler{
		registry: registry, outbound: outbound, random: random,
		queue: make(chan laneGrantOperation, laneGrantQueueLimit),
	}
}

func (handler *laneGrantHandler) HandleMessage(ctx context.Context, message protocolsession.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if message.Kind() != protocolsession.MessageLaneAttach {
		return ErrRuntimeConfig
	}
	operationID, ok := message.OperationID()
	if !ok {
		return ErrOperationMissing
	}
	generation, ok := protocolsession.OperationGenerationFromContext(ctx, operationID)
	if !ok || generation.IsZero() {
		return ErrOperationMissing
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
		return ErrRuntimeClosed
	}
	select {
	case handler.queue <- laneGrantOperation{ctx: ctx, message: message}:
		return nil
	default:
		return ErrOperationOverflow
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
	request, err := decodeLaneAttachRequest(message.Body())
	if err != nil {
		return err
	}
	operationID, ok := message.OperationID()
	if !ok {
		return ErrRuntimeConfig
	}
	nonce, err := readNonzeroLaneNonce(handler.random, protocolsession.LaneAttachNonceBytes)
	if err != nil {
		return err
	}
	grant, err := handler.registry.IssueGrant(request.requestedLaneID, operationID, nonce)
	if err != nil {
		return err
	}
	retainGrant := false
	defer func() {
		if !retainGrant {
			handler.registry.RevokeGrant(grant)
		}
	}()
	body, err := encodeLaneGrant(grant)
	if err != nil {
		return err
	}
	outcome, err := handler.outbound.SendControl(ctx, protocolsession.MessageLaneAttach, operationID, body)
	// Delivered and Unknown both permit peer ownership; only a proven pre-wire
	// drop leaves the exact grant exclusively owned by this handler.
	retainGrant = outcome != protocolsession.SendOutcomeDropped
	return err
}

// candidateChannelOwner makes the admission boundary consume one close
// authority. Failure paths and runtime lanes may converge on Close without
// calling provider cleanup more than once.
type candidateChannelOwner struct {
	protocolsession.FrameChannel
	receive   <-chan framechannel.Frame
	closeOnce sync.Once
	closeErr  error
}

func newCandidateChannelOwner(channel protocolsession.FrameChannel) *candidateChannelOwner {
	if owner, ok := channel.(*candidateChannelOwner); ok {
		return owner
	}
	return &candidateChannelOwner{FrameChannel: channel, receive: channel.Recv()}
}

func newCandidateChannelOwnerWithReceive(
	channel protocolsession.FrameChannel,
	receive <-chan framechannel.Frame,
) *candidateChannelOwner {
	if owner, ok := channel.(*candidateChannelOwner); ok {
		return owner
	}
	return &candidateChannelOwner{FrameChannel: channel, receive: receive}
}

func (owner *candidateChannelOwner) Recv() <-chan framechannel.Frame { return owner.receive }

func (owner *candidateChannelOwner) Close() error {
	owner.closeOnce.Do(func() { owner.closeErr = owner.FrameChannel.Close() })
	return owner.closeErr
}

// Attach routes an untrusted WS2A only far enough to find its live
// ProtocolSession. Unknown/malformed candidates are closed without a response;
// all signed responses remain behind LaneRegistry's traffic-key proof.
func (factory *SenderFactory) Attach(
	ctx context.Context,
	channel protocolsession.FrameChannel,
) (LaneIdentity, error) {
	if factory == nil || channel == nil || ctx == nil {
		if channel != nil {
			_ = channel.Close()
		}
		return LaneIdentity{}, ErrRuntimeConfig
	}
	owner := newCandidateChannelOwner(channel)
	transferred := false
	defer func() {
		if !transferred {
			_ = owner.Close()
		}
	}()
	admissionContext, endAdmission, ok := factory.beginAdmission(ctx)
	if !ok {
		return LaneIdentity{}, ErrRuntimeClosed
	}
	defer endAdmission()
	ctx = admissionContext
	encoded, err := receiveHandshake(ctx, owner.Recv())
	if err != nil {
		return LaneIdentity{}, err
	}
	identity, err := factory.attachCandidate(ctx, owner, owner.Recv(), encoded)
	if err != nil {
		return LaneIdentity{}, err
	}
	transferred = true
	return identity, nil
}

// AdmitPeerChannel binds a connectivity-owned channel to this exact
// ProtocolSession before parsing its lane proof. A peer negotiation therefore
// cannot use the factory-wide untrusted route lookup to attach to a sibling
// receiver session.
func (runtime *SenderRuntime) AdmitPeerChannel(
	ctx context.Context,
	channel protocolsession.FrameChannel,
	control SenderPeerAdmissionControl,
) (SenderPeerAdmissionResult, error) {
	if runtime == nil || channel == nil || ctx == nil {
		if channel != nil {
			_ = channel.Close()
		}
		return silentSenderPeerAdmission(), ErrRuntimeConfig
	}
	owner := newCandidateChannelOwner(channel)
	transferred := false
	defer func() {
		if !transferred {
			_ = owner.Close()
		}
	}()
	if control == nil {
		return silentSenderPeerAdmission(), ErrRuntimeConfig
	}
	admissionContext, endAdmission, err := runtime.beginExternalAdmission(ctx)
	if err != nil {
		return silentSenderPeerAdmission(), err
	}
	defer endAdmission()
	ctx = admissionContext
	encoded, err := receiveHandshake(ctx, owner.Recv())
	if err != nil {
		return silentSenderPeerAdmission(), err
	}
	share, sessionID, err := protocolsession.UntrustedLaneHelloRoute(encoded)
	if err != nil || share != runtime.share || sessionID != runtime.ProtocolSessionID() || !runtime.lanes.hasUsable() {
		return silentSenderPeerAdmission(), errors.Join(ErrHandshake, err)
	}
	result, err := runtime.acceptCandidate(ctx, owner, encoded, control)
	transferred = result.LaneAttachment == SenderPeerLaneAttached
	return result, err
}

func (factory *SenderFactory) attachCandidate(
	ctx context.Context,
	channel protocolsession.FrameChannel,
	receive <-chan framechannel.Frame,
	encoded []byte,
) (LaneIdentity, error) {
	owner := newCandidateChannelOwnerWithReceive(channel, receive)
	transferred := false
	defer func() {
		if !transferred {
			_ = owner.Close()
		}
	}()
	result, err := factory.settleCandidate(ctx, owner, encoded)
	if err != nil {
		return LaneIdentity{}, err
	}
	transferred = result.LaneAttachment == SenderPeerLaneAttached
	return result.Lane, nil
}

func (factory *SenderFactory) settleCandidate(
	ctx context.Context,
	channel protocolsession.FrameChannel,
	encoded []byte,
) (SenderPeerAdmissionResult, error) {
	share, sessionID, err := protocolsession.UntrustedLaneHelloRoute(encoded)
	if err != nil || share != factory.share {
		return silentSenderPeerAdmission(), errors.Join(ErrHandshake, err)
	}
	factory.mu.Lock()
	runtime := factory.sessions[sessionID]
	stopping := factory.stopping
	factory.mu.Unlock()
	if runtime == nil || stopping || !runtime.lanes.hasUsable() {
		return silentSenderPeerAdmission(), ErrHandshake
	}
	admissionContext, endAdmission, err := runtime.beginExternalAdmission(ctx)
	if err != nil {
		return silentSenderPeerAdmission(), err
	}
	defer endAdmission()
	return runtime.acceptCandidate(
		admissionContext, channel, encoded,
		SenderPeerAdmissionControlFunc(func(protocolsession.OperationID, LaneIdentity) bool {
			return admissionContext.Err() == nil
		}),
	)
}

func (runtime *SenderRuntime) acceptCandidate(
	ctx context.Context,
	channel protocolsession.FrameChannel,
	encoded []byte,
	control SenderPeerAdmissionControl,
) (SenderPeerAdmissionResult, error) {
	owner := newCandidateChannelOwner(channel)
	transferred := false
	defer func() {
		if !transferred {
			_ = owner.Close()
		}
	}()
	channel = owner
	senderNonce, err := readNonzeroLaneNonce(runtime.random, protocolsession.LaneSenderNonceBytes)
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
				LaneIdentity{ID: lane.LaneID, Epoch: lane.LaneEpoch},
			)
		}),
	)
	result := senderPeerAdmissionFromRegistry(admission)
	if admission.Disposition == protocolsession.LaneAdmissionSilentClose || err != nil {
		return result, errors.Join(ErrHandshake, err)
	}
	if sendErr := channel.Send(ctx, framechannel.Frame(admission.Response)); sendErr != nil {
		result.ResponseDelivery = SenderPeerResponseDeliveryFailed
		if admission.Disposition == protocolsession.LaneAdmissionAccepted {
			runtime.lanesRegistry.Release(admission.LaneID, admission.LaneEpoch)
		}
		if admission.Disposition == protocolsession.LaneAdmissionRejected {
			return result, errors.Join(&LaneRejectedError{Rejection: admission.Rejection}, sendErr)
		}
		return result, sendErr
	}
	result.ResponseDelivery = SenderPeerResponseDelivered
	if admission.Disposition == protocolsession.LaneAdmissionRejected {
		return result, &LaneRejectedError{Rejection: admission.Rejection}
	}
	identity := LaneIdentity{ID: admission.LaneID, Epoch: admission.LaneEpoch}
	authenticator := protocolsession.InboundMessageAuthenticatorFunc(
		func(uint64, protocolsession.Message) (protocolsession.InboundAuthenticationResult, error) {
			return protocolsession.InboundAuthenticationResult{}, nil
		},
	)
	if _, err := runtime.lanes.add(identity, channel, authenticator, false); err != nil {
		runtime.lanesRegistry.Release(identity.ID, identity.Epoch)
		result.LaneAttachment = SenderPeerLaneAttachmentFailed
		return result, err
	}
	result.LaneAttachment = SenderPeerLaneAttached
	transferred = true
	return result, nil
}

func senderPeerAdmissionFromRegistry(admission protocolsession.LaneAdmission) SenderPeerAdmissionResult {
	result := silentSenderPeerAdmission()
	result.SettlementBegan = admission.SettlementBegan
	result.GrantOperationID = admission.OperationID
	result.Lane = LaneIdentity{ID: admission.LaneID, Epoch: admission.LaneEpoch}
	result.Rejection = admission.Rejection
	switch admission.Disposition {
	case protocolsession.LaneAdmissionAccepted:
		result.Disposition = SenderPeerAdmissionAccepted
	case protocolsession.LaneAdmissionRejected:
		result.Disposition = SenderPeerAdmissionRejected
	}
	return result
}

// RequestLane obtains the one-use sender-signed grant before connectivity opens
// or attaches a new physical channel.
func (runtime *ReceiverRuntime) RequestLane(
	ctx context.Context,
	requestedLaneID uint32,
) (LaneAttachmentGrant, error) {
	if runtime == nil {
		return LaneAttachmentGrant{}, ErrRuntimeClosed
	}
	body, err := encodeLaneAttachRequest(requestedLaneID)
	if err != nil {
		return LaneAttachmentGrant{}, err
	}
	call, err := runtime.rpc.begin(ctx, protocolsession.MessageLaneAttach, body)
	if err != nil {
		return LaneAttachmentGrant{}, err
	}
	defer func() { _ = runtime.rpc.cancelAndEnd(call, contentflow.CancelReasonOutputAbort) }()
	message, err := runtime.rpc.await(ctx, call)
	if err != nil {
		return LaneAttachmentGrant{}, err
	}
	if message.Kind() == protocolsession.MessageOperationError {
		return LaneAttachmentGrant{}, remoteOperationError(message)
	}
	if message.Kind() != protocolsession.MessageLaneAttach {
		return LaneAttachmentGrant{}, ErrOperationMissing
	}
	unsigned, err := protocolsession.SenderControlSemanticBody(message)
	if err != nil {
		return LaneAttachmentGrant{}, err
	}
	return decodeLaneGrant(unsigned, call.id)
}

func (runtime *ReceiverRuntime) DetachLane(identity LaneIdentity) bool {
	return runtime != nil && runtime.lanes.detach(identity)
}

func (runtime *SenderRuntime) DetachLane(identity LaneIdentity) bool {
	return runtime != nil && runtime.lanes.detach(identity)
}

func (runtime *ReceiverRuntime) AttachedLanes() int {
	if runtime == nil {
		return 0
	}
	return runtime.lanes.len()
}

func (runtime *SenderRuntime) AttachedLanes() int {
	if runtime == nil {
		return 0
	}
	return runtime.lanes.len()
}

func readNonzeroLaneNonce(source io.Reader, size int) ([]byte, error) {
	for range laneNonceAttempts {
		value := make([]byte, size)
		if _, err := io.ReadFull(source, value); err != nil {
			return nil, err
		}
		if nonzeroBytes(value) {
			return value, nil
		}
	}
	return nil, ErrRuntimeConfig
}

func nonzeroBytes(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return true
		}
	}
	return false
}
