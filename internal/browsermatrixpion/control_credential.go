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

func (authority *ControlCredentialAuthority) Authenticate(leaseID, credential string) bool {
	if !validOpaqueID(leaseID) {
		return false
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	now := authority.clock.Now().UTC()
	authority.pruneLocked(now)
	claim := authority.claimsByLeaseID[leaseID]
	return !authority.closed && authority.claimMatchesCredentialLocked(claim, credential) &&
		!claim.revoked && now.Before(claim.expiresAt)
}

// beginRequest transfers a temporary use lease to the HTTP handler. Retirement
// can then prove that no credential-authorized operation still owns execution.
func (authority *ControlCredentialAuthority) beginRequest(
	leaseID string,
	credential string,
) (func(), bool) {
	if !validOpaqueID(leaseID) {
		return nil, false
	}
	authority.mu.Lock()
	now := authority.clock.Now().UTC()
	authority.pruneLocked(now)
	claim := authority.claimsByLeaseID[leaseID]
	if authority.closed || !authority.claimMatchesCredentialLocked(claim, credential) ||
		claim.revoked || !now.Before(claim.expiresAt) {
		authority.mu.Unlock()
		return nil, false
	}
	claim.inFlight++
	authority.mu.Unlock()

	var once sync.Once
	return func() { once.Do(func() { authority.endRequest(leaseID) }) }, true
}

func (authority *ControlCredentialAuthority) endRequest(leaseID string) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	claim := authority.claimsByLeaseID[leaseID]
	if claim == nil || claim.inFlight < 1 {
		panic("control credential authority lost an in-flight request owner")
	}
	claim.inFlight--
	if claim.inFlight == 0 {
		now := authority.clock.Now().UTC()
		if !now.Before(claim.expiresAt) {
			authority.recordExpiredClaimLocked(leaseID, now)
		}
		authority.condition.Broadcast()
	}
}

func (authority *ControlCredentialAuthority) claimAttempt(
	leaseID string,
	requestID string,
) error {
	if !validOpaqueID(leaseID) || !validOpaqueID(requestID) {
		return errors.New("control credential attempt claim is invalid")
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	now := authority.clock.Now().UTC()
	authority.pruneLocked(now)
	claim := authority.claimsByLeaseID[leaseID]
	if claim == nil || claim.revoked || !now.Before(claim.expiresAt) {
		return errors.New("control credential lease cannot own an attempt")
	}
	if claim.attemptRequestID == "" {
		claim.attemptRequestID = requestID
		return nil
	}
	if claim.attemptRequestID != requestID {
		return errors.New("control credential lease already consumed its attempt")
	}
	return nil
}

func (authority *ControlCredentialAuthority) leaseActive(leaseID string) bool {
	if !validOpaqueID(leaseID) {
		return false
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	now := authority.clock.Now().UTC()
	authority.pruneLocked(now)
	claim := authority.claimsByLeaseID[leaseID]
	return claim != nil && !claim.revoked && now.Before(claim.expiresAt)
}

func (authority *ControlCredentialAuthority) activeLeaseMetadata(
	leaseID string,
) (ControlCredentialLease, bool) {
	if !validOpaqueID(leaseID) {
		return ControlCredentialLease{}, false
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	now := authority.clock.Now().UTC()
	authority.pruneLocked(now)
	claim := authority.claimsByLeaseID[leaseID]
	if claim == nil || claim.revoked || !now.Before(claim.expiresAt) {
		return ControlCredentialLease{}, false
	}
	return cloneControlCredentialLeaseMetadata(claim.lease), true
}

func (authority *ControlCredentialAuthority) beginRevocation(
	leaseID string,
) (ControlCredentialReceipt, bool, error) {
	if !validOpaqueID(leaseID) {
		return ControlCredentialReceipt{}, false, errors.New("control credential lease ID is invalid")
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	now := authority.clock.Now().UTC()
	authority.pruneLocked(now)
	if retirement, exists := authority.retirementsByLeaseID[leaseID]; exists {
		if retirement.completed {
			return retirement.receipt, true, nil
		}
		retirement.revoking = true
		authority.retirementsByLeaseID[leaseID] = retirement
		return ControlCredentialReceipt{}, false, nil
	}
	claim := authority.claimsByLeaseID[leaseID]
	if claim == nil {
		return ControlCredentialReceipt{}, false, errors.New("control credential lease is not active")
	}
	claim.revoked = true
	claim.revocationStarted = true
	return ControlCredentialReceipt{}, false, nil
}

func (authority *ControlCredentialAuthority) waitForRevocationRequests(leaseID string) error {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if retirement, exists := authority.retirementsByLeaseID[leaseID]; exists {
		if !retirement.completed && retirement.revoking {
			return nil
		}
		return errors.New("control credential lease is not revoking")
	}
	claim := authority.claimsByLeaseID[leaseID]
	if claim == nil || !claim.revoked {
		return errors.New("control credential lease is not revoking")
	}
	for claim.inFlight != 0 {
		authority.condition.Wait()
	}
	return nil
}

func (authority *ControlCredentialAuthority) finishRevocation(
	leaseID string,
) (ControlCredentialReceipt, error) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	now := authority.clock.Now().UTC()
	authority.pruneLocked(now)
	if retirement, exists := authority.retirementsByLeaseID[leaseID]; exists {
		if retirement.completed {
			return retirement.receipt, nil
		}
		if !retirement.revoking {
			return ControlCredentialReceipt{}, errors.New("control credential revocation is incomplete")
		}
		return authority.completeExpiredRetirementLocked(leaseID, retirement, now), nil
	}
	claim := authority.claimsByLeaseID[leaseID]
	if claim == nil || !claim.revoked || claim.inFlight != 0 {
		return ControlCredentialReceipt{}, errors.New("control credential revocation is incomplete")
	}
	return authority.recordRetirementLocked(leaseID, now), nil
}

// consumeAuthorityProbe burns the one-shot probe claim before comparing its
// scope. A mismatched request therefore cannot turn the bearer into a nonce
// oracle or retry it against a different transaction.
func (authority *ControlCredentialAuthority) consumeAuthorityProbe(
	leaseID string,
	credential string,
	request AuthorityProbeRequest,
	expectedProfileID string,
) (AuthorityProbeResponse, string, time.Time, bool) {
	if !validOpaqueID(leaseID) {
		return AuthorityProbeResponse{}, "", time.Time{}, false
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	now := authority.clock.Now().UTC()
	authority.pruneLocked(now)
	claim := authority.claimsByLeaseID[leaseID]
	if authority.closed || !authority.claimMatchesCredentialLocked(claim, credential) ||
		claim.revoked || claim.authorityProbeAttempted {
		return AuthorityProbeResponse{}, "", time.Time{}, false
	}
	claim.authorityProbeAttempted = true

	response := claim.response
	document, err := CanonicalLiveAttestationDocument(response.Attestation)
	exactDigest := err == nil && sha256Hex(document) == response.AttestationSHA256
	sampleAuthority := request.ControlAuthority.SampleAuthority
	if claim.lease.RunID != sampleAuthority.RunID ||
		claim.lease.ProfileID != expectedProfileID ||
		claim.lease.ProbeNonce != request.Nonce ||
		claim.requestedLeaseMillis != request.RequestedLeaseMillis ||
		claim.lease.AttestationSHA256 != response.AttestationSHA256 ||
		claim.lease.AuthorityInstanceID != response.Attestation.Fixture.AuthorityInstanceID ||
		response.Attestation.RunID != sampleAuthority.RunID ||
		response.Attestation.Nonce != request.Nonce ||
		response.Attestation.ExpiresAt != claim.lease.ExpiresAt ||
		!exactDigest || !now.Before(claim.expiresAt) {
		return AuthorityProbeResponse{}, "", time.Time{}, false
	}
	return cloneAuthorityProbeResponse(response), claim.lease.LeaseID, claim.expiresAt, true
}

func (authority *ControlCredentialAuthority) signAttemptTerminalReceipt(
	receipt AttemptTerminalReceipt,
) (SignedAttemptTerminalReceipt, error) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.closed {
		return SignedAttemptTerminalReceipt{}, errors.New("control credential authority is closed")
	}
	return SignAttemptTerminalReceipt(receipt, authority.signer)
}

func (authority *ControlCredentialAuthority) Release(
	leaseID string,
) (ControlCredentialReceipt, error) {
	if !validOpaqueID(leaseID) {
		return ControlCredentialReceipt{}, errors.New("control credential lease ID is invalid")
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	now := authority.clock.Now().UTC()
	authority.pruneLocked(now)
	if retirement, exists := authority.retirementsByLeaseID[leaseID]; exists {
		if retirement.completed {
			return retirement.receipt, nil
		}
		return ControlCredentialReceipt{}, errors.New("control credential lease is not active")
	}
	claim := authority.claimsByLeaseID[leaseID]
	if claim == nil || claim.revoked {
		return ControlCredentialReceipt{}, errors.New("control credential lease is not active")
	}
	if claim.inFlight != 0 {
		return ControlCredentialReceipt{}, errors.New("control credential lease still owns active requests")
	}
	claim.revoked = true
	return authority.recordRetirementLocked(leaseID, now), nil
}

func (authority *ControlCredentialAuthority) RevokeAndWait(
	leaseID string,
) (ControlCredentialReceipt, error) {
	receipt, completed, err := authority.beginRevocation(leaseID)
	if err != nil {
		return ControlCredentialReceipt{}, err
	}
	if completed {
		return receipt, nil
	}
	if err := authority.waitForRevocationRequests(leaseID); err != nil {
		return ControlCredentialReceipt{}, err
	}
	return authority.finishRevocation(leaseID)
}

func (authority *ControlCredentialAuthority) Close() {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.closed {
		return
	}
	authority.closed = true
	for _, claim := range authority.claimsByLeaseID {
		claim.revoked = true
	}
	for authority.hasInFlightRequestsLocked() {
		authority.condition.Wait()
	}
	for index := range authority.signer {
		authority.signer[index] = 0
	}
	for leaseID := range authority.claimsByLeaseID {
		authority.deleteClaimLocked(leaseID)
	}
	clear(authority.acquireReplaysByRequestID)
	clear(authority.retirementsByLeaseID)
	clear(authority.probeClaimExpiresAt)
	clear(authority.attestationClaimExpiresAt)
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

func (authority *ControlCredentialAuthority) pruneLocked(now time.Time) {
	for leaseID, claim := range authority.claimsByLeaseID {
		if now.Before(claim.expiresAt) {
			continue
		}
		claim.revoked = true
		if claim.inFlight == 0 {
			authority.recordExpiredClaimLocked(leaseID, now)
		}
	}
	for requestID, replay := range authority.acquireReplaysByRequestID {
		if !now.Before(replay.expiresAt) {
			delete(authority.acquireReplaysByRequestID, requestID)
		}
	}
	for probeKey, expiresAt := range authority.probeClaimExpiresAt {
		if !now.Before(expiresAt) {
			delete(authority.probeClaimExpiresAt, probeKey)
		}
	}
	for digest, expiresAt := range authority.attestationClaimExpiresAt {
		if !now.Before(expiresAt) {
			delete(authority.attestationClaimExpiresAt, digest)
		}
	}
	for leaseID, retirement := range authority.retirementsByLeaseID {
		if !now.Before(retirement.expiresAt) && (!retirement.revoking || retirement.completed) {
			delete(authority.retirementsByLeaseID, leaseID)
		}
	}
}

func (authority *ControlCredentialAuthority) hasInFlightRequestsLocked() bool {
	for _, claim := range authority.claimsByLeaseID {
		if claim.inFlight != 0 {
			return true
		}
	}
	return false
}

func (authority *ControlCredentialAuthority) deleteClaimLocked(leaseID string) {
	claim := authority.claimsByLeaseID[leaseID]
	if claim == nil {
		return
	}
	if claim.inFlight != 0 {
		panic("control credential authority deleted an in-flight request owner")
	}
	eraseCredentialBytes(claim.credential)
	delete(authority.claimsByLeaseID, leaseID)
}

func (authority *ControlCredentialAuthority) retirementState(
	leaseID string,
) (controlCredentialRetirementState, bool) {
	if !validOpaqueID(leaseID) {
		return controlCredentialRetirementState{}, false
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	now := authority.clock.Now().UTC()
	authority.pruneLocked(now)
	if retirement, exists := authority.retirementsByLeaseID[leaseID]; exists {
		return controlCredentialRetirementState{
			receipt: retirement.receipt, scopeExpiresAt: retirement.scopeExpiresAt,
			expiresAt: retirement.expiresAt, completed: retirement.completed,
		}, true
	}
	claim := authority.claimsByLeaseID[leaseID]
	if claim == nil {
		return controlCredentialRetirementState{}, false
	}
	return controlCredentialRetirementState{
		scopeExpiresAt: claim.expiresAt,
		expiresAt:      authority.retirementExpiresAtLocked(claim.expiresAt, now),
	}, true
}

func (authority *ControlCredentialAuthority) recordRetirementLocked(
	leaseID string,
	now time.Time,
) ControlCredentialReceipt {
	claim := authority.claimsByLeaseID[leaseID]
	if claim == nil || claim.inFlight != 0 {
		panic("control credential authority retired an invalid request owner")
	}
	receipt := ControlCredentialReceipt{LeaseID: claim.lease.LeaseID, Terminal: "revoked"}
	expiresAt := authority.retirementExpiresAtLocked(claim.expiresAt, now)
	authority.retirementsByLeaseID[claim.lease.LeaseID] = controlCredentialRetirement{
		receipt: receipt, acquireRequestID: claim.lease.RequestID,
		probeKey:          controlProbeClaimKey(claim.lease.RunID, claim.lease.ProbeNonce),
		attestationSHA256: claim.lease.AttestationSHA256, scopeExpiresAt: claim.expiresAt,
		expiresAt: expiresAt,
		completed: true,
	}
	authority.extendClaimTombstonesLocked(
		claim.lease.RequestID,
		controlProbeClaimKey(claim.lease.RunID, claim.lease.ProbeNonce),
		claim.lease.AttestationSHA256,
		expiresAt,
	)
	authority.deleteClaimLocked(leaseID)
	return receipt
}

func (authority *ControlCredentialAuthority) recordExpiredClaimLocked(
	leaseID string,
	now time.Time,
) {
	claim := authority.claimsByLeaseID[leaseID]
	if claim == nil || claim.inFlight != 0 || now.Before(claim.expiresAt) {
		panic("control credential authority expired an invalid request owner")
	}
	// Expiry only burns authorization; retention starts when the service observes
	// the burn because physical attempt containment may still be outstanding.
	expiresAt := authority.retirementExpiresAtLocked(claim.expiresAt, now)
	probeKey := controlProbeClaimKey(claim.lease.RunID, claim.lease.ProbeNonce)
	authority.retirementsByLeaseID[leaseID] = controlCredentialRetirement{
		acquireRequestID: claim.lease.RequestID, probeKey: probeKey,
		attestationSHA256: claim.lease.AttestationSHA256, scopeExpiresAt: claim.expiresAt,
		expiresAt: expiresAt, revoking: claim.revocationStarted,
	}
	authority.extendClaimTombstonesLocked(
		claim.lease.RequestID, probeKey, claim.lease.AttestationSHA256, expiresAt,
	)
	authority.deleteClaimLocked(leaseID)
}

func (authority *ControlCredentialAuthority) completeExpiredRetirementLocked(
	leaseID string,
	retirement controlCredentialRetirement,
	now time.Time,
) ControlCredentialReceipt {
	receipt := ControlCredentialReceipt{LeaseID: leaseID, Terminal: "revoked"}
	retirement.receipt = receipt
	retirement.completed = true
	retirement.expiresAt = authority.retirementExpiresAtLocked(retirement.scopeExpiresAt, now)
	authority.retirementsByLeaseID[leaseID] = retirement
	authority.extendClaimTombstonesLocked(
		retirement.acquireRequestID,
		retirement.probeKey,
		retirement.attestationSHA256,
		retirement.expiresAt,
	)
	return receipt
}

func (authority *ControlCredentialAuthority) extendClaimTombstonesLocked(
	requestID string,
	probeKey string,
	attestationSHA256 string,
	expiresAt time.Time,
) {
	if replay, exists := authority.acquireReplaysByRequestID[requestID]; exists &&
		expiresAt.After(replay.expiresAt) {
		replay.expiresAt = expiresAt
		authority.acquireReplaysByRequestID[requestID] = replay
	}
	if current, exists := authority.probeClaimExpiresAt[probeKey]; exists && expiresAt.After(current) {
		authority.probeClaimExpiresAt[probeKey] = expiresAt
	}
	if current, exists := authority.attestationClaimExpiresAt[attestationSHA256]; exists &&
		expiresAt.After(current) {
		authority.attestationClaimExpiresAt[attestationSHA256] = expiresAt
	}
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
