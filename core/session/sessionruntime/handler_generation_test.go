package sessionruntime

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func canceledSenderIngress(
	t *testing.T,
	request protocolsession.Message,
) context.Context {
	t.Helper()
	operationID, _ := request.OperationID()
	operations, _ := protocolsession.NewOperationTable(
		protocolsession.OperationLimits{MaxActive: 2, MaxTombstones: 2}, nil,
	)
	admission, err := operations.ObserveInbound(protocolsession.DirectionReceiverToSender, request)
	if err != nil {
		t.Fatal(err)
	}
	cancelBody, _ := contentflow.EncodeCancelReason(contentflow.CancelReasonSuperseded)
	cancel := operationMessageForTest(t, protocolsession.MessageCancel, operationID, cancelBody)
	if _, err := operations.ObserveInbound(protocolsession.DirectionReceiverToSender, cancel); err != nil {
		t.Fatal(err)
	}
	if admission.Generation.IsActive() || !admission.Generation.IsCurrent() {
		t.Fatal("canceled ingress generation retained executable authority")
	}
	return protocolsession.WithOperationGeneration(context.Background(), admission.Generation)
}

type countingCancelHandler struct{ calls atomic.Int32 }

func (handler *countingCancelHandler) HandleMessage(context.Context, protocolsession.Message) error {
	handler.calls.Add(1)
	return nil
}

type countingPeerCancelHandler struct{ cancels atomic.Int32 }

func (*countingPeerCancelHandler) HandleMessage(context.Context, protocolsession.Message) error {
	return nil
}

func (handler *countingPeerCancelHandler) Cancel(context.Context, protocolsession.OperationID) error {
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
	operations, _ := protocolsession.NewOperationTable(
		protocolsession.OperationLimits{MaxActive: 128, MaxTombstones: 128}, nil,
	)
	cancelBody, _ := contentflow.EncodeCancelReason(contentflow.CancelReasonSuperseded)
	routeCancel := func(seed byte, requestKind protocolsession.MessageKind) {
		operationID := id16[protocolsession.OperationID](seed)
		request := operationMessageForTest(t, requestKind, operationID, []byte{1})
		admission, err := operations.ObserveInbound(protocolsession.DirectionReceiverToSender, request)
		if err != nil {
			t.Fatal(err)
		}
		cancel := operationMessageForTest(t, protocolsession.MessageCancel, operationID, cancelBody)
		if _, err := operations.ObserveInbound(protocolsession.DirectionReceiverToSender, cancel); err != nil {
			t.Fatal(err)
		}
		ctx := protocolsession.WithOperationGeneration(context.Background(), admission.Generation)
		if err := mux.HandleMessage(ctx, cancel); err != nil {
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
	catalogHandler := newCatalogHandler(nil, senderOutbound{})
	if err := catalogHandler.HandleMessage(canceledSenderIngress(t, catalogRequest), catalogRequest); err != nil {
		t.Fatalf("catalog stale request: %v", err)
	}
	if len(catalogHandler.queue) != 0 {
		t.Fatal("canceled catalog request reached its service queue")
	}

	laneBody, _ := encodeLaneAttachRequest(0)
	laneRequest := operationMessageForTest(t, protocolsession.MessageLaneAttach, operationID, laneBody)
	laneHandler := newLaneGrantHandler(nil, senderOutbound{}, nil)
	if err := laneHandler.HandleMessage(canceledSenderIngress(t, laneRequest), laneRequest); err != nil {
		t.Fatalf("lane stale request: %v", err)
	}
	if len(laneHandler.queue) != 0 {
		t.Fatal("canceled lane request reached the grant queue")
	}

	siblingID := id16[protocolsession.OperationID](202)
	sibling := operationMessageForTest(t, protocolsession.MessageListChildren, siblingID, []byte{1})
	if err := catalogHandler.HandleMessage(senderIngressContext(t, sibling), sibling); err != nil {
		t.Fatalf("catalog sibling: %v", err)
	}
	if len(catalogHandler.queue) != 1 {
		t.Fatal("canceled generation suppressed an unrelated catalog request")
	}
}
