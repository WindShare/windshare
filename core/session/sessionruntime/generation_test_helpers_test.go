package sessionruntime

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func senderIngressContext(t *testing.T, message protocolsession.Message) context.Context {
	t.Helper()
	operations, err := protocolsession.NewOperationTable(
		protocolsession.OperationLimits{MaxActive: 4, MaxTombstones: 4}, nil,
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

type testReceiverPeerSemantics struct {
	protocolsession.SenderControlSemanticValidator
}

func (semantics testReceiverPeerSemantics) BeginOperationContinuation(
	kind protocolsession.MessageKind,
	_ []byte,
) (protocolsession.OperationContinuationAuthority, bool, error) {
	if kind != protocolsession.MessagePeerOffer {
		return nil, false, nil
	}
	return testReceiverPeerContinuationAuthority{}, true, nil
}

func (semantics testReceiverPeerSemantics) ClassifyUnboundOperationContinuation(
	kind protocolsession.MessageKind,
	_ []byte,
) (protocolsession.OperationContinuationScope, bool, error) {
	if kind != protocolsession.MessagePeerCandidate {
		return protocolsession.OperationContinuationScope{}, false, nil
	}
	return testReceiverPeerContinuationScope(), true, nil
}

type testReceiverPeerContinuationAuthority struct{}

func (testReceiverPeerContinuationAuthority) ClassifyOperationContinuation(
	kind protocolsession.MessageKind,
	body []byte,
) ([sha256.Size]byte, bool, error) {
	if kind != protocolsession.MessagePeerCandidate {
		return [sha256.Size]byte{}, false, nil
	}
	return sha256.Sum256(body), true, nil
}

func (testReceiverPeerContinuationAuthority) OperationContinuationScope() protocolsession.OperationContinuationScope {
	return testReceiverPeerContinuationScope()
}

func (testReceiverPeerContinuationAuthority) MaximumContinuations() int { return 4 }

func testReceiverPeerContinuationScope() protocolsession.OperationContinuationScope {
	return protocolsession.OperationContinuationScope(sha256.Sum256([]byte("receiver-peer-test")))
}

func receiverPeerSemanticsForTest(
	validator protocolsession.SenderControlSemanticValidator,
) ReceiverPeerSemantics {
	return testReceiverPeerSemantics{SenderControlSemanticValidator: validator}
}
