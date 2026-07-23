package v2signal

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"flag"
	"os"
	"strconv"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/session/protocolsession"
)

var updatePeerSignalingVector = flag.Bool("update", false, "regenerate the v2 peer-signaling vector")

const peerSignalingVectorPath = "../../core/testvectors/v2-peer-signaling.json"

type peerSignalingVector struct {
	SchemaVersion      uint64                   `json:"schemaVersion"`
	MessageKinds       peerMessageKinds         `json:"messageKinds"`
	PeerPathIDB64      string                   `json:"peerPathIdB64"`
	AttemptIDB64       string                   `json:"attemptIdB64"`
	SenderPublicKeyB64 string                   `json:"senderPublicKeyB64"`
	ControlBinding     peerControlBindingVector `json:"controlBinding"`
	Offer              peerDescriptionVector    `json:"offer"`
	Answer             peerDescriptionVector    `json:"answer"`
	Candidate          peerCandidateVector      `json:"candidate"`
}

type peerMessageKinds struct {
	Offer     uint8 `json:"offer"`
	Answer    uint8 `json:"answer"`
	Candidate uint8 `json:"candidate"`
}

type peerControlBindingVector struct {
	ShareInstanceB64     string `json:"shareInstanceB64"`
	ProtocolSessionIDB64 string `json:"protocolSessionIdB64"`
	LaneID               uint32 `json:"laneId"`
	LaneEpoch            uint32 `json:"laneEpoch"`
	Direction            uint8  `json:"direction"`
	OperationIDB64       string `json:"operationIdB64"`
}

type peerDescriptionVector struct {
	SDP                    string `json:"sdp"`
	BodyB64                string `json:"bodyB64"`
	Sequence               string `json:"sequence,omitempty"`
	UnsignedWrapperCBORB64 string `json:"unsignedWrapperCborB64,omitempty"`
	ControlPreimageB64     string `json:"controlPreimageB64,omitempty"`
	ControlSignatureB64    string `json:"controlSignatureB64,omitempty"`
	SignedBodyB64          string `json:"signedBodyB64,omitempty"`
}

type peerCandidateVector struct {
	Candidate              string  `json:"candidate"`
	SDPMid                 *string `json:"sdpMid"`
	SDPMLineIndex          *uint16 `json:"sdpMLineIndex"`
	UsernameFragment       *string `json:"usernameFragment"`
	BodyB64                string  `json:"bodyB64"`
	Sequence               string  `json:"sequence"`
	UnsignedWrapperCBORB64 string  `json:"unsignedWrapperCborB64"`
	ControlPreimageB64     string  `json:"controlPreimageB64"`
	ControlSignatureB64    string  `json:"controlSignatureB64"`
	SignedBodyB64          string  `json:"signedBodyB64"`
}

type peerControlBytes struct {
	sequence        uint64
	unsignedWrapper []byte
	preimage        []byte
	signature       []byte
	signedBody      []byte
}

func TestPeerSignalingVectorUpToDate(t *testing.T) {
	vector := buildPeerSignalingVector(t)
	encoded, err := json.MarshalIndent(vector, "", "  ")
	if err != nil {
		t.Fatalf("encode peer-signaling vector: %v", err)
	}
	encoded = append(encoded, '\n')
	if *updatePeerSignalingVector {
		if err := os.WriteFile(peerSignalingVectorPath, encoded, 0o644); err != nil {
			t.Fatalf("write peer-signaling vector: %v", err)
		}
	}
	committed, err := os.ReadFile(peerSignalingVectorPath)
	if err != nil {
		t.Fatalf("read peer-signaling vector: %v", err)
	}
	if !bytes.Equal(committed, encoded) {
		t.Fatalf("%s is stale; run go test ./connectivity/v2signal -update", peerSignalingVectorPath)
	}
}

func buildPeerSignalingVector(t *testing.T) peerSignalingVector {
	t.Helper()
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
	offerBody, err := EncodeOffer(offer)
	if err != nil {
		t.Fatal(err)
	}
	answerBody, err := EncodeAnswer(answer)
	if err != nil {
		t.Fatal(err)
	}
	candidateBody, err := EncodeCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}

	signingKey := ed25519.NewKeyFromSeed(peerVectorBytes(0xa1, ed25519.SeedSize))
	share := mustPeerShare(t, peerVectorBytes(0x41, protocolsession.IdentityBytes))
	sessionID := mustPeerSession(t, peerVectorBytes(0x61, protocolsession.IdentityBytes))
	operationID := mustPeerOperation(t, peerVectorBytes(0x81, protocolsession.IdentityBytes))
	base := protocolsession.ControlBinding{
		ShareInstance: share, ProtocolSessionID: sessionID, LaneID: 0x01020304, LaneEpoch: 2,
		Direction: protocolsession.DirectionSenderToReceiver,
	}
	answerControl := buildPeerControl(t, signingKey, base, protocolsession.MessagePeerAnswer, operationID, 9, answerBody)
	candidateControl := buildPeerControl(t, signingKey, base, protocolsession.MessagePeerCandidate, operationID, 10, candidateBody)

	return peerSignalingVector{
		SchemaVersion: SignalingSchemaVersion,
		MessageKinds: peerMessageKinds{
			Offer: uint8(MessagePeerOffer), Answer: uint8(MessagePeerAnswer), Candidate: uint8(MessagePeerCandidate),
		},
		PeerPathIDB64:      base64.StdEncoding.EncodeToString(binding.PeerPathID[:]),
		AttemptIDB64:       base64.StdEncoding.EncodeToString(binding.AttemptID[:]),
		SenderPublicKeyB64: base64.StdEncoding.EncodeToString(signingKey.Public().(ed25519.PublicKey)),
		ControlBinding: peerControlBindingVector{
			ShareInstanceB64:     base64.StdEncoding.EncodeToString(share.Bytes()),
			ProtocolSessionIDB64: base64.StdEncoding.EncodeToString(sessionID.Bytes()),
			LaneID:               base.LaneID, LaneEpoch: base.LaneEpoch, Direction: uint8(base.Direction),
			OperationIDB64: base64.StdEncoding.EncodeToString(operationID.Bytes()),
		},
		Offer: peerDescriptionVector{SDP: offer.SDP, BodyB64: base64.StdEncoding.EncodeToString(offerBody)},
		Answer: peerDescriptionVector{
			SDP: answer.SDP, BodyB64: base64.StdEncoding.EncodeToString(answerBody),
			Sequence:               strconv.FormatUint(answerControl.sequence, 10),
			UnsignedWrapperCBORB64: base64.StdEncoding.EncodeToString(answerControl.unsignedWrapper),
			ControlPreimageB64:     base64.StdEncoding.EncodeToString(answerControl.preimage),
			ControlSignatureB64:    base64.StdEncoding.EncodeToString(answerControl.signature),
			SignedBodyB64:          base64.StdEncoding.EncodeToString(answerControl.signedBody),
		},
		Candidate: peerCandidateVector{
			Candidate: candidate.Candidate, SDPMid: candidate.SDPMid, SDPMLineIndex: candidate.SDPMLineIndex,
			UsernameFragment: candidate.UsernameFragment, BodyB64: base64.StdEncoding.EncodeToString(candidateBody),
			Sequence:               strconv.FormatUint(candidateControl.sequence, 10),
			UnsignedWrapperCBORB64: base64.StdEncoding.EncodeToString(candidateControl.unsignedWrapper),
			ControlPreimageB64:     base64.StdEncoding.EncodeToString(candidateControl.preimage),
			ControlSignatureB64:    base64.StdEncoding.EncodeToString(candidateControl.signature),
			SignedBodyB64:          base64.StdEncoding.EncodeToString(candidateControl.signedBody),
		},
	}
}

func buildPeerControl(
	t *testing.T,
	signingKey ed25519.PrivateKey,
	base protocolsession.ControlBinding,
	kind protocolsession.MessageKind,
	operationID protocolsession.OperationID,
	sequence uint64,
	semanticBody []byte,
) peerControlBytes {
	t.Helper()
	binding := base
	binding.Sequence = sequence
	binding.MessageKind = kind
	binding.OperationID = operationID
	binding.HasOperationID = true
	unsignedWrapper := mustPeerCanonical(t, map[uint64]any{
		0: uint64(1), 1: cbor.RawMessage(semanticBody),
	})
	preimage, err := protocolsession.ControlSignaturePreimage(
		protocolsession.ControlDomainOperation, binding, semanticBody,
	)
	if err != nil {
		t.Fatalf("build peer control preimage: %v", err)
	}
	signature := ed25519.Sign(signingKey, preimage)
	signedBody, err := protocolsession.SignControlBody(
		signingKey, protocolsession.ControlDomainOperation, binding, semanticBody,
	)
	if err != nil {
		t.Fatalf("sign peer control: %v", err)
	}
	verified, err := protocolsession.VerifyControlBody(
		signingKey.Public().(ed25519.PublicKey), protocolsession.ControlDomainOperation, binding, signedBody,
	)
	if err != nil || !bytes.Equal(verified, semanticBody) {
		t.Fatalf("verify peer control: semantic body changed: %v", err)
	}
	return peerControlBytes{
		sequence: sequence, unsignedWrapper: unsignedWrapper, preimage: preimage,
		signature: signature, signedBody: signedBody,
	}
}

func mustPeerCanonical(t *testing.T, value any) []byte {
	t.Helper()
	mode, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := mode.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func mustPeerShare(t *testing.T, raw []byte) catalog.ShareInstance {
	t.Helper()
	value, err := catalog.ShareInstanceFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustPeerSession(t *testing.T, raw []byte) protocolsession.ProtocolSessionID {
	t.Helper()
	value, err := protocolsession.ProtocolSessionIDFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustPeerOperation(t *testing.T, raw []byte) protocolsession.OperationID {
	t.Helper()
	value, err := protocolsession.OperationIDFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func peerVectorBytes(first byte, count int) []byte {
	result := make([]byte, count)
	for index := range result {
		result[index] = first + byte(index)
	}
	return result
}
