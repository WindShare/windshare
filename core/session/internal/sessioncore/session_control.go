package sessioncore

import (
	"bytes"
	"context"
	"sync"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func (runtime *Runtime) SetPeerPathControlHandler(handler func(context.Context, []byte) error) {
	runtime.peerPathControls.set(handler)
}

func (runtime *Runtime) SetTURNControlHandler(handler func(context.Context, []byte) error) {
	runtime.turnControls.set(handler)
}

// Each control family owns its callback while sharing authentication and lane
// delivery.
type sessionControlHandler struct {
	mu       sync.RWMutex
	callback func(context.Context, []byte) error
}

func (handler *sessionControlHandler) set(callback func(context.Context, []byte) error) {
	handler.mu.Lock()
	handler.callback = callback
	handler.mu.Unlock()
}

func (handler *sessionControlHandler) HandleMessage(
	ctx context.Context,
	_ protocolsession.Operation,
	message protocolsession.Message,
) error {
	body := message.Body()
	handler.mu.RLock()
	callback := handler.callback
	handler.mu.RUnlock()
	if callback == nil {
		return nil
	}
	return callback(ctx, bytes.Clone(body))
}

func (outbound *Responses) SendSessionControl(ctx context.Context, kind protocolsession.MessageKind, body []byte) error {
	return outbound.runtime.SendSessionControl(ctx, kind, body)
}

func (runtime *Runtime) SendSessionControl(ctx context.Context, kind protocolsession.MessageKind, body []byte) error {
	if runtime == nil || ctx == nil {
		return session.ErrRuntimeConfig
	}
	if err := protocolsession.ValidateSessionControlBody(runtime.role, kind, body); err != nil {
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
	return lane.writer.SendControl(ctx, message, nil)
}
