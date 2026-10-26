package receiversession

import (
	"crypto/sha256"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type testReceiverPeerSemantics struct {
	protocolsession.SenderControlSemanticValidator
}

func (semantics testReceiverPeerSemantics) BeginOperationContinuation(
	kind protocolsession.MessageKind,
	_ []byte,
) (protocolsession.OperationContinuationContract, bool, error) {
	if kind != protocolsession.MessagePeerOffer {
		return nil, false, nil
	}
	return testReceiverPeerContinuationContract{}, true, nil
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

type testReceiverPeerContinuationContract struct{}

func (testReceiverPeerContinuationContract) ClassifyOperationContinuation(
	kind protocolsession.MessageKind,
	body []byte,
) ([sha256.Size]byte, bool, error) {
	if kind != protocolsession.MessagePeerCandidate {
		return [sha256.Size]byte{}, false, nil
	}
	return sha256.Sum256(body), true, nil
}

func (testReceiverPeerContinuationContract) OperationContinuationScope() protocolsession.OperationContinuationScope {
	return testReceiverPeerContinuationScope()
}

func (testReceiverPeerContinuationContract) MaximumContinuations() int { return 4 }

func testReceiverPeerContinuationScope() protocolsession.OperationContinuationScope {
	return protocolsession.OperationContinuationScope(sha256.Sum256([]byte("receiver-peer-test")))
}

func receiverPeerSemanticsForTest(
	validator protocolsession.SenderControlSemanticValidator,
) PeerSemantics {
	return testReceiverPeerSemantics{SenderControlSemanticValidator: validator}
}
