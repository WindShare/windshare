package sendersession

import (
	"context"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
)

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
	if session.runtime == nil {
		return sessioncontract.ErrRuntimeConfig
	}
	return session.outbound.SendSessionControl(ctx, kind, body)
}
