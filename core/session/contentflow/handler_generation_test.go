package contentflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session/protocolsession"
)

type contextCapturingOutbound struct {
	contexts chan context.Context
	errs     chan error
}

func (outbound *contextCapturingOutbound) SendControl(
	ctx context.Context,
	_ protocolsession.Operation,
	_ protocolsession.MessageKind,
	_ []byte,
) (protocolsession.SendEvidence, error) {
	outbound.errs <- ctx.Err()
	outbound.contexts <- ctx
	return protocolsession.SendDelivered, nil
}

func (*contextCapturingOutbound) SendFragment(context.Context, protocolsession.Operation, protocolsession.Message) error {
	return nil
}

func (*contextCapturingOutbound) SendOperationError(context.Context, protocolsession.Operation, OperationFailure) error {
	return nil
}

func TestSenderHandlerCancelsOperationContextOnNormalCompletion(t *testing.T) {
	fixture := newRuntimeFixture(t, 1)
	defer fixture.close(t)
	outbound := &contextCapturingOutbound{
		contexts: make(chan context.Context, 1),
		errs:     make(chan error, 1),
	}
	handler, err := NewSenderHandler(SenderHandlerConfig{
		Service: fixture.service, Outbound: outbound, QueueDepth: 2, Workers: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stopRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- handler.Run(runCtx) }()
	defer func() {
		stopRun()
		<-runDone
	}()

	requestBody, _ := NewOpenRequest([]OpenItem{{FileID: fixture.file}})
	body, _ := EncodeOpenRequest(requestBody)
	operationID := protocolsession.OperationID(205)
	request := operationMessage(t, protocolsession.MessageOpenRevisions, operationID, body)
	op := newStubOperationState().admit(operationID, protocolsession.MessageOpenRevisions)
	if err := handler.HandleMessage(runCtx, op, request); err != nil {
		t.Fatalf("handle open request: %v", err)
	}

	var opCtx context.Context
	select {
	case opCtx = <-outbound.contexts:
	case <-time.After(time.Second):
		t.Fatal("open operation did not reach outbound")
	}
	if err := <-outbound.errs; err != nil {
		t.Fatalf("operation context was canceled before outbound completed: %v", err)
	}
	select {
	case <-opCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("completed operation context remained attached to live session parent")
	}
	if !errors.Is(opCtx.Err(), context.Canceled) {
		t.Fatalf("completed operation context err=%v want context.Canceled", opCtx.Err())
	}
	if runCtx.Err() != nil {
		t.Fatalf("session parent context unexpectedly ended: %v", runCtx.Err())
	}
}

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
	operationID := protocolsession.OperationID(201)
	request := operationMessage(t, protocolsession.MessageOpenRevisions, operationID, body)
	state := newStubOperationState()
	op := state.admit(operationID, protocolsession.MessageOpenRevisions)
	state.cancel(op.ID())
	if op.IsActive() || !op.IsCurrent() {
		t.Fatal("canceled operation did not become a current non-executable tombstone")
	}
	if err := handler.HandleMessage(context.Background(), op, request); err != nil {
		t.Fatalf("stale request suppression: %v", err)
	}
	if len(handler.queue) != 0 {
		t.Fatal("canceled request reached the service queue")
	}

	siblingID := protocolsession.OperationID(202)
	sibling := operationMessage(t, protocolsession.MessageOpenRevisions, siblingID, body)
	if err := handler.HandleMessage(context.Background(), state.admit(siblingID, protocolsession.MessageOpenRevisions), sibling); err != nil {
		t.Fatalf("sibling request: %v", err)
	}
	if len(handler.queue) != 1 {
		t.Fatal("canceled generation suppressed an unrelated sibling")
	}
}
