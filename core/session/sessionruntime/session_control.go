package sessionruntime

import (
	"bytes"
	"context"
	"sync"

	"github.com/windshare/windshare/core/session/protocolsession"
)

func (runtime *runtimeCore) SetPeerPathControlHandler(handler func(context.Context, []byte) error) {
	runtime.peerPathControls.set(handler)
}

func (runtime *runtimeCore) SetTURNControlHandler(handler func(context.Context, []byte) error) {
	runtime.turnControls.set(handler)
}

// Each control family owns its callback while sharing authentication and lane
// delivery. Replacing a path handler cannot disconnect TURN delegation.
type sessionControlHandler struct {
	runtime  *runtimeCore
	mu       sync.RWMutex
	callback func(context.Context, []byte) error
}

func (handler *sessionControlHandler) set(callback func(context.Context, []byte) error) {
	handler.mu.Lock()
	handler.callback = callback
	handler.mu.Unlock()
}

func (handler *sessionControlHandler) HandleMessage(ctx context.Context, message protocolsession.Message) error {
	body := message.Body()
	if handler.runtime.role == protocolsession.RoleReceiver {
		var err error
		body, err = protocolsession.SenderControlSemanticBody(message)
		if err != nil {
			return err
		}
	}
	if err := protocolsession.ValidateSessionControlBody(message.Kind(), body); err != nil {
		return err
	}
	handler.mu.RLock()
	callback := handler.callback
	handler.mu.RUnlock()
	if callback == nil {
		return nil
	}
	return callback(ctx, bytes.Clone(body))
}

func (runtime *ReceiverRuntime) SendPeerPathControl(ctx context.Context, body []byte) error {
	return runtime.sendSessionControl(ctx, protocolsession.MessagePeerPathControl, body)
}

func (runtime *ReceiverRuntime) SendTURNControl(ctx context.Context, body []byte) error {
	return runtime.sendSessionControl(ctx, protocolsession.MessageTURNControl, body)
}

func (runtime *ReceiverRuntime) sendSessionControl(ctx context.Context, kind protocolsession.MessageKind, body []byte) error {
	if runtime == nil || ctx == nil {
		return ErrRuntimeConfig
	}
	if err := protocolsession.ValidateSessionControlBody(kind, body); err != nil {
		return err
	}
	message, err := protocolsession.NewMessage(kind, nil, body)
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

func (session senderPeerSession) SetTURNControlHandler(handler func(context.Context, []byte) error) {
	session.runtime.SetTURNControlHandler(handler)
}

func (session senderPeerSession) SendPeerPathControl(ctx context.Context, body []byte) error {
	return session.sendSessionControl(ctx, protocolsession.MessagePeerPathControl, body)
}

func (session senderPeerSession) SendTURNControl(ctx context.Context, body []byte) error {
	return session.sendSessionControl(ctx, protocolsession.MessageTURNControl, body)
}

func (session senderPeerSession) sendSessionControl(ctx context.Context, kind protocolsession.MessageKind, body []byte) error {
	if session.runtime == nil || ctx == nil {
		return ErrRuntimeConfig
	}
	if err := protocolsession.ValidateSessionControlBody(kind, body); err != nil {
		return err
	}
	lane, err := session.runtime.lanes.selectLane(nil)
	if err != nil {
		return err
	}
	prepared, err := protocolsession.PrepareSenderControl(session.outbound.privateKey, session.runtime.senderControlBase(lane.identity), kind, nil, body)
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
