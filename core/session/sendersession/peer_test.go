package sendersession

import (
	"context"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type inertSenderPeerHandler struct{}

func (inertSenderPeerHandler) HandleMessage(context.Context, protocolsession.Operation, protocolsession.Message) error {
	return nil
}

func (inertSenderPeerHandler) Cancel(context.Context, protocolsession.Operation) error {
	return nil
}

func (inertSenderPeerHandler) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func inertSenderPeerFactory() PeerHandlerFactory {
	return PeerHandlerFactoryFunc(func(PeerSession) (PeerHandler, error) {
		return inertSenderPeerHandler{}, nil
	})
}
