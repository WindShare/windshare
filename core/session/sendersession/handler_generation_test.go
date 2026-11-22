package sendersession

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func canceledSenderOperation(
	t *testing.T,
	request protocolsession.Message,
) protocolsession.Operation {
	t.Helper()
	id, ok := request.OperationID()
	if !ok {
		t.Fatal("missing operation id")
	}
	state := newStubOperationState()
	op := state.admit(id, request.Kind())
	state.cancel(op.Ref())
	if op.IsActive() || !op.IsCurrent() {
		t.Fatal("canceled operation retained executable authority")
	}
	return op
}

type countingCancelHandler struct{ calls atomic.Int32 }

func (handler *countingCancelHandler) HandleMessage(context.Context, protocolsession.Operation, protocolsession.Message) error {
	handler.calls.Add(1)
	return nil
}

type countingPeerCancelHandler struct{ cancels atomic.Int32 }

func (*countingPeerCancelHandler) HandleMessage(context.Context, protocolsession.Operation, protocolsession.Message) error {
	return nil
}

func (handler *countingPeerCancelHandler) Cancel(context.Context, protocolsession.Operation) error {
	handler.cancels.Add(1)
	return nil
}

func (*countingPeerCancelHandler) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestCancelMuxRoutesStormToOnlyTheExactOperationOwner(t *testing.T) {
	catalog := &countingCancelHandler{}
	contentHandler := &countingCancelHandler{}
	peer := &countingPeerCancelHandler{}
	mux := cancelMux{catalog: catalog, content: contentHandler, peer: peer}
	state := newStubOperationState()
	cancelBody, _ := contentflow.EncodeCancelReason(contentflow.CancelReasonSuperseded)
	routeCancel := func(seed byte, requestKind protocolsession.MessageKind) {
		operationID := id16[protocolsession.OperationID](seed)
		op := state.admit(operationID, requestKind)
		state.cancel(op.Ref())
		cancel := operationMessageForTest(t, protocolsession.MessageCancel, operationID, cancelBody)
		if err := mux.HandleMessage(context.Background(), op, cancel); err != nil {
			t.Fatal(err)
		}
	}
	for seed := byte(1); seed <= 64; seed++ {
		routeCancel(seed, protocolsession.MessageRequestBlocks)
	}
	if contentHandler.calls.Load() != 64 || catalog.calls.Load() != 0 || peer.cancels.Load() != 0 {
		t.Fatalf("content cancel storm routed content=%d catalog=%d peer=%d",
			contentHandler.calls.Load(), catalog.calls.Load(), peer.cancels.Load())
	}
	routeCancel(65, protocolsession.MessageListChildren)
	routeCancel(66, protocolsession.MessagePeerOffer)
	routeCancel(67, protocolsession.MessageLaneAttach)
	if contentHandler.calls.Load() != 64 || catalog.calls.Load() != 1 || peer.cancels.Load() != 1 {
		t.Fatalf("cross-domain cancel routing content=%d catalog=%d peer=%d",
			contentHandler.calls.Load(), catalog.calls.Load(), peer.cancels.Load())
	}
}

func TestSenderQueuesSuppressCanceledGenerationBeforeWorkPublication(t *testing.T) {
	operationID := id16[protocolsession.OperationID](201)
	catalogRequest := operationMessageForTest(
		t, protocolsession.MessageListChildren, operationID, []byte{1},
	)
	catalogHandler := newCatalogHandler(nil, &sessioncore.Responses{})
	if err := catalogHandler.HandleMessage(context.Background(), canceledSenderOperation(t, catalogRequest), catalogRequest); err != nil {
		t.Fatalf("catalog stale request: %v", err)
	}
	if len(catalogHandler.queue) != 0 {
		t.Fatal("canceled catalog request reached its service queue")
	}

	laneBody, _ := protocolsession.EncodeLaneAttachRequest(0)
	laneRequest := operationMessageForTest(t, protocolsession.MessageLaneAttach, operationID, laneBody)
	laneHandler := newLaneGrantHandler(nil, &sessioncore.Responses{}, nil)
	if err := laneHandler.HandleMessage(context.Background(), canceledSenderOperation(t, laneRequest), laneRequest); err != nil {
		t.Fatalf("lane stale request: %v", err)
	}
	if len(laneHandler.queue) != 0 {
		t.Fatal("canceled lane request reached the grant queue")
	}

	siblingID := id16[protocolsession.OperationID](202)
	sibling := operationMessageForTest(t, protocolsession.MessageListChildren, siblingID, []byte{1})
	if err := catalogHandler.HandleMessage(context.Background(), senderIngressOperation(t, sibling), sibling); err != nil {
		t.Fatalf("catalog sibling: %v", err)
	}
	if len(catalogHandler.queue) != 1 {
		t.Fatal("canceled generation suppressed an unrelated catalog request")
	}
}
