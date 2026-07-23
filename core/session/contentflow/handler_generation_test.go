package contentflow

import (
	"context"
	"testing"

	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestSenderHandlerSuppressesRequestWhoseGenerationWasCanceledBeforePublication(t *testing.T) {
	fixture := newRuntimeFixture(t, 1)
	defer fixture.close(t)
	handler, err := NewSenderHandler(SenderHandlerConfig{
		Service: fixture.service, Outbound: newRecordingOutbound(), QueueDepth: 2, Workers: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	requestBody, _ := NewOpenRequest([]OpenItem{{FileID: fixture.file}})
	body, _ := EncodeOpenRequest(requestBody)
	operationID := flowID[protocolsession.OperationID](201)
	request := operationMessage(t, protocolsession.MessageOpenRevisions, operationID, body)
	operations, _ := protocolsession.NewOperationTable(
		protocolsession.OperationLimits{MaxActive: 2, MaxTombstones: 2}, nil,
	)
	admission, err := operations.ObserveInbound(protocolsession.DirectionReceiverToSender, request)
	if err != nil {
		t.Fatal(err)
	}
	requestContext := protocolsession.WithOperationGeneration(context.Background(), admission.Generation)
	cancelBody, _ := EncodeCancelReason(CancelReasonSuperseded)
	cancel := operationMessage(t, protocolsession.MessageCancel, operationID, cancelBody)
	if _, err := operations.ObserveInbound(protocolsession.DirectionReceiverToSender, cancel); err != nil {
		t.Fatal(err)
	}
	if admission.Generation.IsActive() || !admission.Generation.IsCurrent() {
		t.Fatal("canceled generation did not become a current non-executable tombstone")
	}
	if err := handler.HandleMessage(requestContext, request); err != nil {
		t.Fatalf("stale request suppression: %v", err)
	}
	if len(handler.queue) != 0 {
		t.Fatal("canceled request reached the service queue")
	}

	siblingID := flowID[protocolsession.OperationID](202)
	sibling := operationMessage(t, protocolsession.MessageOpenRevisions, siblingID, body)
	if err := handler.HandleMessage(senderMessageContext(t, sibling), sibling); err != nil {
		t.Fatalf("sibling request: %v", err)
	}
	if len(handler.queue) != 1 {
		t.Fatal("canceled generation suppressed an unrelated sibling")
	}
}
