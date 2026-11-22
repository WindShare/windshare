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
	state := newStubOperationState()
	op := state.admit(operationID, protocolsession.MessageOpenRevisions)
	state.cancel(op.Ref())
	if op.IsActive() || !op.IsCurrent() {
		t.Fatal("canceled operation did not become a current non-executable tombstone")
	}
	if err := handler.HandleMessage(context.Background(), op, request); err != nil {
		t.Fatalf("stale request suppression: %v", err)
	}
	if len(handler.queue) != 0 {
		t.Fatal("canceled request reached the service queue")
	}

	siblingID := flowID[protocolsession.OperationID](202)
	sibling := operationMessage(t, protocolsession.MessageOpenRevisions, siblingID, body)
	if err := handler.HandleMessage(context.Background(), state.admit(siblingID, protocolsession.MessageOpenRevisions), sibling); err != nil {
		t.Fatalf("sibling request: %v", err)
	}
	if len(handler.queue) != 1 {
		t.Fatal("canceled generation suppressed an unrelated sibling")
	}
}
