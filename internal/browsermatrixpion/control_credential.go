package browsermatrixpion

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

const (
	controlCredentialEntropyBytes = 32
	controlLeaseIdentifierBytes   = 24
	maximumControlLease           = 2 * time.Minute
)

type ControlCredentialAcquireRequest struct {
	RequestID            string                  `json:"requestId"`
	RunID                string                  `json:"runId"`
	ProfileID            string                  `json:"profileId"`
	ProbeNonce           string                  `json:"probeNonce"`
	RequestedLeaseMillis int64                   `json:"requestedLeaseMillis"`
	TURN                 *ControlTURNDeclaration `json:"turn,omitempty"`
}

type ControlCredentialLease struct {
	LeaseID             string                  `json:"leaseId"`
	RequestID           string                  `json:"requestId"`
	RunID               string                  `json:"runId"`
	ProfileID           string                  `json:"profileId"`
	AuthorityInstanceID string                  `json:"authorityInstanceId"`
	AttestationSHA256   string                  `json:"attestationSha256"`
	ProbeNonce          string                  `json:"probeNonce"`
	IssuedAt            string                  `json:"issuedAt"`
	ExpiresAt           string                  `json:"expiresAt"`
	MaxAttempts         int                     `json:"maxAttempts"`
	Credential          []byte                  `json:"-"`
	TURN                *ControlTURNDeclaration `json:"turn,omitempty"`
}

// String and GoString intentionally exclude Credential so routine diagnostics cannot
// turn an ephemeral authority bearer into process output.
func (lease ControlCredentialLease) String() string {
	return fmt.Sprintf(
		"control-credential-lease(id=%s,request=%s,run=%s,profile=%s,authority=%s,attestation=%s,expires=%s)",
		lease.LeaseID,
		lease.RequestID,
		lease.RunID,
		lease.ProfileID,
		lease.AuthorityInstanceID,
		lease.AttestationSHA256,
		lease.ExpiresAt,
	)
}

func (lease ControlCredentialLease) GoString() string { return lease.String() }

type ControlCredentialReceipt struct {
	LeaseID  string `json:"leaseId"`
	Terminal string `json:"terminal"`
}

type ControlCredentialAuthorityConfig struct {
	Fixture                  ExternalFixture
	AttestationSigner        ed25519.PrivateKey
	MaximumLease             time.Duration
	TombstoneRetention       time.Duration
	MaximumClaims            int
	Clock                    LeaseClock
	CredentialSource         io.Reader
	ControlLeaseIDSource     io.Reader
	AttestationLeaseIDSource io.Reader
}

type ControlCredentialAuthority struct {
	fixture                  ExternalFixture
	signer                   ed25519.PrivateKey
	maximumLease             time.Duration
	tombstoneRetention       time.Duration
	maximumClaims            int
	clock                    LeaseClock
	credentialSource         io.Reader
	controlLeaseIDSource     io.Reader
	attestationLeaseIDSource io.Reader

	mu                        sync.Mutex
	condition                 *sync.Cond
	claimsByLeaseID           map[string]*controlCredentialClaim
	acquireReplaysByRequestID map[string]controlCredentialAcquireReplay
	retirementsByLeaseID      map[string]controlCredentialRetirement
	probeClaimExpiresAt       map[string]time.Time
	attestationClaimExpiresAt map[string]time.Time
	closed                    bool
}

type controlCredentialClaim struct {
	lease                   ControlCredentialLease
	credential              []byte
	response                AuthorityProbeResponse
	requestedLeaseMillis    int64
	expiresAt               time.Time
	inFlight                int
	attemptRequestID        string
	revoked                 bool
	revocationStarted       bool
	authorityProbeAttempted bool
}

type controlCredentialAcquireReplay struct {
	request   ControlCredentialAcquireRequest
	leaseID   string
	expiresAt time.Time
}

type controlCredentialRetirement struct {
	receipt           ControlCredentialReceipt
	acquireRequestID  string
	probeKey          string
	attestationSHA256 string
	scopeExpiresAt    time.Time
	expiresAt         time.Time
	revoking          bool
	completed         bool
}

type controlCredentialRetirementState struct {
	receipt        ControlCredentialReceipt
	scopeExpiresAt time.Time
	expiresAt      time.Time
	completed      bool
}

func NewControlCredentialAuthority(
	config ControlCredentialAuthorityConfig,
) (*ControlCredentialAuthority, error) {
	fixture := cloneExternalFixture(config.Fixture)
	if err := ValidateExternalFixture(fixture); err != nil {
		return nil, err
	}
	if len(config.AttestationSigner) != ed25519.PrivateKeySize ||
		config.MaximumLease <= 0 || config.MaximumLease > maximumLeaseLimit ||
		config.TombstoneRetention <= 0 || config.TombstoneRetention > maximumTombstoneLife ||
		config.MaximumClaims < 1 || config.MaximumClaims > maximumTombstoneCapacity {
		return nil, errors.New("control credential authority is incomplete")
	}
	clock := config.Clock
	if clock == nil {
		clock = realLeaseClock{}
	}
	credentialSource := config.CredentialSource
	if credentialSource == nil {
		credentialSource = rand.Reader
	}
	controlLeaseIDSource := config.ControlLeaseIDSource
	if controlLeaseIDSource == nil {
		controlLeaseIDSource = rand.Reader
	}
	attestationLeaseIDSource := config.AttestationLeaseIDSource
	if attestationLeaseIDSource == nil {
		attestationLeaseIDSource = rand.Reader
	}
	authority := &ControlCredentialAuthority{
		fixture:                   fixture,
		signer:                    append(ed25519.PrivateKey(nil), config.AttestationSigner...),
		maximumLease:              config.MaximumLease,
		tombstoneRetention:        config.TombstoneRetention,
		maximumClaims:             config.MaximumClaims,
		clock:                     clock,
		credentialSource:          credentialSource,
		controlLeaseIDSource:      controlLeaseIDSource,
		attestationLeaseIDSource:  attestationLeaseIDSource,
		claimsByLeaseID:           make(map[string]*controlCredentialClaim),
		acquireReplaysByRequestID: make(map[string]controlCredentialAcquireReplay),
		retirementsByLeaseID:      make(map[string]controlCredentialRetirement),
		probeClaimExpiresAt:       make(map[string]time.Time),
		attestationClaimExpiresAt: make(map[string]time.Time),
	}
	authority.condition = sync.NewCond(&authority.mu)
	return authority, nil
}

// Acquire commits the caller's nonce before any bearer credential exists. This
// removes the digest circularity: the broker receives the exact digest of the
// signed response that the one-shot authority probe is allowed to retrieve.
func (authority *ControlCredentialAuthority) Acquire(
	request ControlCredentialAcquireRequest,
) (ControlCredentialLease, error) {
	if !validOpaqueID(request.RequestID) ||
		validateCanonicalID(request.RunID, "run ID") != nil ||
		!validProfileID(request.ProfileID) ||
		!validOpaqueID(request.ProbeNonce) ||
		request.RequestedLeaseMillis < 1 ||
		request.RequestedLeaseMillis > maximumControlLease.Milliseconds() {
		return ControlCredentialLease{}, errors.New("control credential request is invalid")
	}

	authority.mu.Lock()
	defer authority.mu.Unlock()
	now := authority.clock.Now().UTC().Truncate(time.Millisecond)
	authority.pruneLocked(now)
	if authority.closed {
		return ControlCredentialLease{}, errors.New("control credential authority is unavailable")
	}
	if replay, exists := authority.acquireReplaysByRequestID[request.RequestID]; exists {
		if !sameControlCredentialAcquireRequest(replay.request, request) {
			return ControlCredentialLease{}, errors.New("control credential request ID changed its scope")
		}
		claim := authority.claimsByLeaseID[replay.leaseID]
		if claim == nil || claim.revoked || !now.Before(claim.expiresAt) {
			return ControlCredentialLease{}, errors.New("control credential request is already retired")
		}
		lease := claim.lease
		lease.TURN = cloneControlTURNDeclaration(claim.lease.TURN)
		lease.Credential = append([]byte(nil), claim.credential...)
		return lease, nil
	}
	if request.ProfileID != authority.fixture.ProfileID ||
		len(authority.claimsByLeaseID)+len(authority.retirementsByLeaseID) >= authority.maximumClaims ||
		len(authority.probeClaimExpiresAt) >= authority.maximumClaims {
		return ControlCredentialLease{}, errors.New("control credential authority is unavailable")
	}
	probeKey := controlProbeClaimKey(request.RunID, request.ProbeNonce)
	if _, claimed := authority.probeClaimExpiresAt[probeKey]; claimed {
		return ControlCredentialLease{}, errors.New("control credential probe nonce is already claimed")
	}
	granted, err := authority.grantedLeaseLocked(request, now)
	if err != nil {
		return ControlCredentialLease{}, err
	}
	attestationLeaseID, err := readOpaqueAuthorityID(
		authority.attestationLeaseIDSource,
		attestationLeaseIdentifierBytes,
	)
	if err != nil {
		return ControlCredentialLease{}, errors.New("control credential attestation lease ID source failed")
	}
	controlLeaseID, err := readOpaqueAuthorityID(
		authority.controlLeaseIDSource,
		controlLeaseIdentifierBytes,
	)
	if err != nil {
		return ControlCredentialLease{}, errors.New("control credential lease ID source failed")
	}
	if authority.claimsByLeaseID[controlLeaseID] != nil ||
		authority.retirementsByLeaseID[controlLeaseID].receipt.LeaseID != "" {
		return ControlCredentialLease{}, errors.New("control credential lease ID collided")
	}
	credential, err := readControlCredential(authority.credentialSource)
	if err != nil {
		return ControlCredentialLease{}, err
	}
	if authority.credentialCollidesLocked(credential) {
		eraseCredentialBytes(credential)
		return ControlCredentialLease{}, errors.New("control credential source collided")
	}

	expiresAt := now.Add(granted)
	fixture := cloneExternalFixture(authority.fixture)
	if request.TURN != nil {
		fixture.NetworkSemantics.TURNCredentialID = request.TURN.CredentialID
		fixture.NetworkSemantics.TURNUsername = request.TURN.Username
		fixture.NetworkSemantics.TURNCredentialExpiresAt = request.TURN.ExpiresAt
	}
	attestation := LiveExternalFixtureAttestation{
		SchemaVersion: LiveAttestationSchemaVersion,
		RunID:         request.RunID,
		Nonce:         request.ProbeNonce,
		LeaseID:       attestationLeaseID,
		LeaseMillis:   granted.Milliseconds(),
		IssuedAt:      now.Format(canonicalTimestampLayout),
		ExpiresAt:     expiresAt.Format(canonicalTimestampLayout),
		Fixture:       fixture,
	}
	response, err := SignLiveAttestation(attestation, authority.signer)
	if err != nil {
		eraseCredentialBytes(credential)
		return ControlCredentialLease{}, errors.New("control credential attestation signing failed")
	}
	if _, claimed := authority.attestationClaimExpiresAt[response.AttestationSHA256]; claimed {
		eraseCredentialBytes(credential)
		return ControlCredentialLease{}, errors.New("control credential attestation digest collided")
	}
	lease := ControlCredentialLease{
		LeaseID:             controlLeaseID,
		RequestID:           request.RequestID,
		RunID:               request.RunID,
		ProfileID:           request.ProfileID,
		AuthorityInstanceID: authority.fixture.AuthorityInstanceID,
		AttestationSHA256:   response.AttestationSHA256,
		ProbeNonce:          request.ProbeNonce,
		IssuedAt:            attestation.IssuedAt,
		ExpiresAt:           attestation.ExpiresAt,
		MaxAttempts:         1,
		TURN:                cloneControlTURNDeclaration(request.TURN),
	}
	claim := &controlCredentialClaim{
		lease:                   cloneControlCredentialLeaseMetadata(lease),
		credential:              append([]byte(nil), credential...),
		response:                cloneAuthorityProbeResponse(response),
		requestedLeaseMillis:    request.RequestedLeaseMillis,
		expiresAt:               expiresAt,
		authorityProbeAttempted: false,
	}
	authority.claimsByLeaseID[controlLeaseID] = claim
	claimTombstoneExpiresAt := authority.retirementExpiresAtLocked(expiresAt, now)
	authority.acquireReplaysByRequestID[request.RequestID] = controlCredentialAcquireReplay{
		request: cloneControlCredentialAcquireRequest(request), leaseID: controlLeaseID,
		expiresAt: claimTombstoneExpiresAt,
	}
	authority.probeClaimExpiresAt[probeKey] = claimTombstoneExpiresAt
	authority.attestationClaimExpiresAt[response.AttestationSHA256] = claimTombstoneExpiresAt

	lease.Credential = credential
	lease.TURN = cloneControlTURNDeclaration(lease.TURN)
	return lease, nil
}

func (authority *ControlCredentialAuthority) String() string {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	return fmt.Sprintf(
		"control-credential-authority(instance=%s,profile=%s,active=%d)",
		authority.fixture.AuthorityInstanceID,
		authority.fixture.ProfileID,
		len(authority.claimsByLeaseID),
	)
}

func (authority *ControlCredentialAuthority) grantedLeaseLocked(
	request ControlCredentialAcquireRequest,
	now time.Time,
) (time.Duration, error) {
	requested := time.Duration(request.RequestedLeaseMillis) * time.Millisecond
	if requested <= 0 || requested > maximumControlLease {
		return 0, errors.New("control credential lease is outside authority")
	}
	granted := requested
	if authority.fixture.ProfileID == "scheduled-coturn" {
		if request.TURN == nil || !validControlTURNDeclaration(*request.TURN) {
			return 0, errors.New("control credential TURN declaration is unavailable")
		}
		credentialExpiry, _ := parseCanonicalTimestamp(
			request.TURN.ExpiresAt,
		)
		remaining := credentialExpiry.Sub(now)
		if remaining < time.Millisecond || remaining > requested || remaining > authority.maximumLease {
			return 0, errors.New("control credential Coturn lease is unavailable")
		}
		granted = remaining
	} else if request.TURN != nil {
		return 0, errors.New("control credential TURN declaration is unexpected")
	} else if authority.maximumLease < granted {
		granted = authority.maximumLease
	}
	return granted, nil
}

func sameControlCredentialAcquireRequest(
	left ControlCredentialAcquireRequest,
	right ControlCredentialAcquireRequest,
) bool {
	if left.RequestID != right.RequestID || left.RunID != right.RunID ||
		left.ProfileID != right.ProfileID || left.ProbeNonce != right.ProbeNonce ||
		left.RequestedLeaseMillis != right.RequestedLeaseMillis {
		return false
	}
	if left.TURN == nil || right.TURN == nil {
		return left.TURN == nil && right.TURN == nil
	}
	return *left.TURN == *right.TURN
}

func cloneControlCredentialAcquireRequest(
	request ControlCredentialAcquireRequest,
) ControlCredentialAcquireRequest {
	request.TURN = cloneControlTURNDeclaration(request.TURN)
	return request
}

func validControlTURNDeclaration(declaration ControlTURNDeclaration) bool {
	if validateCanonicalID(declaration.CredentialID, "TURN credential ID") != nil ||
		!validICECredential(declaration.Username) {
		return false
	}
	_, err := parseCanonicalTimestamp(declaration.ExpiresAt)
	return err == nil
}

func cloneControlCredentialLeaseMetadata(lease ControlCredentialLease) ControlCredentialLease {
	lease.Credential = nil
	lease.TURN = cloneControlTURNDeclaration(lease.TURN)
	return lease
}

func (authority *ControlCredentialAuthority) credentialCollidesLocked(credential []byte) bool {
	for _, claim := range authority.claimsByLeaseID {
		if len(credential) == len(claim.credential) &&
			subtle.ConstantTimeCompare(credential, claim.credential) == 1 {
			return true
		}
	}
	return false
}

func (authority *ControlCredentialAuthority) claimMatchesCredentialLocked(
	claim *controlCredentialClaim,
	credential string,
) bool {
	return claim != nil && len(credential) == len(claim.credential) &&
		subtle.ConstantTimeCompare([]byte(credential), claim.credential) == 1
}

func (authority *ControlCredentialAuthority) retirementExpiresAtLocked(
	scopeExpiresAt time.Time,
	now time.Time,
) time.Time {
	retentionExpiresAt := now.Add(authority.tombstoneRetention)
	if scopeExpiresAt.After(retentionExpiresAt) {
		return scopeExpiresAt
	}
	return retentionExpiresAt
}

func controlProbeClaimKey(runID, probeNonce string) string {
	return runID + "\x00" + probeNonce
}

func readOpaqueAuthorityID(source io.Reader, byteCount int) (string, error) {
	identifier := make([]byte, byteCount)
	if _, err := io.ReadFull(source, identifier); err != nil {
		eraseCredentialBytes(identifier)
		return "", err
	}
	value := base64.RawURLEncoding.EncodeToString(identifier)
	eraseCredentialBytes(identifier)
	if !validOpaqueID(value) {
		return "", errors.New("authority ID source is invalid")
	}
	return value, nil
}

func readControlCredential(source io.Reader) ([]byte, error) {
	entropy := make([]byte, controlCredentialEntropyBytes)
	if _, err := io.ReadFull(source, entropy); err != nil {
		eraseCredentialBytes(entropy)
		return nil, errors.New("control credential source failed")
	}
	credential := []byte(base64.RawURLEncoding.EncodeToString(entropy))
	eraseCredentialBytes(entropy)
	if len(credential) < minimumCredentialBytes || !validOpaqueID(string(credential)) {
		eraseCredentialBytes(credential)
		return nil, errors.New("control credential source is invalid")
	}
	return credential, nil
}

func cloneAuthorityProbeResponse(response AuthorityProbeResponse) AuthorityProbeResponse {
	response.Attestation.Fixture = cloneExternalFixture(response.Attestation.Fixture)
	return response
}

func eraseCredentialBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
