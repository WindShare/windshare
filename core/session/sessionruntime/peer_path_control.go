package sessionruntime

import (
	"bytes"
	"context"

	"github.com/windshare/windshare/core/session/protocolsession"
)

// PeerPathControlSession is independent of an operation capability. A handler
// may expire path demand after its negotiation has already reached a final.
type PeerPathControlSession interface {
	SetPeerPathControlHandler(func(context.Context, []byte) error)
	SendPeerPathControl(context.Context, []byte) error
}

func (runtime *runtimeCore) SetPeerPathControlHandler(handler func(context.Context, []byte) error) {
	runtime.peerPathMu.Lock()
	runtime.peerPathHandler = handler
	runtime.peerPathMu.Unlock()
}

type peerPathControlHandler struct{ runtime *runtimeCore }

func (handler peerPathControlHandler) HandleMessage(ctx context.Context, message protocolsession.Message) error {
	body := message.Body()
	if handler.runtime.role == protocolsession.RoleReceiver {
		var err error
		body, err = protocolsession.SenderControlSemanticBody(message)
		if err != nil {
			return err
		}
	}
	if _, err := protocolsession.DecodePeerPathControl(body); err != nil {
		return err
	}
	handler.runtime.peerPathMu.RLock()
	callback := handler.runtime.peerPathHandler
	handler.runtime.peerPathMu.RUnlock()
	if callback == nil {
		return nil
	}
	return callback(ctx, bytes.Clone(body))
}

func (runtime *ReceiverRuntime) SendPeerPathControl(ctx context.Context, body []byte) error {
	if runtime == nil || ctx == nil {
		return ErrRuntimeConfig
	}
	if _, err := protocolsession.DecodePeerPathControl(body); err != nil {
		return err
	}
	message, err := protocolsession.NewMessage(protocolsession.MessagePeerPathControl, nil, body)
	if err != nil {
		return err
	}
	lane, err := runtime.lanes.selectLane(nil)
	if err != nil {
		return err
	}
	receipt, err := lane.writer.TryControl(message)
	if err != nil {
		return err
	}
	_, err = receipt.Wait(ctx)
	return err
}

func (session senderPeerSession) SetPeerPathControlHandler(handler func(context.Context, []byte) error) {
	session.runtime.SetPeerPathControlHandler(handler)
}
func (session senderPeerSession) SendPeerPathControl(ctx context.Context, body []byte) error {
	if session.runtime == nil || ctx == nil {
		return ErrRuntimeConfig
	}
	if _, err := protocolsession.DecodePeerPathControl(body); err != nil {
		return err
	}
	lane, err := session.runtime.lanes.selectLane(nil)
	if err != nil {
		return err
	}
	prepared, err := protocolsession.PrepareSenderControl(session.outbound.privateKey, session.runtime.senderControlBase(lane.identity), protocolsession.MessagePeerPathControl, nil, body)
	if err != nil {
		return err
	}
	receipt, err := lane.writer.TrySenderControl(prepared)
	if err != nil {
		return err
	}
	_, err = receipt.Wait(ctx)
	return err
}

var _ PeerPathControlSession = senderPeerSession{}
var _ PeerPathControlSession = (*ReceiverRuntime)(nil)
