package browsermatrixpion

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	ProtocolVersion  = "windshare.browser-network-matrix.remote-pion/v3"
	maximumBodyBytes = 1 << 20
	maximumSDPBytes  = 512 << 10
)

var (
	canonicalIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
	opaqueIDPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	sha256Pattern      = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type ProbeRequest struct {
	ProtocolVersion string `json:"protocolVersion"`
	AttemptBinding
	Nonce   string `json:"nonce"`
	STUNURI string `json:"stunUri"`
}

type ProbeResponse struct {
	ProtocolVersion string `json:"protocolVersion"`
	AttemptBinding
	Nonce                   string `json:"nonce"`
	ServerReflexiveObserved bool   `json:"serverReflexiveObserved"`
}

type AuthorityProbeRequest struct {
	ProtocolVersion      string `json:"protocolVersion"`
	ControlAuthority     ControlAuthority `json:"controlAuthority"`
	Nonce                string `json:"nonce"`
	RequestedLeaseMillis int64  `json:"requestedLeaseMillis"`
}

type TURNCredentialRequest struct {
	ProtocolVersion string `json:"protocolVersion"`
	AttemptBinding
}

type TURNCredentialResponse struct {
	ProtocolVersion string `json:"protocolVersion"`
	AttemptBinding
	CredentialID string `json:"credentialId"`
	ExpiresAt    string `json:"expiresAt"`
	Username     string `json:"username"`
	Credential   string `json:"credential"`
}

type CreateAttemptRequest struct {
	ProtocolVersion  string                  `json:"protocolVersion"`
	RequestAuthority AttemptRequestAuthority `json:"requestAuthority"`
	LeaseMillis      int64                   `json:"leaseMillis"`
}

type CreateAttemptResponse struct {
	ProtocolVersion  string           `json:"protocolVersion"`
	AttemptAuthority AttemptAuthority `json:"attemptAuthority"`
	LeaseIssuedAt    string           `json:"leaseIssuedAt"`
	LeaseExpiresAt   string           `json:"leaseExpiresAt"`
	LeaseMillis      int64            `json:"leaseMillis"`
}

type OfferRequest struct {
	ProtocolVersion  string           `json:"protocolVersion"`
	AttemptAuthority AttemptAuthority `json:"attemptAuthority"`
	Type             string           `json:"type"`
	SDP              string           `json:"sdp"`
}

type OfferResponse struct {
	ProtocolVersion  string           `json:"protocolVersion"`
	AttemptAuthority AttemptAuthority `json:"attemptAuthority"`
	Type             string           `json:"type"`
	SDP              string           `json:"sdp"`
}

type CandidateEvidence struct {
	CandidateType string `json:"candidateType"`
	Protocol      string `json:"protocol"`
	Address       string `json:"address"`
	Port          uint16 `json:"port"`
	AddressFamily string `json:"addressFamily"`
}

type SelectedPairEvidence struct {
	Local  CandidateEvidence `json:"local"`
	Remote CandidateEvidence `json:"remote"`
}

type AttemptResult struct {
	ProtocolVersion        string                        `json:"protocolVersion"`
	AttemptAuthority       AttemptAuthority              `json:"attemptAuthority"`
	State                  string                        `json:"state"`
	SelectedPair           *SelectedPairEvidence         `json:"selectedPair"`
	ChallengeBindingSHA256 string                        `json:"challengeBindingSha256"`
	FailureCode            *string                       `json:"failureCode"`
	TerminalReceipt        *SignedAttemptTerminalReceipt `json:"terminalReceipt"`
}

type DataChannelChallenge struct {
	ProtocolVersion  string           `json:"protocolVersion"`
	AttemptAuthority AttemptAuthority `json:"attemptAuthority"`
}

type CleanupReceipt struct {
	ProtocolVersion  string           `json:"protocolVersion"`
	AttemptAuthority AttemptAuthority `json:"attemptAuthority"`
	Terminal         string           `json:"terminal"`
}

func decodeRequest(w http.ResponseWriter, request *http.Request, destination any) error {
	request.Body = http.MaxBytesReader(w, request.Body, maximumBodyBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return errors.New("request body is not the exact protocol object")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("request body contains trailing data")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	encoded, err := canonicalJSONLine(value)
	if err != nil {
		http.Error(w, "response-encoding-failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

func writeProtocolError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, struct {
		Code string `json:"code"`
	}{Code: code})
}

func validateProbeRequest(request ProbeRequest) error {
	if request.ProtocolVersion != ProtocolVersion || !validOpaqueID(request.Nonce) || !validSTUNURI(request.STUNURI) ||
		validateAttemptBinding(request.AttemptBinding) != nil {
		return errors.New("probe request is outside the remote Pion protocol")
	}
	return nil
}

func validateAuthorityProbeRequest(request AuthorityProbeRequest) error {
	if request.ProtocolVersion != ProtocolVersion || ValidateControlAuthority(request.ControlAuthority) != nil ||
		!validOpaqueID(request.Nonce) || request.RequestedLeaseMillis < 1 ||
		request.RequestedLeaseMillis > maximumLeaseLimit.Milliseconds() {
		return errors.New("authority probe request is outside the remote Pion protocol")
	}
	return nil
}

func validateCreateRequest(request CreateAttemptRequest, maximumLease time.Duration) (time.Duration, error) {
	if request.ProtocolVersion != ProtocolVersion || ValidateAttemptRequestAuthority(request.RequestAuthority) != nil ||
		request.LeaseMillis < 1 {
		return 0, errors.New("attempt request is outside the remote Pion protocol")
	}
	lease := time.Duration(request.LeaseMillis) * time.Millisecond
	if lease <= 0 || lease > maximumLease {
		return 0, errors.New("attempt lease is outside the configured authority")
	}
	return lease, nil
}

func validateOfferRequest(request OfferRequest, authority AttemptAuthority) error {
	if request.ProtocolVersion != ProtocolVersion || request.AttemptAuthority != authority ||
		ValidateAttemptAuthority(request.AttemptAuthority) != nil ||
		request.Type != "offer" || request.SDP == "" || len(request.SDP) > maximumSDPBytes ||
		strings.IndexByte(request.SDP, 0) >= 0 {
		return errors.New("offer request is outside the remote Pion protocol")
	}
	return nil
}

func validateAttemptBinding(binding AttemptBinding) error {
	return ValidateAttemptBinding(binding)
}

func validateTURNCredentialRequest(request TURNCredentialRequest) error {
	if request.ProtocolVersion != ProtocolVersion || validateAttemptBinding(request.AttemptBinding) != nil {
		return errors.New("TURN credential request is outside the remote Pion protocol")
	}
	return nil
}

func validateCanonicalID(value, label string) error {
	if len(value) > 96 || !canonicalIDPattern.MatchString(value) {
		return fmt.Errorf("%s is not a canonical authority ID", label)
	}
	return nil
}

func validOpaqueID(value string) bool {
	return len(value) >= 16 && len(value) <= 128 && opaqueIDPattern.MatchString(value)
}

func validProfileID(value string) bool {
	switch value {
	case "scheduled-public-stun", "scheduled-restricted-udp", "scheduled-coturn", "manual-real-nat":
		return true
	default:
		return false
	}
}
