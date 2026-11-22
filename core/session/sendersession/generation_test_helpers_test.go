package sendersession

import (
	"sync"
	"testing"

	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type stubOperationState struct {
	mu      sync.Mutex
	next    uint64
	active  map[protocolsession.OperationRef]protocolsession.MessageKind
	current map[protocolsession.OperationRef]protocolsession.MessageKind
}

func newStubOperationState() *stubOperationState {
	return &stubOperationState{
		active:  make(map[protocolsession.OperationRef]protocolsession.MessageKind),
		current: make(map[protocolsession.OperationRef]protocolsession.MessageKind),
	}
}

func (s *stubOperationState) admit(id protocolsession.OperationID, kind protocolsession.MessageKind) protocolsession.Operation {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	ref := protocolsession.OperationRef{ID: id, Token: s.next}
	s.active[ref] = kind
	s.current[ref] = kind
	return protocolsession.NewOperation(ref, s)
}

func (s *stubOperationState) cancel(ref protocolsession.OperationRef) {
	s.mu.Lock()
	delete(s.active, ref)
	s.mu.Unlock()
}

func (s *stubOperationState) Active(ref protocolsession.OperationRef) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.active[ref]
	return ok
}

func (s *stubOperationState) Current(ref protocolsession.OperationRef) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.current[ref]
	return ok
}

func (s *stubOperationState) RequestKind(ref protocolsession.OperationRef) (protocolsession.MessageKind, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kind, ok := s.current[ref]
	return kind, ok
}

func (s *stubOperationState) PeerAttemptBinding(protocolsession.OperationRef) (protocolsession.PeerAttemptBinding, bool) {
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
