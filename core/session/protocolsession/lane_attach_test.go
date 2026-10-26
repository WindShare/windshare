package protocolsession

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

func TestLaneAttachRequestPreservesAllocationAndReuseIdentities(t *testing.T) {
	for _, requested := range []uint32{0, 7, ^uint32(0)} {
		body, err := EncodeLaneAttachRequest(requested)
		if err != nil {
			t.Fatal(err)
		}
		request, err := DecodeLaneAttachRequest(body)
		if err != nil || request.RequestedLaneID != requested {
			t.Fatalf("request %d decoded as %+v: %v", requested, request, err)
		}
		if requested == 7 && hex.EncodeToString(body) != "a3000101000207" {
			t.Fatalf("request wire representation changed: %x", body)
		}
	}
}

func TestLaneAttachRequestRejectsMalformedAndAlternateCBOR(t *testing.T) {
	for name, encoded := range map[string]string{
		"empty":               "",
		"null":                "f6",
		"extra field":         "a40001010002070300",
		"missing field":       "a200010100",
		"wrong version":       "a3000201000207",
		"grant disposition":   "a3000101010207",
		"noninteger identity": "a300010100024107",
		"identity overflow":   "a300010100021b0000000100000000",
		"negative identity":   "a3000101000220",
		"duplicate key":       "a40001010002070207",
		"indefinite map":      "bf000101000207ff",
		"tagged map":          "c0a3000101000207",
		"nonminimal integer":  "a300180101000207",
		"unordered keys":      "a3020701000001",
	} {
		t.Run(name, func(t *testing.T) {
			body, err := hex.DecodeString(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeLaneAttachRequest(body); !errors.Is(err, ErrLaneMalformed) {
				t.Fatalf("decode malformed request: %v", err)
			}
		})
	}
}

func loadLaneGrantVector(t *testing.T) (laneControlVector, LaneGrant) {
	t.Helper()
	var vector laneControlVector
	decodeSessionCase(t, "sender-granted-lane-attach", &vector)
	operationID, err := OperationIDFromBytes(decodeB64(t, vector.GrantOperationIDB64))
	if err != nil {
		t.Fatal(err)
	}
	grant := LaneGrant{LaneID: vector.AttachedLaneID, LaneEpoch: vector.AttachedLaneEpoch, OperationID: operationID}
	copy(grant.AttachNonce[:], decodeB64(t, vector.AttachNonceB64))
	return vector, grant
}

func TestLaneGrantCodecMatchesSharedWireVector(t *testing.T) {
	vector, grant := loadLaneGrantVector(t)
	want := decodeB64(t, vector.GrantSemanticBodyB64)
	body, err := EncodeLaneGrant(grant)
	if err != nil || !bytes.Equal(body, want) {
		t.Fatalf("grant wire representation = %x, want %x: %v", body, want, err)
	}
	decoded, err := DecodeLaneGrant(want, grant.OperationID)
	if err != nil || decoded != grant {
		t.Fatalf("decoded grant = %+v, want %+v: %v", decoded, grant, err)
	}
	otherOperation := testOperationID(91)
	decoded, err = DecodeLaneGrant(want, otherOperation)
	if err != nil || decoded.OperationID != otherOperation {
		t.Fatalf("grant did not bind its enclosing operation: %+v, %v", decoded, err)
	}
	if _, err := DecodeLaneGrant(want, OperationID{}); !errors.Is(err, ErrInvalidOperationID) {
		t.Fatalf("missing enclosing operation: %v", err)
	}
}

func TestLaneGrantRejectsInvalidFields(t *testing.T) {
	_, grant := loadLaneGrantVector(t)
	for _, test := range []struct {
		name  string
		key   uint64
		value any
	}{
		{"version", 0, uint64(2)},
		{"request disposition", 1, uint64(0)},
		{"zero lane", 2, uint64(0)},
		{"lane overflow", 2, uint64(1) << 32},
		{"lane type", 2, []byte{1}},
		{"zero epoch", 3, uint64(0)},
		{"epoch overflow", 3, uint64(1) << 32},
		{"epoch type", 3, []byte{1}},
		{"zero nonce", 4, make([]byte, LaneAttachNonceBytes)},
		{"short nonce", 4, []byte{1}},
		{"nonce type", 4, uint64(1)},
		{"extra field", 5, uint64(0)},
	} {
		t.Run(test.name, func(t *testing.T) {
			fields := map[uint64]any{0: uint64(1), 1: uint64(1), 2: uint64(grant.LaneID), 3: uint64(grant.LaneEpoch), 4: grant.AttachNonce[:]}
			fields[test.key] = test.value
			if _, err := DecodeLaneGrant(mustControlBody(t, fields), grant.OperationID); !errors.Is(err, ErrLaneMalformed) {
				t.Fatalf("decode malformed grant: %v", err)
			}
		})
	}
	for name, mutate := range map[string]func(*LaneGrant){
		"zero lane":  func(value *LaneGrant) { value.LaneID = 0 },
		"zero epoch": func(value *LaneGrant) { value.LaneEpoch = 0 },
		"zero nonce": func(value *LaneGrant) { clear(value.AttachNonce[:]) },
	} {
		t.Run(name+" input", func(t *testing.T) {
			invalid := grant
			mutate(&invalid)
			if _, err := EncodeLaneGrant(invalid); !errors.Is(err, ErrLaneInput) {
				t.Fatalf("encode invalid grant: %v", err)
			}
		})
	}
	grant.OperationID = OperationID{}
	if _, err := EncodeLaneGrant(grant); !errors.Is(err, ErrInvalidOperationID) {
		t.Fatalf("encode grant without operation: %v", err)
	}
}

func TestLaneGrantAuthenticationOwnsSchemaWithoutRuntimeRegistration(t *testing.T) {
	vector, grant := loadLaneGrantVector(t)
	sessionID, err := ProtocolSessionIDFromBytes(decodeB64(t, vector.ProtocolSessionIDB64))
	if err != nil {
		t.Fatal(err)
	}
	base := ControlBinding{
		ShareInstance: mustShare(t, decodeB64(t, vector.ShareInstanceB64)), ProtocolSessionID: sessionID,
		LaneID: vector.GrantLaneID, LaneEpoch: vector.GrantLaneEpoch, Direction: DirectionSenderToReceiver,
	}
	signingKey := vectorSenderSigningKey()
	sequence := parseSequence(t, vector.GrantSequence)
	message, err := DecodeMessage(decodeB64(t, vector.GrantPlaintextB64))
	if err != nil {
		t.Fatal(err)
	}
	binding := base
	binding.Sequence, binding.MessageKind = sequence, MessageLaneAttach
	binding.OperationID, binding.HasOperationID = grant.OperationID, true
	malformed := mustControlBody(t, map[uint64]any{0: uint64(1), 1: uint64(1), 2: uint64(0), 3: uint64(1), 4: grant.AttachNonce[:]})
	signed, err := SignControlBody(signingKey, ControlDomainLaneAttach, binding, malformed)
	if err != nil {
		t.Fatal(err)
	}
	invalidMessage, err := NewMessage(MessageLaneAttach, &grant.OperationID, signed)
	if err != nil {
		t.Fatal(err)
	}
	external := SenderControlSemanticValidatorFunc(func(MessageKind, OperationID, []byte) error {
		t.Fatal("lane authorization schema delegated to runtime")
		return nil
	})
	for _, validator := range []SenderControlSemanticValidator{nil, external} {
		authenticator, err := NewSenderControlAuthenticator(checkedSender(t, signingKey.Public().(ed25519.PublicKey)), base, validator)
		if err != nil {
			t.Fatal(err)
		}
		if err := authenticator.Verify(sequence, message); err != nil {
			t.Fatalf("valid signed grant: %v", err)
		}
		if err := authenticator.Verify(sequence, invalidMessage); !errors.Is(err, ErrControlSemantic) || !errors.Is(err, ErrLaneMalformed) {
			t.Fatalf("signed malformed grant: %v", err)
		}
	}
}

func TestLaneGrantRoundTripPreservesRegistryRevocationAuthority(t *testing.T) {
	now := time.Unix(100, 0)
	registry, _, _, _, _ := newLifecycleLaneRegistry(t, &now)
	grant, err := registry.IssueGrant(0, testOperationID(92), laneSequence(93, LaneAttachNonceBytes))
	if err != nil {
		t.Fatal(err)
	}
	body, err := EncodeLaneGrant(grant)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeLaneGrant(body, grant.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if !registry.RevokeGrant(decoded) {
		t.Fatal("wire grant lost its exact registry identity")
	}
}
