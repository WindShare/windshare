package sendersession

import (
	"errors"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
	"testing"
)

func TestSenderPeerFunctionAdapterRejectsNilContinuationClassifier(t *testing.T) {
	var nilFactory PeerHandlerFactoryFunc
	if _, _, err := nilFactory.BeginOperationContinuation(
		protocolsession.MessagePeerOffer,
		[]byte{0xf6},
	); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
		t.Fatalf("nil begin classifier error=%v", err)
	}
	if _, _, err := nilFactory.ClassifyUnboundOperationContinuation(
		protocolsession.MessagePeerCandidate,
		[]byte{0xf6},
	); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
		t.Fatalf("nil unbound classifier error=%v", err)
	}

	factory := PeerHandlerFactoryFunc(func(PeerSession) (PeerHandler, error) {
		return inertSenderPeerHandler{}, nil
	})
	if _, recognized, err := factory.ClassifyUnboundOperationContinuation(
		protocolsession.MessagePeerCandidate,
		[]byte{0xf6},
	); err != nil || recognized {
		t.Fatalf("function adapter unbound classification recognized=%t error=%v", recognized, err)
	}
}
