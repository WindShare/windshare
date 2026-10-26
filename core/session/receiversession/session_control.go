package receiversession

import (
	"context"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func (runtime *Runtime) SendPeerPathControl(ctx context.Context, body []byte) error {
	return runtime.sendSessionControl(ctx, protocolsession.MessagePeerPathControl, body)
}

func (runtime *Runtime) SendTURNControl(ctx context.Context, body []byte) error {
	return runtime.sendSessionControl(ctx, protocolsession.MessageTURNControl, body)
}

func (runtime *Runtime) sendSessionControl(ctx context.Context, kind protocolsession.MessageKind, body []byte) error {
	if runtime == nil {
		return sessioncontract.ErrRuntimeConfig
	}
	return runtime.core.SendSessionControl(ctx, kind, body)
}
