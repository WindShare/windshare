package sendersession

import (
	"context"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"testing"
)

func senderIngressContext(t *testing.T, message protocolsession.Message) context.Context {
	t.Helper()
	operations, err := protocolsession.NewOperationTable(
		protocolsession.OperationLimits{MaxActive: 4, MaxTracked: 4}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := operations.ObserveInbound(protocolsession.DirectionReceiverToSender, message)
	if err != nil || admission.Disposition != protocolsession.OperationDeliver {
		t.Fatalf("admit sender ingress: disposition=%d error=%v", admission.Disposition, err)
	}
	return protocolsession.WithOperationGeneration(context.Background(), admission.Generation)
}

func operationMessageForTest(
	t *testing.T,
	kind protocolsession.MessageKind,
	operationID protocolsession.OperationID,
	body []byte,
) protocolsession.Message {
	t.Helper()
	message, err := protocolsession.NewMessage(kind, &operationID, body)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func validCancelBody(t *testing.T) []byte {
	t.Helper()
	body, err := contentflow.EncodeCancelReason(contentflow.CancelReasonOutputAbort)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
