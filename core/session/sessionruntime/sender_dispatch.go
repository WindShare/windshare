package sessionruntime

import (
	"context"

	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type operationCancelHandler interface {
	HandleMessage(context.Context, protocolsession.Message) error
}

type cancelMux struct {
	catalog operationCancelHandler
	content operationCancelHandler
	peer    SenderPeerHandler
}

func (mux cancelMux) HandleMessage(ctx context.Context, message protocolsession.Message) error {
	operationID, ok := message.OperationID()
	if !ok {
		return ErrRuntimeConfig
	}
	if _, err := contentflow.DecodeCancelReason(message.Body()); err != nil {
		return err
	}
	generation, ok := protocolsession.OperationGenerationFromContext(ctx, operationID)
	if !ok {
		return ErrOperationMissing
	}
	requestKind, ok := generation.RequestKind()
	if !ok {
		// A preemptive CANCEL has no work owner. Delayed handlers independently
		// suppress its now-tombstoned generation before starting service work.
		return nil
	}
	switch requestKind {
	case protocolsession.MessageListChildren:
		return mux.catalog.HandleMessage(ctx, message)
	case protocolsession.MessageOpenRevisions, protocolsession.MessageRenewLease,
		protocolsession.MessageReleaseLease, protocolsession.MessageRequestBlocks:
		return mux.content.HandleMessage(ctx, message)
	case protocolsession.MessagePeerOffer:
		return mux.peer.Cancel(ctx, operationID)
	case protocolsession.MessageLaneAttach:
		return nil
	default:
		return ErrRuntimeConfig
	}
}

func registerSenderHandlers(
	router *protocolsession.RoleRouter,
	catalogHandler *catalogHandler,
	contentHandler *contentflow.SenderHandler,
	laneHandler *laneGrantHandler,
	peerHandler SenderPeerHandler,
) error {
	if err := router.RegisterHandler(protocolsession.MessageListChildren, catalogHandler); err != nil {
		return err
	}
	for _, kind := range []protocolsession.MessageKind{
		protocolsession.MessageOpenRevisions, protocolsession.MessageRenewLease,
		protocolsession.MessageReleaseLease, protocolsession.MessageRequestBlocks,
	} {
		if err := router.RegisterHandler(kind, contentHandler); err != nil {
			return err
		}
	}
	if err := router.RegisterHandler(protocolsession.MessageLaneAttach, laneHandler); err != nil {
		return err
	}
	for _, kind := range []protocolsession.MessageKind{
		protocolsession.MessagePeerOffer, protocolsession.MessagePeerCandidate,
	} {
		if err := router.RegisterHandler(kind, peerHandler); err != nil {
			return err
		}
	}
	return router.RegisterHandler(protocolsession.MessageCancel, cancelMux{
		catalog: catalogHandler, content: contentHandler, peer: peerHandler,
	})
}
