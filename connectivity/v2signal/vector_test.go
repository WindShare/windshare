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

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/session/protocolsession"
)

var updatePeerSignalingVector = flag.Bool("update", false, "regenerate the v2 peer-signaling vector")

const (
	peerSignalingVectorPath = "../../core/testvectors/v2-peer-signaling.json"
	updateVectorsCommand    = "make vectors-update"
)

type peerSignalingVector struct {
	PeerSlot           uint8                    `json:"peerSlot"`
	PeerErrors         []peerBoundErrorVector   `json:"peerErrors"`
	AttemptSequence    string                   `json:"attemptSequence"`
	FailureScopes      []peerFailureVector      `json:"failureScopes"`
	PathControls       []peerPathControlVector  `json:"pathControls"`
	SchemaVersion      uint64                   `json:"schemaVersion"`
	MessageKinds       peerMessageKinds         `json:"messageKinds"`
	PeerPathIDB64      string                   `json:"peerPathIdB64"`
	AttemptIDB64       string                   `json:"attemptIdB64"`
	SenderPublicKeyB64 string                   `json:"senderPublicKeyB64"`
	ControlBinding     peerControlBindingVector `json:"controlBinding"`
	TURNOffer          peerDescriptionVector    `json:"turnOffer"`
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
	OperationID          string `json:"operationId"`
}

type peerDescriptionVector struct {
	SDP     string `json:"sdp"`
	BodyB64 string `json:"bodyB64"`
}

type peerCandidateVector struct {
	Candidate        string  `json:"candidate"`
	SDPMid           *string `json:"sdpMid"`
	SDPMLineIndex    *uint16 `json:"sdpMLineIndex"`
	UsernameFragment *string `json:"usernameFragment"`
	BodyB64          string  `json:"bodyB64"`
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
		t.Fatalf("%s is stale; run %s from the repository root", peerSignalingVectorPath, updateVectorsCommand)
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
	turnOffer := offer
	turnOffer.Binding.Purpose = TURNRelay
	turnOffer.TURNGrantID = protocolsession.TURNGrantID{1}
	turnBody, err := EncodeOffer(turnOffer)
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
	operationID := protocolsession.OperationID(0x0102030405060708)
	base := protocolsession.EnvelopeBinding{
		ShareInstance: share, ProtocolSessionID: sessionID, LaneID: 0x01020304, LaneEpoch: 2,
		Direction: protocolsession.DirectionSenderToReceiver,
	}

	return peerSignalingVector{
		SchemaVersion:   SignalingSchemaVersion,
		AttemptSequence: strconv.FormatUint(binding.AttemptSequence, 10),
		PeerSlot:        binding.PeerSlot,
		FailureScopes:   buildPeerFailureVectors(),
		PeerErrors:      buildPeerBoundErrorVectors(t),
		PathControls:    buildPeerPathControlVectors(t),
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
			OperationID: operationID.String(),
		},
		TURNOffer: peerDescriptionVector{SDP: turnOffer.SDP, BodyB64: base64.StdEncoding.EncodeToString(turnBody)},
		Offer:     peerDescriptionVector{SDP: offer.SDP, BodyB64: base64.StdEncoding.EncodeToString(offerBody)},
		Answer: peerDescriptionVector{
			SDP: answer.SDP, BodyB64: base64.StdEncoding.EncodeToString(answerBody),
		},
		Candidate: peerCandidateVector{
			Candidate: candidate.Candidate, SDPMid: candidate.SDPMid, SDPMLineIndex: candidate.SDPMLineIndex,
			UsernameFragment: candidate.UsernameFragment, BodyB64: base64.StdEncoding.EncodeToString(candidateBody),
		},
	}
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

func peerVectorBytes(first byte, count int) []byte {
	result := make([]byte, count)
	for index := range result {
		result[index] = first + byte(index)
	}
	return result
}
