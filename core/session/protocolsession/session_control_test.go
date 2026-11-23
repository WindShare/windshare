package protocolsession

import (
	"bytes"
	"errors"
	"testing"
)

func TestSessionControlSchemasRejectUnsupportedVersionsAndNonControlKinds(t *testing.T) {
	body, err := EncodePeerPathControl(PeerPathControl{
		PeerPathID: [16]byte{1}, NetworkGenerationID: [16]byte{2},
		ControlSequence: 1, Kind: PeerPathRevoke,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateSessionControlBody(RoleSender, MessagePeerPathControl, body); err != nil {
		t.Fatal(err)
	}
	futurePath := bytes.Clone(body)
	futurePath[1] = 4
	if err := ValidateSessionControlBody(RoleSender, MessagePeerPathControl, futurePath); !errors.Is(err, ErrPeerPathControl) {
		t.Fatalf("unsupported reachability version changed decoder: %v", err)
	}
	if err := ValidateSessionControlBody(RoleSender, MessageListChildren, body); !errors.Is(err, ErrUnknownMessageKind) {
		t.Fatalf("operation kind accepted as session control: %v", err)
	}
}
