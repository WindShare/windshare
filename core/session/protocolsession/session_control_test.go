package protocolsession

import (
	"bytes"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/session/turncontrol"
)

func TestSessionControlSchemasCannotSelectAnotherFamily(t *testing.T) {
	bodies := sessionControlBodies(t)
	for kind, body := range bodies {
		if err := ValidateSessionControlBody(kind, body); err != nil {
			t.Fatal(err)
		}
		for otherKind := range bodies {
			if otherKind != kind && ValidateSessionControlBody(otherKind, body) == nil {
				t.Fatalf("kind %d accepted kind %d body", otherKind, kind)
			}
		}
	}
	// A future reachability version must fail in its own decoder, never enter TURN.
	futurePath := bytes.Clone(bodies[MessagePeerPathControl])
	futurePath[1] = byte(turncontrol.Schema)
	if err := ValidateSessionControlBody(MessagePeerPathControl, futurePath); !errors.Is(err, ErrPeerPathControl) {
		t.Fatalf("unsupported reachability version changed decoder: %v", err)
	}
	if err := ValidateSessionControlBody(MessageListChildren, bodies[MessageTURNControl]); !errors.Is(err, ErrUnknownMessageKind) {
		t.Fatalf("operation kind accepted as session control: %v", err)
	}
}

func sessionControlBodies(t *testing.T) map[MessageKind][]byte {
	t.Helper()
	path, err := EncodePeerPathControl(PeerPathControl{PeerPathID: [16]byte{1}, NetworkGenerationID: [16]byte{2},
		ControlSequence: 1, Kind: PeerPathRevoke})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := turncontrol.Encode(turncontrol.Available{
		Authority: turncontrol.Header{SessionID: [16]byte{2}, OwnerPathID: [16]byte{3}, Sequence: 1}, Profiles: []string{"primary"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return map[MessageKind][]byte{MessagePeerPathControl: path, MessageTURNControl: turn}
}
