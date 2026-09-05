package protocolsession

import (
	"errors"
	"testing"
)

func testPeerAttemptBinding() *PeerAttemptBinding {
	return &PeerAttemptBinding{PeerPathID: [16]byte{1}, AttemptID: [16]byte{2}, AttemptSequence: 1}
}

func TestPeerFailureBindingIsClosedAndScopeSpecific(t *testing.T) {
	binding := testPeerAttemptBinding()
	valid := OperationFailure{Scope: OperationScopePeer, Code: PeerOperationCodeTimeout, Message: "expired", PeerAttempt: binding}
	for name, mutate := range map[string]func(*OperationFailure){
		"missing":       func(f *OperationFailure) { f.PeerAttempt = nil },
		"zero path":     func(f *OperationFailure) { f.PeerAttempt.PeerPathID = [16]byte{} },
		"zero attempt":  func(f *OperationFailure) { f.PeerAttempt.AttemptID = [16]byte{} },
		"zero sequence": func(f *OperationFailure) { f.PeerAttempt.AttemptSequence = 0 },
		"other scope":   func(f *OperationFailure) { f.Scope = OperationScopeBlock; f.Code = blockOperationCodeFirst },
	} {
		t.Run(name, func(t *testing.T) {
			failure := valid
			copy := *binding
			failure.PeerAttempt = &copy
			mutate(&failure)
			if _, err := EncodeOperationFailure(failure); err == nil {
				t.Fatal("accepted invalid peer binding")
			}
		})
	}
	for _, malformed := range []any{nil, []any{}, []any{binding.PeerPathID[:], binding.AttemptID[:], uint64(0)}, []any{[]byte{1}, binding.AttemptID[:], uint64(1)}, []any{binding.PeerPathID[:], binding.AttemptID[:], "1"}} {
		body, err := EncodeBody(map[uint64]any{0: uint64(2), 1: uint64(5), 2: uint64(PeerOperationCodeTimeout), 3: false, 4: nil, 5: "expired", 6: malformed})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeOperationFailure(body); !errors.Is(err, ErrInvalidOperationFailure) {
			t.Fatalf("malformed binding accepted: %v", err)
		}
	}
}
