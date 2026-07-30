package testicetopology

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/windshare/windshare/core/session/protocolsession"
)

type evidenceVocabularyRegistry struct {
	SchemaVersion            int                        `json:"schemaVersion"`
	BrowserEngines           []string                   `json:"browserEngines"`
	Suites                   []string                   `json:"suites"`
	ResultStatuses           []string                   `json:"resultStatuses"`
	RTCCapabilities          []string                   `json:"rtcCapabilities"`
	PeerAttemptOutcomes      []string                   `json:"peerAttemptOutcomes"`
	DeliveryOutcomes         []string                   `json:"deliveryOutcomes"`
	ExecutionOutcomes        []string                   `json:"executionOutcomes"`
	AttemptSides             []string                   `json:"attemptSides"`
	BrowserStages            []string                   `json:"browserStages"`
	SenderStages             []string                   `json:"senderStages"`
	TerminalStages           []string                   `json:"terminalStages"`
	FailureScopes            []string                   `json:"failureScopes"`
	TypedPeerErrorCodes      []string                   `json:"typedPeerErrorCodes"`
	PeerOperationCodeMapping []peerOperationCodeMapping `json:"peerOperationCodeMapping"`
	ICECandidateTypes        []string                   `json:"iceCandidateTypes"`
	ICEProtocols             []string                   `json:"iceProtocols"`
	IPAddressFamilies        []string                   `json:"ipAddressFamilies"`
}

type peerOperationCodeMapping struct {
	Code           int    `json:"code"`
	TypedErrorCode string `json:"typedErrorCode"`
}

func TestEvidenceVocabularyExactlyMatchesFrozenContract(t *testing.T) {
	t.Parallel()
	registry := loadEvidenceVocabularyRegistry(t)
	want := evidenceVocabularyRegistry{
		SchemaVersion:       1,
		BrowserEngines:      []string{"chromium", "firefox", "webkit"},
		Suites:              []string{"main", "pion"},
		ResultStatuses:      []string{"provisional", "final-valid", "final-invalid"},
		RTCCapabilities:     []string{"unknown", "unavailable", "unusable", "available"},
		PeerAttemptOutcomes: []string{"not-started", "admitted", "failed"},
		DeliveryOutcomes:    []string{"not-started", "succeeded", "failed"},
		ExecutionOutcomes:   []string{"healthy", "crashed", "infrastructure-failed", "unknown"},
		AttemptSides:        []string{"browser", "sender"},
		BrowserStages: []string{
			"started", "offer-created", "offer-sent", "answer-received", "datachannel-open",
			"lane-granted", "lane-attached", "admitted", "failed",
		},
		SenderStages: []string{
			"started", "offer-received", "answer-created", "answer-sent", "datachannel-open",
			"lane-admission-started", "admitted", "failed",
		},
		TerminalStages: []string{"admitted", "failed"},
		FailureScopes:  []string{"attempt", "session"},
		TypedPeerErrorCodes: []string{
			"peer-negotiation", "peer-timeout", "peer-candidates", "peer-admission",
			"signaling-contract", "attempt-cancelled", "runtime-stopped", "unexpected",
		},
		PeerOperationCodeMapping: []peerOperationCodeMapping{
			{Code: int(protocolsession.PeerOperationCodeNegotiation), TypedErrorCode: "peer-negotiation"},
			{Code: int(protocolsession.PeerOperationCodeTimeout), TypedErrorCode: "peer-timeout"},
			{Code: int(protocolsession.PeerOperationCodeCandidates), TypedErrorCode: "peer-candidates"},
			{Code: int(protocolsession.PeerOperationCodeAdmission), TypedErrorCode: "peer-admission"},
		},
		ICECandidateTypes: []string{"host", "prflx", "srflx", "relay"},
		ICEProtocols:      []string{"udp", "tcp"},
		IPAddressFamilies: []string{"ipv4", "ipv6"},
	}
	if !reflect.DeepEqual(registry, want) {
		t.Fatalf("vocabulary registry =\n%+v\nwant =\n%+v", registry, want)
	}
}

func loadEvidenceVocabularyRegistry(t *testing.T) evidenceVocabularyRegistry {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "browser-evidence", "v1", "vocabulary.json")
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	if err := validateCanonicalJSON(encoded, "browser evidence vocabulary"); err != nil {
		t.Fatalf("strict vocabulary JSON: %v", err)
	}
	root, err := rawObject(encoded, "browser evidence vocabulary")
	if err != nil {
		t.Fatalf("decode vocabulary object: %v", err)
	}
	if err := requireExactKeys(
		root,
		"browser evidence vocabulary",
		"schemaVersion",
		"browserEngines",
		"suites",
		"resultStatuses",
		"rtcCapabilities",
		"peerAttemptOutcomes",
		"deliveryOutcomes",
		"executionOutcomes",
		"attemptSides",
		"browserStages",
		"senderStages",
		"terminalStages",
		"failureScopes",
		"typedPeerErrorCodes",
		"peerOperationCodeMapping",
		"iceCandidateTypes",
		"iceProtocols",
		"ipAddressFamilies",
	); err != nil {
		t.Fatalf("vocabulary shape: %v", err)
	}
	var encodedMappings []json.RawMessage
	if err := json.Unmarshal(root["peerOperationCodeMapping"], &encodedMappings); err != nil || encodedMappings == nil {
		t.Fatalf("peer operation code mapping must be an array: %v", err)
	}
	for index, encodedMapping := range encodedMappings {
		label := fmt.Sprintf("peer operation code mapping %d", index)
		mapping, err := rawObject(encodedMapping, label)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if err := requireExactKeys(mapping, label, "code", "typedErrorCode"); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	}
	var registry evidenceVocabularyRegistry
	if err := decodeCanonicalJSON(encoded, "browser evidence vocabulary", &registry); err != nil {
		t.Fatalf("decode vocabulary: %v", err)
	}
	return registry
}
