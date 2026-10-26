package sendersession

import (
	"context"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func (peer senderPeerSession) SetPeerPathControlHandler(handler func(context.Context, []byte) error) {
	peer.runtime.SetPeerPathControlHandler(handler)
}

func (peer senderPeerSession) SetTURNControlHandler(handler func(context.Context, []byte) error) {
	peer.runtime.SetTURNControlHandler(handler)
}

func (peer senderPeerSession) SendPeerPathControl(ctx context.Context, body []byte) error {
	return peer.sendSessionControl(ctx, protocolsession.MessagePeerPathControl, body)
}

func (peer senderPeerSession) SendTURNControl(ctx context.Context, body []byte) error {
	return peer.sendSessionControl(ctx, protocolsession.MessageTURNControl, body)
}

func (peer senderPeerSession) sendSessionControl(ctx context.Context, kind protocolsession.MessageKind, body []byte) error {
	if peer.runtime == nil {
		return session.ErrRuntimeConfig
	}
	return peer.outbound.SendSessionControl(ctx, kind, body)
}
