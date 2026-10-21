package protocolsession

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
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

func TestSessionControlSignaturesBindKindAndValidateItsSchema(t *testing.T) {
	key := vectorSenderSigningKey()
	base := ControlBinding{ShareInstance: catalog.ShareInstance{1}, ProtocolSessionID: ProtocolSessionID{2},
		LaneID: 3, LaneEpoch: 4, Direction: DirectionSenderToReceiver}
	authenticator, err := NewSenderControlAuthenticator(checkedSender(t, key.Public().(ed25519.PublicKey)), base, nil)
	if err != nil {
		t.Fatal(err)
	}
	bodies := sessionControlBodies(t)
	for kind, body := range bodies {
		domain := ControlDomainPeerPath
		otherKind := MessageTURNControl
		if kind == MessageTURNControl {
			domain, otherKind = ControlDomainTURN, MessagePeerPathControl
		}
		binding := base
		binding.MessageKind, binding.Sequence = kind, 7
		signed, err := SignControlBody(key, domain, binding, body)
		if err != nil {
			t.Fatal(err)
		}
		message, err := NewMessage(kind, nil, signed)
		if err != nil {
			t.Fatal(err)
		}
		if err := authenticator.Verify(binding.Sequence, message); err != nil {
			t.Fatalf("valid control kind %d: %v", kind, err)
		}
		for _, mutate := range []func(*Message){
			func(m *Message) { m.kind = otherKind },
			func(m *Message) { m.body = bytes.Clone(signed); m.body[len(m.body)-1] ^= 1 },
			func(m *Message) { m.body = body },
		} {
			tampered := message
			mutate(&tampered)
			if err := authenticator.Verify(binding.Sequence, tampered); err == nil {
				t.Fatalf("kind %d accepted a substituted or unsigned control", kind)
			}
		}
		wrongBody, err := SignControlBody(key, domain, binding, bodies[otherKind])
		if err != nil {
			t.Fatal(err)
		}
		message, err = NewMessage(kind, nil, wrongBody)
		if err != nil {
			t.Fatal(err)
		}
		if err := authenticator.Verify(binding.Sequence, message); !errors.Is(err, ErrControlSemantic) {
			t.Fatalf("signed kind %d accepted another family's schema: %v", kind, err)
		}
		binding.MessageKind = otherKind
		if _, err := SignControlBody(key, domain, binding, body); !errors.Is(err, ErrControlBinding) {
			t.Fatalf("signature domain accepted another kind: %v", err)
		}
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
