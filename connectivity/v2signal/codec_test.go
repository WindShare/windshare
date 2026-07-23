package v2signal

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/fxamacker/cbor/v2"
)

func TestFrozenPeerSignalingBodiesAndOperationKinds(t *testing.T) {
	var vector struct {
		SchemaVersion uint64 `json:"schemaVersion"`
		MessageKinds  struct {
			Offer     uint8 `json:"offer"`
			Answer    uint8 `json:"answer"`
			Candidate uint8 `json:"candidate"`
		} `json:"messageKinds"`
		Offer struct {
			BodyBase64 string `json:"bodyB64"`
		} `json:"offer"`
		Answer struct {
			BodyBase64 string `json:"bodyB64"`
		} `json:"answer"`
		Candidate struct {
			BodyBase64 string `json:"bodyB64"`
		} `json:"candidate"`
	}
	encodedVector, err := os.ReadFile(filepath.Join("..", "..", "core", "testvectors", "v2-peer-signaling.json"))
	if err != nil {
		t.Fatalf("load shared vector: %v", err)
	}
	if err := json.Unmarshal(encodedVector, &vector); err != nil {
		t.Fatalf("decode shared vector: %v", err)
	}
	if vector.SchemaVersion != SignalingSchemaVersion ||
		vector.MessageKinds.Offer != uint8(MessagePeerOffer) ||
		vector.MessageKinds.Answer != uint8(MessagePeerAnswer) ||
		vector.MessageKinds.Candidate != uint8(MessagePeerCandidate) {
		t.Fatal("shared signaling kind registry changed")
	}
	binding := testBinding()
	mid := "data"
	line := uint16(0)
	username := "windshare"
	offer := Offer{Binding: binding, SDP: "v=0\r\na=setup:actpass\r\n"}
	answer := Answer{Binding: binding, SDP: "v=0\r\na=setup:active\r\n"}
	candidate := Candidate{
		Binding: binding, Candidate: "candidate:1 1 udp 2122260223 192.0.2.1 5000 typ host",
		SDPMid: &mid, SDPMLineIndex: &line, UsernameFragment: &username,
	}
	tests := []struct {
		name       string
		kind       MessageKind
		bodyBase64 string
		encode     func() ([]byte, error)
		decode     func([]byte) error
	}{
		{"offer", MessagePeerOffer, vector.Offer.BodyBase64, func() ([]byte, error) { return EncodeOffer(offer) }, func(encoded []byte) error {
			decoded, err := DecodeOffer(encoded)
			if err == nil && decoded != offer {
				t.Fatal("offer round trip changed")
			}
			return err
		}},
		{"answer", MessagePeerAnswer, vector.Answer.BodyBase64, func() ([]byte, error) { return EncodeAnswer(answer) }, func(encoded []byte) error {
			decoded, err := DecodeAnswer(encoded)
			if err == nil && decoded != answer {
				t.Fatal("answer round trip changed")
			}
			return err
		}},
		{"candidate", MessagePeerCandidate, vector.Candidate.BodyBase64, func() ([]byte, error) { return EncodeCandidate(candidate) }, func(encoded []byte) error {
			decoded, err := DecodeCandidate(encoded)
			if err == nil && (decoded.Binding != candidate.Binding || decoded.Candidate != candidate.Candidate ||
				*decoded.SDPMid != mid || *decoded.SDPMLineIndex != line || *decoded.UsernameFragment != username) {
				t.Fatal("candidate round trip changed")
			}
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := test.encode()
			if err != nil {
				t.Fatal(err)
			}
			if got := base64.StdEncoding.EncodeToString(encoded); got != test.bodyBase64 {
				t.Fatalf("body base64 = %s", got)
			}
			if err := test.decode(encoded); err != nil {
				t.Fatal(err)
			}
			if got := []MessageKind{offer.Kind(), answer.Kind(), candidate.Kind()}[int(test.kind-MessagePeerOffer)]; got != test.kind {
				t.Fatalf("operation kind = %d, want %d", got, test.kind)
			}
		})
	}
}

func TestPeerSignalingRejectsHostileBodiesAndBindingSubstitution(t *testing.T) {
	binding := testBinding()
	other := binding
	other.AttemptID[0] ^= 0xff
	if err := binding.RequireSame(other); !errors.Is(err, ErrSignalBinding) {
		t.Fatalf("binding substitution error = %v", err)
	}
	if err := binding.RequireSame(binding); err != nil || binding.String() == "" {
		t.Fatalf("stable binding = %q, %v", binding, err)
	}
	if _, err := EncodeOffer(Offer{Binding: binding, SDP: ""}); !errors.Is(err, ErrInvalidSignal) {
		t.Fatalf("empty SDP error = %v", err)
	}
	if _, err := EncodeAnswer(Answer{Binding: binding, SDP: "e\u0301"}); !errors.Is(err, ErrInvalidSignal) {
		t.Fatalf("non-NFC SDP error = %v", err)
	}
	if _, err := EncodeCandidate(Candidate{Binding: binding}); !errors.Is(err, ErrInvalidSignal) {
		t.Fatalf("empty candidate error = %v", err)
	}

	valid, _ := EncodeOffer(Offer{Binding: binding, SDP: "v=0"})
	nonCanonical := append([]byte{0x98, 0x04}, valid[1:]...)
	if _, err := DecodeOffer(nonCanonical); !errors.Is(err, ErrNonCanonicalSignal) {
		t.Fatalf("noncanonical array error = %v", err)
	}
	var fields []cbor.RawMessage
	if err := signalDecMode.Unmarshal(valid, &fields); err != nil {
		t.Fatal(err)
	}
	zeroPath, _ := signalEncMode.Marshal(make([]byte, IdentityBytes))
	fields[1] = zeroPath
	hostile, _ := signalEncMode.Marshal(fields)
	if _, err := DecodeOffer(hostile); !errors.Is(err, ErrInvalidSignal) {
		t.Fatalf("zero path error = %v", err)
	}

	candidate, _ := EncodeCandidate(Candidate{Binding: binding, Candidate: "candidate:1", SDPMid: nil})
	if _, err := DecodeCandidate(candidate[:len(candidate)-1]); !errors.Is(err, ErrInvalidSignal) {
		t.Fatalf("truncated candidate error = %v", err)
	}
	extra := append(bytes.Clone(candidate), 0)
	if _, err := DecodeCandidate(extra); !errors.Is(err, ErrInvalidSignal) {
		t.Fatalf("trailing candidate error = %v", err)
	}
}

func testBinding() Binding {
	var binding Binding
	for index := range binding.PeerPathID {
		binding.PeerPathID[index] = byte(index + 1)
		binding.AttemptID[index] = byte(index + 0x21)
	}
	return binding
}
