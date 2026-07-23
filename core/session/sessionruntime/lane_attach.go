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
	"github.com/windshare/windshare/core/transfer"
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

// Attach routes an untrusted WS2A only far enough to find its live
// ProtocolSession. Unknown/malformed candidates are closed without a response;
// all signed responses remain behind LaneRegistry's traffic-key proof.
func (factory *SenderFactory) Attach(
	ctx context.Context,
	channel protocolsession.FrameChannel,
) (LaneIdentity, error) {
	if factory == nil || channel == nil || ctx == nil {
		return LaneIdentity{}, ErrRuntimeConfig
	}
	admissionContext, endAdmission, ok := factory.beginAdmission(ctx)
	if !ok {
		return LaneIdentity{}, ErrRuntimeClosed
	}
	defer endAdmission()
	ctx = admissionContext
	receive := channel.Recv()
	encoded, err := receiveHandshake(ctx, receive)
	if err != nil {
		_ = channel.Close()
		return LaneIdentity{}, err
	}
	return factory.attachCandidate(ctx, channel, receive, encoded)
}

// AdmitPeerChannel binds a connectivity-owned channel to this exact
// ProtocolSession before parsing its lane proof. A peer negotiation therefore
// cannot use the factory-wide untrusted route lookup to attach to a sibling
// receiver session.
func (runtime *SenderRuntime) AdmitPeerChannel(
	ctx context.Context,
	channel protocolsession.FrameChannel,
) (LaneIdentity, error) {
	if runtime == nil || channel == nil || ctx == nil {
		return LaneIdentity{}, ErrRuntimeConfig
	}
	admissionContext, endAdmission, err := runtime.beginExternalAdmission(ctx)
	if err != nil {
		return LaneIdentity{}, err
	}
	defer endAdmission()
	ctx = admissionContext
	receive := channel.Recv()
	encoded, err := receiveHandshake(ctx, receive)
	if err != nil {
		_ = channel.Close()
		return LaneIdentity{}, err
	}
	share, sessionID, err := protocolsession.UntrustedLaneHelloRoute(encoded)
	if err != nil || share != runtime.share || sessionID != runtime.ProtocolSessionID() || !runtime.lanes.hasUsable() {
		_ = channel.Close()
		return LaneIdentity{}, errors.Join(ErrHandshake, err)
	}
	return runtime.acceptCandidate(ctx, &handedOffChannel{FrameChannel: channel, receive: receive}, encoded)
}

func (factory *SenderFactory) attachCandidate(
	ctx context.Context,
	channel protocolsession.FrameChannel,
	receive <-chan framechannel.Frame,
	encoded []byte,
) (LaneIdentity, error) {
	share, sessionID, err := protocolsession.UntrustedLaneHelloRoute(encoded)
	if err != nil || share != factory.share {
		_ = channel.Close()
		return LaneIdentity{}, errors.Join(ErrHandshake, err)
	}
	factory.mu.Lock()
	runtime := factory.sessions[sessionID]
	stopping := factory.stopping
	factory.mu.Unlock()
	if runtime == nil || stopping || !runtime.lanes.hasUsable() {
		_ = channel.Close()
		return LaneIdentity{}, ErrHandshake
	}
	admissionContext, endAdmission, err := runtime.beginExternalAdmission(ctx)
	if err != nil {
		_ = channel.Close()
		return LaneIdentity{}, err
	}
	defer endAdmission()
	return runtime.acceptCandidate(
		admissionContext,
		&handedOffChannel{FrameChannel: channel, receive: receive},
		encoded,
	)
}

func (runtime *SenderRuntime) acceptCandidate(
	ctx context.Context,
	channel protocolsession.FrameChannel,
	encoded []byte,
) (LaneIdentity, error) {
	senderNonce, err := readNonzeroLaneNonce(runtime.random, protocolsession.LaneSenderNonceBytes)
	if err != nil {
		_ = channel.Close()
		return LaneIdentity{}, err
	}
	admission, err := runtime.lanesRegistry.AdmitCandidate(encoded, senderNonce)
	if admission.Disposition == protocolsession.LaneAdmissionSilentClose || err != nil {
		_ = channel.Close()
		return LaneIdentity{}, errors.Join(ErrHandshake, err)
	}
	if sendErr := channel.Send(ctx, framechannel.Frame(admission.Response)); sendErr != nil {
		if admission.Disposition == protocolsession.LaneAdmissionAccepted {
			runtime.lanesRegistry.Release(admission.LaneID, admission.LaneEpoch)
		}
		_ = channel.Close()
		return LaneIdentity{}, sendErr
	}
	if admission.Disposition == protocolsession.LaneAdmissionRejected {
		_ = channel.Close()
		return LaneIdentity{}, &LaneRejectedError{Rejection: protocolsession.LaneRejection{Code: admission.Rejection}}
	}
	identity := LaneIdentity{ID: admission.LaneID, Epoch: admission.LaneEpoch}
	authenticator := protocolsession.InboundMessageAuthenticatorFunc(
		func(uint64, protocolsession.Message) (protocolsession.InboundAuthenticationResult, error) {
			return protocolsession.InboundAuthenticationResult{}, nil
		},
	)
	if _, err := runtime.lanes.add(identity, channel, authenticator, false); err != nil {
		runtime.lanesRegistry.Release(identity.ID, identity.Epoch)
		return LaneIdentity{}, err
	}
	return identity, nil
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

// AttachLane authenticates a connectivity-owned FrameChannel as exactly the
// granted LaneID/Epoch, then makes it available to both the shared router and the
// receiver-scoped LaneSet. Provider, path, and cost remain outside core.
func (runtime *ReceiverRuntime) AttachLane(
	ctx context.Context,
	grant LaneAttachmentGrant,
	channel protocolsession.FrameChannel,
) (LaneIdentity, error) {
	if runtime == nil || channel == nil || ctx == nil || grant.LaneID == 0 || grant.LaneEpoch == 0 || grant.OperationID.IsZero() {
		return LaneIdentity{}, ErrRuntimeConfig
	}
	admissionContext, endAdmission, err := runtime.beginExternalAdmission(ctx)
	if err != nil {
		return LaneIdentity{}, err
	}
	defer endAdmission()
	ctx = admissionContext
	hello, err := protocolsession.NewLaneHello(
		runtime.descriptor.ShareInstance(), runtime.ProtocolSessionID(), grant.LaneID, grant.LaneEpoch,
		grant.OperationID, grant.AttachNonce[:], runtime.keys.ReceiverToSender(),
	)
	if err != nil {
		return LaneIdentity{}, err
	}
	receive := channel.Recv()
	if err := channel.Send(ctx, framechannel.Frame(hello.Encoded())); err != nil {
		_ = channel.Close()
		return LaneIdentity{}, err
	}
	response, err := receiveHandshake(ctx, receive)
	if err != nil {
		_ = channel.Close()
		return LaneIdentity{}, err
	}
	if len(response) == protocolsession.LaneRejectBytes {
		rejection, parseErr := protocolsession.ParseLaneReject(response, hello, runtime.publicKey)
		_ = channel.Close()
		if parseErr != nil {
			return LaneIdentity{}, parseErr
		}
		return LaneIdentity{}, &LaneRejectedError{Rejection: rejection}
	}
	if _, err := protocolsession.ParseLaneAccept(response, hello, runtime.publicKey); err != nil {
		_ = channel.Close()
		return LaneIdentity{}, err
	}
	identity := LaneIdentity{ID: grant.LaneID, Epoch: grant.LaneEpoch}
	base := protocolsession.ControlBinding{
		ShareInstance: runtime.descriptor.ShareInstance(), ProtocolSessionID: runtime.ProtocolSessionID(),
		LaneID: identity.ID, LaneEpoch: identity.Epoch, Direction: protocolsession.DirectionSenderToReceiver,
	}
	authenticator, err := protocolsession.NewSenderControlAuthenticator(runtime.publicKey, base, runtime.semantic)
	if err != nil {
		_ = channel.Close()
		return LaneIdentity{}, err
	}
	handOff := &handedOffChannel{FrameChannel: channel, receive: receive}
	blockLane := &receiverBlockLane{
		identity: identity, rpc: runtime.rpc, assembler: runtime.assembler,
		opener: runtime.opener, revisions: runtime.revisions,
	}
	_, err = runtime.lanes.addWithAdmission(identity, handOff, authenticator, false, func() error {
		return runtime.laneSet.Add(transfer.LaneIdentity{ID: identity.ID, Epoch: identity.Epoch}, blockLane)
	})
	if err != nil {
		return LaneIdentity{}, err
	}
	return identity, nil
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
	for attempt := 0; attempt < laneNonceAttempts; attempt++ {
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
