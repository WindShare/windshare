package sessioncore

import (
	"bytes"
	"context"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
	"sync"
)

func (runtime *Runtime) SetPeerPathControlHandler(handler func(context.Context, []byte) error) {
	runtime.peerPathControls.set(handler)
}

func (runtime *Runtime) SetTURNControlHandler(handler func(context.Context, []byte) error) {
	runtime.turnControls.set(handler)
}

// Each control family owns its callback while sharing authentication and lane
// delivery. Replacing a path handler cannot disconnect TURN delegation.
type sessionControlHandler struct {
	runtime  *Runtime
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

func (outbound *Responses) SendSessionControl(ctx context.Context, kind protocolsession.MessageKind, body []byte) error {
	if outbound.runtime == nil || ctx == nil {
		return session.ErrRuntimeConfig
	}
	if err := protocolsession.ValidateSessionControlBody(kind, body); err != nil {
		return err
	}
	lane, err := outbound.runtime.lanes.selectLane(nil)
	if err != nil {
		return err
	}
	prepared, err := outbound.prepareControl(outbound.controlBinding(lane.identity), kind, nil, body)
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

func (runtime *Runtime) SendSessionControl(ctx context.Context, kind protocolsession.MessageKind, body []byte) error {
	if runtime == nil || ctx == nil {
		return session.ErrRuntimeConfig
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
