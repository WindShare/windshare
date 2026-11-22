package sendersession

import (
	"sync"
	"testing"

	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type stubOperationState struct {
	mu      sync.Mutex
	active  map[protocolsession.OperationID]protocolsession.MessageKind
	current map[protocolsession.OperationID]protocolsession.MessageKind
}

func newStubOperationState() *stubOperationState {
	return &stubOperationState{
		active:  make(map[protocolsession.OperationID]protocolsession.MessageKind),
		current: make(map[protocolsession.OperationID]protocolsession.MessageKind),
	}
}

func (s *stubOperationState) admit(id protocolsession.OperationID, kind protocolsession.MessageKind) protocolsession.Operation {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active[id] = kind
	s.current[id] = kind
	return protocolsession.NewOperation(id, s)
}

func (s *stubOperationState) cancel(id protocolsession.OperationID) {
	s.mu.Lock()
	delete(s.active, id)
	s.mu.Unlock()
}

func (s *stubOperationState) Active(id protocolsession.OperationID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.active[id]
	return ok
}

func (s *stubOperationState) Current(id protocolsession.OperationID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.current[id]
	return ok
}

func (s *stubOperationState) RequestKind(id protocolsession.OperationID) (protocolsession.MessageKind, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kind, ok := s.current[id]
	return kind, ok
}

func (s *stubOperationState) PeerAttemptBinding(protocolsession.OperationID) (protocolsession.PeerAttemptBinding, bool) {
	return protocolsession.PeerAttemptBinding{}, false
}

func senderIngressOperation(t *testing.T, message protocolsession.Message) protocolsession.Operation {
	t.Helper()
	id, ok := message.OperationID()
	if !ok {
		t.Fatal("missing operation id")
	}
	return newStubOperationState().admit(id, message.Kind())
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
