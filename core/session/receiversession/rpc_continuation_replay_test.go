package receiversession

import (
	"bytes"
	"crypto/sha256"

	"github.com/windshare/windshare/core/session/protocolsession"

	"time"
)

const continuationReplayTestTimeout = time.Second

type continuationReplayClassifier struct{}

func (continuationReplayClassifier) BeginOperationContinuation(
	kind protocolsession.MessageKind,
	body []byte,
) (protocolsession.OperationContinuationContract, bool, error) {
	if kind != protocolsession.MessagePeerOffer {
		return nil, false, nil
	}
	if !bytes.Equal(body, []byte{0xf6}) {
		return nil, true, protocolsession.ErrInvalidMessage
	}
	return continuationReplayAuthority{}, true, nil
}

func (continuationReplayClassifier) ClassifyUnboundOperationContinuation(
	kind protocolsession.MessageKind,
	body []byte,
) (protocolsession.OperationContinuationScope, bool, error) {
	if kind != protocolsession.MessagePeerCandidate {
		return protocolsession.OperationContinuationScope{}, false, nil
	}
	if !bytes.Equal(body, []byte{0xf6}) {
		return protocolsession.OperationContinuationScope{}, true, protocolsession.ErrInvalidMessage
	}
	return continuationReplayScope(), true, nil
}

type continuationReplayAuthority struct{}

func (continuationReplayAuthority) ClassifyOperationContinuation(
	kind protocolsession.MessageKind,
	body []byte,
) ([sha256.Size]byte, bool, error) {
	if kind != protocolsession.MessagePeerCandidate {
		return [sha256.Size]byte{}, false, nil
	}
	if !bytes.Equal(body, []byte{0xf6}) {
		return [sha256.Size]byte{}, true, protocolsession.ErrInvalidMessage
	}
	return sha256.Sum256(body), true, nil
}

func (continuationReplayAuthority) OperationContinuationScope() protocolsession.OperationContinuationScope {
	return continuationReplayScope()
}

func (continuationReplayAuthority) MaximumContinuations() int { return 4 }

func continuationReplayScope() protocolsession.OperationContinuationScope {
	return protocolsession.OperationContinuationScope(sha256.Sum256([]byte("continuation-replay-test")))
}
