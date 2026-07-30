package browsermatrixbroker

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/windshare/windshare/internal/browsermatrixpion"
)

const (
	maximumControlLease       = 2 * time.Minute
	maximumRetirementTimeout  = 2 * time.Minute
	maximumTombstoneRetention = 24 * time.Hour
	maximumBrokerCapacity     = 65_535
)

type AdminService interface {
	AcquireControlCredential(browsermatrixpion.ControlCredentialAcquireRequest) (browsermatrixpion.ControlCredentialLease, error)
	BindControlTURNCredential(browsermatrixpion.ControlTURNCredentialLease) error
	ReleaseControlCredential(string) (browsermatrixpion.ControlCredentialReceipt, error)
	RevokeControlCredentialAndWait(string) (browsermatrixpion.ControlCredentialReceipt, error)
}

type TURNAcquireRequest struct {
	RequestID   string
	RunID       string
	ProfileID   string
	ProbeNonce  string
	ExpiresAt   string
	MaxAttempts int
}

type TURNReservation struct {
	ProviderLeaseID string
	RequestID       string
	RunID           string
	ProfileID       string
	ProbeNonce      string
	CredentialID    string
	Username        string
	ExpiresAt       string
	MaxAttempts     int
	Credential      []byte
}

func (lease TURNReservation) String() string {
	return fmt.Sprintf(
		"broker-turn-reservation(id=%s,request=%s,run=%s,profile=%s,expires=%s)",
		lease.ProviderLeaseID,
		lease.RequestID,
		lease.RunID,
		lease.ProfileID,
		lease.ExpiresAt,
	)
}

func (lease TURNReservation) GoString() string { return lease.String() }

type TURNBindRequest struct {
	ProviderLeaseID   string
	RequestID         string
	RunID             string
	ProfileID         string
	ProbeNonce        string
	ControlLeaseID    string
	AttestationSHA256 string
	CredentialID      string
	Username          string
	ExpiresAt         string
	MaxAttempts       int
}

type TURNBoundLease TURNBindRequest

type TURNRetirementRequest struct {
	Operation         string
	RequestID         string
	ProviderLeaseID   string
	ControlLeaseID    string
	RunID             string
	ProfileID         string
	ProbeNonce        string
	AttestationSHA256 string
}

type TURNRetirementReceipt struct {
	RequestID       string
	ProviderLeaseID string
	Terminal        string
}

type RevocableTURNProvider interface {
	Acquire(context.Context, TURNAcquireRequest) (TURNReservation, error)
	BindAndWait(context.Context, TURNBindRequest) (TURNBoundLease, error)
	RevokeAndWait(context.Context, TURNRetirementRequest) (TURNRetirementReceipt, error)
}

type TraceEvent struct {
	Milestone string `json:"milestone"`
	Operation string `json:"operation,omitempty"`
	RequestID string `json:"requestId,omitempty"`
	LeaseID   string `json:"leaseId,omitempty"`
	Outcome   string `json:"outcome"`
}

type TraceSink func(TraceEvent)

type Config struct {
	ControllerOrigin   string
	ProfileID          string
	ExpectedIdentity   WorkloadIdentityBinding
	LeaseDuration      time.Duration
	RetirementTimeout  time.Duration
	TombstoneRetention time.Duration
	MaximumTombstones  int
	MaximumOIDCReplays int
	Signer             ed25519.PrivateKey
	Admin              AdminService
	TURNProvider       RevocableTURNProvider
	Clock              Clock
	Trace              TraceSink
}

type Handler struct {
	controllerOrigin   string
	profileID          string
	expectedIdentity   WorkloadIdentityBinding
	leaseDuration      time.Duration
	retirementTimeout  time.Duration
	tombstoneRetention time.Duration
	maximumTombstones  int
	signer             ed25519.PrivateKey
	admin              AdminService
	identityValidator  WorkloadIdentityValidator
	turnProvider       RevocableTURNProvider
	clock              Clock
	trace              TraceSink

	mu               sync.Mutex
	accepting        bool
	settled          bool
	activeRequests   int
	activeZero       chan struct{}
	lifecycleContext context.Context
	cancelLifecycle  context.CancelFunc
	settlementGate   chan struct{}
	acquisitions     map[string]*acquisitionClaim
	requestOwners    map[string]*acquisitionClaim
	leases           map[string]*compositeLease
	retirementClaims map[string]*retirementClaim
}

type acquisitionClaim struct {
	operation        sync.Mutex
	scope            requestScope
	leaseID          string
	providerLeaseID  string
	turnCredentialID string
	turnUsername     string
	turnExpiresAt    string
	controlOwned     bool
	providerOwned    bool
	failed           bool
	ready            bool
	inFlight         int
	expiresAt        time.Time
}

type compositeLease struct {
	operation         sync.Mutex
	owner             *acquisitionClaim
	scope             requestScope
	leaseID           string
	authorityID       string
	attestationSHA256 string
	controlExpiresAt  string
	controlExpires    time.Time
	providerLeaseID   string
	turnCredentialID  string
	turnUsername      string
	turnExpiresAt     string
	controlSettled    bool
	turnSettled       bool
	ready             bool
	terminal          bool
	terminalOperation string
	receipt           ReceiptPayload
	expiresAt         time.Time
}

type retirementClaim struct {
	scope      requestScope
	done       chan struct{}
	receipt    ReceiptPayload
	err        error
	expiresAt  time.Time
	completed  bool
	inProgress bool
}

func NewHandler(config Config) (*Handler, error) {
	clock := config.Clock
	if clock == nil {
		clock = realClock{}
	}
	validator, err := NewOIDCValidator(OIDCPolicy{
		Issuer: config.ExpectedIdentity.Issuer, Audience: config.ExpectedIdentity.Audience,
		Repository: config.ExpectedIdentity.Repository, Ref: config.ExpectedIdentity.Ref,
		WorkflowRef: config.ExpectedIdentity.WorkflowRef, JWKSURL: GitHubActionsJWKSURL,
		MaximumTokenReplays: config.MaximumOIDCReplays, Clock: clock,
	})
	if err != nil {
		return nil, err
	}
	return newHandler(config, clock, validator)
}

// NewTestHandlerForHarness allows deterministic validator, clock, and admin
// injection without weakening the production constructor's issuer/JWKS pins.
func NewTestHandlerForHarness(
	config Config,
	validator WorkloadIdentityValidator,
) (*Handler, error) {
	clock := config.Clock
	if clock == nil {
		clock = realClock{}
	}
	return newHandler(config, clock, validator)
}

func newHandler(
	config Config,
	clock Clock,
	identityValidator WorkloadIdentityValidator,
) (*Handler, error) {
	if !canonicalControllerOrigin(config.ControllerOrigin) || !validProfileID(config.ProfileID) ||
		config.ProfileID == "manual-real-nat" ||
		!validExpectedIdentity(config.ExpectedIdentity) || config.LeaseDuration <= 0 ||
		config.LeaseDuration > maximumControlLease ||
		config.RetirementTimeout <= 0 || config.RetirementTimeout > maximumRetirementTimeout ||
		config.LeaseDuration%time.Millisecond != 0 || config.TombstoneRetention <= 0 ||
		config.TombstoneRetention > maximumTombstoneRetention ||
		config.MaximumTombstones < 1 || config.MaximumTombstones > maximumBrokerCapacity ||
		config.MaximumOIDCReplays < 1 || config.MaximumOIDCReplays > maximumBrokerCapacity ||
		len(config.Signer) != ed25519.PrivateKeySize || config.Admin == nil ||
		identityValidator == nil || config.ExpectedIdentity.Issuer == "" ||
		config.ExpectedIdentity.Audience == "" {
		return nil, errors.New("credential broker composition is incomplete")
	}
	if config.ProfileID == "scheduled-coturn" {
		if config.TURNProvider == nil {
			return nil, errors.New("credential broker revocable TURN capability is unavailable")
		}
	} else if config.TURNProvider != nil {
		return nil, errors.New("credential broker received an unexpected TURN capability")
	}
	lifecycleContext, cancelLifecycle := context.WithCancel(context.Background())
	activeZero := make(chan struct{})
	close(activeZero)
	settlementGate := make(chan struct{}, 1)
	settlementGate <- struct{}{}
	return &Handler{
		controllerOrigin: config.ControllerOrigin, profileID: config.ProfileID,
		expectedIdentity: config.ExpectedIdentity, leaseDuration: config.LeaseDuration,
		retirementTimeout:  config.RetirementTimeout,
		tombstoneRetention: config.TombstoneRetention, maximumTombstones: config.MaximumTombstones,
		signer: append(ed25519.PrivateKey(nil), config.Signer...), admin: config.Admin,
		identityValidator: identityValidator, turnProvider: config.TURNProvider,
		clock: clock, trace: config.Trace,
		lifecycleContext: lifecycleContext, cancelLifecycle: cancelLifecycle,
		activeZero: activeZero, settlementGate: settlementGate,
		accepting: true, acquisitions: make(map[string]*acquisitionClaim),
		requestOwners: make(map[string]*acquisitionClaim),
		leases:        make(map[string]*compositeLease), retirementClaims: make(map[string]*retirementClaim),
	}, nil
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if !handler.admit() {
		writeBrokerError(writer, http.StatusServiceUnavailable)
		return
	}
	defer handler.finishRequest()
	operationContext, cancelOperation := context.WithCancel(request.Context())
	stopLifecycleCancellation := context.AfterFunc(handler.lifecycleContext, cancelOperation)
	defer func() { stopLifecycleCancellation(); cancelOperation() }()
	if !handler.exactHTTPRequest(request) {
		writeBrokerError(writer, http.StatusBadRequest)
		return
	}
	frame, err := readBoundedFrame(request.Body)
	if err != nil || int64(len(frame)) != request.ContentLength {
		erase(frame)
		writeBrokerError(writer, http.StatusBadRequest)
		return
	}
	defer erase(frame)
	parsed, err := parseRequestFrame(frame)
	if err != nil || validateRequestShape(parsed) != nil ||
		parsed.scope.ControllerOrigin != handler.controllerOrigin ||
		parsed.scope.ProfileID != handler.profileID || parsed.scope.Identity != handler.expectedIdentity {
		writeBrokerError(writer, http.StatusBadRequest)
		return
	}
	if err := handler.identityValidator.Validate(operationContext, parsed.workloadAssertion); err != nil {
		handler.emit(parsed.scope, "identity-rejected")
		writeBrokerError(writer, http.StatusUnauthorized)
		return
	}

	var response []byte
	if parsed.scope.Operation == "acquire" {
		response, err = handler.acquire(operationContext, parsed.scope)
	} else {
		response, err = handler.retire(operationContext, parsed.scope)
	}
	if err != nil {
		handler.emit(parsed.scope, "operation-rejected")
		writeBrokerError(writer, brokerStatus(err))
		return
	}
	defer erase(response)
	writer.Header().Set("Content-Type", BrokerContentType)
	writer.Header().Set("Content-Length", strconv.Itoa(len(response)))
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(response)
	handler.emit(parsed.scope, "completed")
}

func (handler *Handler) acquire(ctx context.Context, scope requestScope) ([]byte, error) {
	claim, err := handler.acquireClaim(scope)
	if err != nil {
		return nil, err
	}
	defer handler.releaseAcquisitionClaim(claim)
	claim.operation.Lock()
	defer claim.operation.Unlock()
	if claim.failed {
		return nil, errors.Join(errOperationConflict, handler.settleFailedAcquisition(claim))
	}
	if claim.ready {
		return handler.replayReadyAcquisition(claim)
	}

	var reservation TURNReservation
	var turnDeclaration *browsermatrixpion.ControlTURNDeclaration
	if handler.turnProvider != nil {
		reservation, err = handler.acquireTURNReservation(ctx, claim)
		if err != nil {
			return nil, handler.failAcquisition(claim, err)
		}
		defer erase(reservation.Credential)
		turnDeclaration = &browsermatrixpion.ControlTURNDeclaration{
			CredentialID: reservation.CredentialID,
			Username:     reservation.Username,
			ExpiresAt:    reservation.ExpiresAt,
		}
	}
	lease, acquireErr := handler.admin.AcquireControlCredential(browsermatrixpion.ControlCredentialAcquireRequest{
		RequestID: scope.RequestID, RunID: scope.RunID, ProfileID: scope.ProfileID,
		ProbeNonce: scope.ProbeNonce, RequestedLeaseMillis: handler.leaseDuration.Milliseconds(),
		TURN: turnDeclaration,
	})
	defer erase(lease.Credential)
	registrationErr := handler.recordControlMaterialization(claim, lease, reservation)
	issuedAt, expiresAt, validationErr := validateControlLease(
		scope, lease, handler.clock.Now().UTC(), turnDeclaration,
	)
	if acquireErr != nil || registrationErr != nil || validationErr != nil {
		return nil, handler.failAcquisition(
			claim,
			errors.Join(errOperationConflict, acquireErr, registrationErr, validationErr),
		)
	}
	if handler.turnProvider != nil {
		bindRequest := TURNBindRequest{
			ProviderLeaseID: reservation.ProviderLeaseID, RequestID: scope.RequestID,
			RunID: scope.RunID, ProfileID: scope.ProfileID, ProbeNonce: scope.ProbeNonce,
			ControlLeaseID: lease.LeaseID, AttestationSHA256: lease.AttestationSHA256,
			CredentialID: reservation.CredentialID, Username: reservation.Username,
			ExpiresAt: reservation.ExpiresAt, MaxAttempts: 1,
		}
		bound, bindErr := handler.turnProvider.BindAndWait(ctx, bindRequest)
		if bindErr != nil || !exactTURNBoundLease(bindRequest, bound) ||
			subtle.ConstantTimeCompare(reservation.Credential, lease.Credential) == 1 ||
			handler.admin.BindControlTURNCredential(browsermatrixpion.ControlTURNCredentialLease{
				RequestID: scope.RequestID, RunID: scope.RunID, ProfileID: scope.ProfileID,
				ProbeNonce: scope.ProbeNonce, ControlLeaseID: lease.LeaseID,
				AttestationSHA256: lease.AttestationSHA256, CredentialID: reservation.CredentialID,
				Username: reservation.Username, ExpiresAt: reservation.ExpiresAt,
				MaxAttempts: 1, Credential: reservation.Credential,
			}) != nil {
			return nil, handler.failAcquisition(claim, errors.Join(errCapabilityUnavailable, bindErr))
		}
	}
	if err := handler.markCompositeReady(claim); err != nil {
		return nil, handler.failAcquisition(claim, err)
	}
	turnCapability := "not-required"
	if handler.turnProvider != nil {
		turnCapability = "bound"
	}
	payload := LeasePayload{
		ProtocolVersion: RemotePionProtocolVersion, RequestID: lease.RequestID,
		ReleaseRequestID: scope.ReleaseRequestID, RevokeRequestID: scope.RevokeRequestID,
		LeaseID: lease.LeaseID, RunID: lease.RunID, ProfileID: lease.ProfileID,
		ProbeNonce: lease.ProbeNonce, AuthorityInstanceID: lease.AuthorityInstanceID,
		AttestationSHA256: lease.AttestationSHA256,
		IssuedAt:          issuedAt.Format(canonicalTimestampLayout), ExpiresAt: expiresAt.Format(canonicalTimestampLayout),
		MaxAttempts: 1, CredentialByteLength: len(lease.Credential),
		TURNCapability: turnCapability, TURNProviderLeaseID: reservation.ProviderLeaseID,
		TURNCredentialID: reservation.CredentialID, TURNUsername: reservation.Username,
		TURNExpiresAt: reservation.ExpiresAt,
	}
	return encodeLeaseFrame(handler.signer, payload, lease.Credential)
}

func (handler *Handler) replayReadyAcquisition(claim *acquisitionClaim) ([]byte, error) {
	var turnDeclaration *browsermatrixpion.ControlTURNDeclaration
	if handler.turnProvider != nil {
		turnDeclaration = &browsermatrixpion.ControlTURNDeclaration{
			CredentialID: claim.turnCredentialID, Username: claim.turnUsername,
			ExpiresAt: claim.turnExpiresAt,
		}
	}
	lease, err := handler.admin.AcquireControlCredential(browsermatrixpion.ControlCredentialAcquireRequest{
		RequestID: claim.scope.RequestID, RunID: claim.scope.RunID, ProfileID: claim.scope.ProfileID,
		ProbeNonce: claim.scope.ProbeNonce, RequestedLeaseMillis: handler.leaseDuration.Milliseconds(),
		TURN: turnDeclaration,
	})
	defer erase(lease.Credential)
	issuedAt, expiresAt, validationErr := validateControlLease(
		claim.scope, lease, handler.clock.Now().UTC(), turnDeclaration,
	)
	handler.mu.Lock()
	composite := handler.leases[claim.leaseID]
	validComposite := composite != nil && composite.owner == claim && composite.ready && !composite.terminal &&
		composite.leaseID == lease.LeaseID && composite.authorityID == lease.AuthorityInstanceID &&
		composite.attestationSHA256 == lease.AttestationSHA256 && composite.controlExpiresAt == lease.ExpiresAt
	handler.mu.Unlock()
	if err != nil || validationErr != nil || !validComposite {
		return nil, errors.Join(errOperationConflict, err, validationErr)
	}
	turnCapability := "not-required"
	if handler.turnProvider != nil {
		turnCapability = "bound"
	}
	payload := LeasePayload{
		ProtocolVersion: RemotePionProtocolVersion, RequestID: lease.RequestID,
		ReleaseRequestID: claim.scope.ReleaseRequestID, RevokeRequestID: claim.scope.RevokeRequestID,
		LeaseID: lease.LeaseID, RunID: lease.RunID, ProfileID: lease.ProfileID,
		ProbeNonce: lease.ProbeNonce, AuthorityInstanceID: lease.AuthorityInstanceID,
		AttestationSHA256: lease.AttestationSHA256,
		IssuedAt:          issuedAt.Format(canonicalTimestampLayout), ExpiresAt: expiresAt.Format(canonicalTimestampLayout),
		MaxAttempts: 1, CredentialByteLength: len(lease.Credential), TURNCapability: turnCapability,
		TURNProviderLeaseID: claim.providerLeaseID, TURNCredentialID: claim.turnCredentialID,
		TURNUsername: claim.turnUsername, TURNExpiresAt: claim.turnExpiresAt,
	}
	return encodeLeaseFrame(handler.signer, payload, lease.Credential)
}

func (handler *Handler) acquireTURNReservation(
	ctx context.Context,
	claim *acquisitionClaim,
) (TURNReservation, error) {
	if claim.turnExpiresAt == "" {
		claim.turnExpiresAt = handler.clock.Now().UTC().Truncate(time.Millisecond).
			Add(handler.leaseDuration).Format(canonicalTimestampLayout)
	}
	request := TURNAcquireRequest{
		RequestID: claim.scope.RequestID, RunID: claim.scope.RunID,
		ProfileID: claim.scope.ProfileID, ProbeNonce: claim.scope.ProbeNonce,
		ExpiresAt: claim.turnExpiresAt, MaxAttempts: 1,
	}
	reservation, err := handler.turnProvider.Acquire(ctx, request)
	recordErr := handler.recordTURNMaterialization(claim, reservation)
	if err != nil || recordErr != nil || !exactTURNReservation(request, reservation) ||
		!validCredentialBytes(reservation.Credential) {
		return reservation, errors.Join(
			errors.New("revocable TURN provider rejected the reservation"), err, recordErr,
		)
	}
	return reservation, nil
}

func (handler *Handler) retire(ctx context.Context, scope requestScope) ([]byte, error) {
	claim, normalizedScope, owner, err := handler.retirementClaim(scope)
	if err != nil {
		return nil, err
	}
	if owner {
		go handler.completeRetirement(normalizedScope, claim)
	}
	select {
	case <-claim.done:
		if claim.err != nil {
			return nil, claim.err
		}
		return encodeReceiptFrame(handler.signer, claim.receipt)
	case <-ctx.Done():
		return nil, errors.New("credential broker retirement waiter was cancelled")
	}
}

func (handler *Handler) completeRetirement(scope requestScope, claim *retirementClaim) {
	retirementContext, cancel := context.WithTimeout(context.Background(), handler.retirementTimeout)
	receipt, expiresAt, err := handler.executeRetirement(retirementContext, scope)
	cancel()
	handler.mu.Lock()
	if claim.scope != scope || !claim.inProgress {
		handler.mu.Unlock()
		panic("credential broker retirement lost its exact registry owner")
	}
	claim.err = err
	claim.inProgress = false
	if err == nil {
		claim.receipt = receipt
		claim.expiresAt = expiresAt
		claim.completed = true
	}
	close(claim.done)
	handler.mu.Unlock()
}

func (handler *Handler) executeRetirement(
	ctx context.Context,
	scope requestScope,
) (ReceiptPayload, time.Time, error) {
	handler.mu.Lock()
	lease := handler.leases[scope.LeaseID]
	ready := lease != nil && lease.ready
	handler.mu.Unlock()
	if lease == nil || !exactRetirementScope(scope, lease) || !ready {
		return ReceiptPayload{}, time.Time{}, errOperationConflict
	}
	lease.operation.Lock()
	defer lease.operation.Unlock()

	if lease.terminal {
		if lease.terminalOperation != scope.Operation || lease.receipt.RequestID != scope.RequestID {
			return ReceiptPayload{}, time.Time{}, errOperationConflict
		}
		return lease.receipt, lease.expiresAt, nil
	}
	var adminErr, turnErr error
	if !lease.controlSettled {
		var adminReceipt browsermatrixpion.ControlCredentialReceipt
		if scope.Operation == "release" {
			adminReceipt, adminErr = handler.admin.ReleaseControlCredential(scope.LeaseID)
		} else {
			adminReceipt, adminErr = handler.admin.RevokeControlCredentialAndWait(scope.LeaseID)
		}
		if adminErr == nil && adminReceipt.LeaseID == scope.LeaseID && adminReceipt.Terminal == "revoked" {
			lease.controlSettled = true
		} else if adminErr == nil {
			adminErr = errors.New("control authority returned a nonterminal receipt")
		}
	}
	// The provider remains usable until the contained Pion authority proves that
	// every control-authenticated request and attempt is reaped. This ordering
	// prevents a rejected graceful release from destroying a still-owned attempt.
	if lease.controlSettled && !lease.turnSettled {
		turnErr = handler.revokeTURN(ctx, scope, lease)
		if turnErr == nil {
			lease.turnSettled = true
		}
	}
	if !lease.controlSettled || !lease.turnSettled {
		return ReceiptPayload{}, time.Time{}, errors.Join(errCapabilityUnavailable, adminErr, turnErr)
	}
	now := handler.clock.Now().UTC().Truncate(time.Millisecond)
	expiresAt := now.Add(handler.tombstoneRetention)
	if lease.controlExpires.After(expiresAt) {
		expiresAt = lease.controlExpires
	}
	receipt := ReceiptPayload{
		ProtocolVersion: RemotePionProtocolVersion, Operation: scope.Operation,
		RequestID: scope.RequestID, ReleaseRequestID: lease.scope.ReleaseRequestID,
		RevokeRequestID: lease.scope.RevokeRequestID, LeaseID: scope.LeaseID,
		RunID: lease.scope.RunID, ProfileID: lease.scope.ProfileID,
		ProbeNonce: lease.scope.ProbeNonce, AuthorityInstanceID: lease.authorityID,
		AttestationSHA256: lease.attestationSHA256, LeaseExpiresAt: lease.controlExpiresAt,
		ControlTerminal: "revoked", TURNProviderLeaseID: lease.providerLeaseID,
		TURNTerminal: turnTerminal(handler.turnProvider != nil),
		Terminal:     "revoked", RetiredAt: now.Format(canonicalTimestampLayout),
	}
	handler.mu.Lock()
	lease.terminal = true
	lease.terminalOperation = scope.Operation
	lease.expiresAt = expiresAt
	lease.receipt = receipt
	if acquisition := handler.acquisitions[lease.scope.RequestID]; acquisition != nil &&
		expiresAt.After(acquisition.expiresAt) {
		acquisition.expiresAt = expiresAt
	}
	if acquisition := handler.acquisitions[lease.scope.RequestID]; acquisition != nil {
		acquisition.controlOwned = false
		acquisition.providerOwned = false
	}
	handler.mu.Unlock()
	return receipt, expiresAt, nil
}

func (handler *Handler) acquireClaim(scope requestScope) (*acquisitionClaim, error) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.pruneLocked(handler.clock.Now().UTC())
	if claim := handler.acquisitions[scope.RequestID]; claim != nil {
		if claim.scope != scope || handler.requestOwners[scope.ReleaseRequestID] != claim ||
			handler.requestOwners[scope.RevokeRequestID] != claim {
			return nil, errOperationConflict
		}
		claim.inFlight++
		return claim, nil
	}
	if len(handler.acquisitions) >= handler.maximumTombstones ||
		handler.requestOwners[scope.RequestID] != nil ||
		handler.requestOwners[scope.ReleaseRequestID] != nil ||
		handler.requestOwners[scope.RevokeRequestID] != nil {
		return nil, errCapacityExhausted
	}
	claim := &acquisitionClaim{
		scope:     scope,
		inFlight:  1,
		expiresAt: handler.clock.Now().UTC().Add(handler.tombstoneRetention),
	}
	handler.acquisitions[scope.RequestID] = claim
	handler.requestOwners[scope.RequestID] = claim
	handler.requestOwners[scope.ReleaseRequestID] = claim
	handler.requestOwners[scope.RevokeRequestID] = claim
	return claim, nil
}

func (handler *Handler) releaseAcquisitionClaim(claim *acquisitionClaim) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if claim.inFlight < 1 {
		panic("credential broker acquisition lost its exact registry owner")
	}
	claim.inFlight--
}

func (handler *Handler) retirementClaim(
	scope requestScope,
) (*retirementClaim, requestScope, bool, error) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.pruneLocked(handler.clock.Now().UTC())
	lease := handler.leases[scope.LeaseID]
	if lease == nil || !exactRetirementRequest(scope, lease) || !lease.ready {
		return nil, requestScope{}, false, errOperationConflict
	}
	scope.ReleaseRequestID = lease.scope.ReleaseRequestID
	scope.RevokeRequestID = lease.scope.RevokeRequestID
	if claim := handler.retirementClaims[scope.RequestID]; claim != nil {
		if claim.scope != scope {
			return nil, requestScope{}, false, errOperationConflict
		}
		if claim.completed || claim.inProgress {
			return claim, scope, false, nil
		}
		claim.done = make(chan struct{})
		claim.err = nil
		claim.inProgress = true
		return claim, scope, true, nil
	}
	if len(handler.retirementClaims) >= handler.maximumTombstones {
		return nil, requestScope{}, false, errCapacityExhausted
	}
	claim := &retirementClaim{scope: scope, done: make(chan struct{}), inProgress: true}
	handler.retirementClaims[scope.RequestID] = claim
	return claim, scope, true, nil
}

func (handler *Handler) recordTURNMaterialization(
	claim *acquisitionClaim,
	reservation TURNReservation,
) error {
	if !validOpaqueID(reservation.ProviderLeaseID) {
		return errors.New("revocable TURN provider lease ID is invalid")
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if claim.providerLeaseID != "" {
		if claim.providerLeaseID != reservation.ProviderLeaseID ||
			claim.turnCredentialID != reservation.CredentialID ||
			claim.turnUsername != reservation.Username || claim.turnExpiresAt != reservation.ExpiresAt {
			return errOperationConflict
		}
		return nil
	}
	claim.providerLeaseID = reservation.ProviderLeaseID
	claim.turnCredentialID = reservation.CredentialID
	claim.turnUsername = reservation.Username
	claim.turnExpiresAt = reservation.ExpiresAt
	claim.providerOwned = true
	return nil
}

func (handler *Handler) recordControlMaterialization(
	claim *acquisitionClaim,
	control browsermatrixpion.ControlCredentialLease,
	reservation TURNReservation,
) error {
	if !validOpaqueID(control.LeaseID) {
		return errors.New("control authority lease ID is invalid")
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if existing := handler.leases[control.LeaseID]; existing != nil {
		expectedScope := claim.scope
		expectedScope.LeaseID = control.LeaseID
		if existing.owner != claim || existing.scope != expectedScope ||
			existing.providerLeaseID != reservation.ProviderLeaseID ||
			existing.authorityID != control.AuthorityInstanceID ||
			existing.attestationSHA256 != control.AttestationSHA256 ||
			existing.controlExpiresAt != control.ExpiresAt {
			return errOperationConflict
		}
		claim.controlOwned = true
		return nil
	}
	if len(handler.leases) >= handler.maximumTombstones || claim.leaseID != "" {
		return errCapacityExhausted
	}
	controlExpires, _ := parseTimestamp(control.ExpiresAt)
	ownedScope := claim.scope
	ownedScope.LeaseID = control.LeaseID
	lease := &compositeLease{
		owner: claim, scope: ownedScope, leaseID: control.LeaseID,
		authorityID: control.AuthorityInstanceID, attestationSHA256: control.AttestationSHA256,
		controlExpiresAt: control.ExpiresAt, controlExpires: controlExpires,
		providerLeaseID: reservation.ProviderLeaseID, turnCredentialID: reservation.CredentialID,
		turnUsername: reservation.Username, turnExpiresAt: reservation.ExpiresAt,
		turnSettled: handler.turnProvider == nil, expiresAt: controlExpires,
	}
	handler.leases[control.LeaseID] = lease
	claim.leaseID = control.LeaseID
	claim.controlOwned = true
	claim.expiresAt = controlExpires
	return nil
}

func (handler *Handler) markCompositeReady(claim *acquisitionClaim) error {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	lease := handler.leases[claim.leaseID]
	if lease == nil || lease.owner != claim || lease.terminal || claim.failed {
		return errOperationConflict
	}
	lease.ready = true
	claim.ready = true
	return nil
}

func (handler *Handler) failAcquisition(claim *acquisitionClaim, cause error) error {
	handler.mu.Lock()
	claim.failed = true
	handler.mu.Unlock()
	return errors.Join(cause, handler.settleFailedAcquisition(claim))
}

func (handler *Handler) settleFailedAcquisition(claim *acquisitionClaim) error {
	retirementContext, cancel := context.WithTimeout(context.Background(), handler.retirementTimeout)
	defer cancel()
	handler.mu.Lock()
	providerOwned := claim.providerOwned
	controlOwned := claim.controlOwned
	lease := handler.leases[claim.leaseID]
	handler.mu.Unlock()
	var turnErr, controlErr error
	if controlOwned {
		receipt, err := handler.admin.RevokeControlCredentialAndWait(claim.leaseID)
		if err != nil || receipt.LeaseID != claim.leaseID || receipt.Terminal != "revoked" {
			controlErr = errors.Join(err, errors.New("control authority did not prove failed acquisition retirement"))
		} else {
			handler.mu.Lock()
			claim.controlOwned = false
			if lease != nil {
				lease.controlSettled = true
			}
			handler.mu.Unlock()
		}
	}
	if providerOwned && controlErr == nil {
		turnErr = handler.revokeTURNClaim(retirementContext, claim)
		if turnErr == nil {
			handler.mu.Lock()
			claim.providerOwned = false
			if lease != nil {
				lease.turnSettled = true
			}
			handler.mu.Unlock()
		}
	}
	if turnErr == nil && controlErr == nil {
		handler.mu.Lock()
		now := handler.clock.Now().UTC().Truncate(time.Millisecond)
		expiresAt := now.Add(handler.tombstoneRetention)
		if lease != nil {
			lease.terminal = true
			lease.expiresAt = expiresAt
		}
		claim.expiresAt = expiresAt
		handler.mu.Unlock()
	}
	return errors.Join(turnErr, controlErr)
}

func (handler *Handler) revokeTURN(
	ctx context.Context,
	scope requestScope,
	lease *compositeLease,
) error {
	if handler.turnProvider == nil {
		return nil
	}
	return handler.revokeTURNProvider(ctx, TURNRetirementRequest{
		Operation:       scope.Operation,
		RequestID:       derivedRequestID(scope.RequestID, lease.providerLeaseID, scope.Operation),
		ProviderLeaseID: lease.providerLeaseID, ControlLeaseID: lease.leaseID,
		RunID: lease.scope.RunID, ProfileID: lease.scope.ProfileID,
		ProbeNonce: lease.scope.ProbeNonce, AttestationSHA256: lease.attestationSHA256,
	})
}

func (handler *Handler) revokeTURNClaim(
	ctx context.Context,
	claim *acquisitionClaim,
) error {
	if handler.turnProvider == nil {
		return nil
	}
	attestationSHA256 := ""
	handler.mu.Lock()
	providerOwned := claim.providerOwned
	lease := handler.leases[claim.leaseID]
	handler.mu.Unlock()
	if !providerOwned {
		return nil
	}
	if lease != nil {
		attestationSHA256 = lease.attestationSHA256
	}
	return handler.revokeTURNProvider(ctx, TURNRetirementRequest{
		Operation:       "revoke-and-wait",
		RequestID:       derivedRequestID(claim.scope.RevokeRequestID, claim.providerLeaseID, "acquisition-failure"),
		ProviderLeaseID: claim.providerLeaseID, ControlLeaseID: claim.leaseID,
		RunID: claim.scope.RunID, ProfileID: claim.scope.ProfileID,
		ProbeNonce: claim.scope.ProbeNonce, AttestationSHA256: attestationSHA256,
	})
}

func (handler *Handler) revokeTURNProvider(
	ctx context.Context,
	request TURNRetirementRequest,
) error {
	if !validOpaqueID(request.ProviderLeaseID) || !validOpaqueID(request.RequestID) {
		return errors.New("revocable TURN provider lease is invalid")
	}
	receipt, err := handler.turnProvider.RevokeAndWait(ctx, request)
	if err != nil || receipt.RequestID != request.RequestID ||
		receipt.ProviderLeaseID != request.ProviderLeaseID ||
		receipt.Terminal != "revoked" {
		return errors.New("revocable TURN provider did not prove early revocation")
	}
	return nil
}

func (handler *Handler) CloseAndWait(ctx context.Context) error {
	return handler.closeAndWait(ctx, false)
}

func (handler *Handler) ForceCloseAndWait(ctx context.Context) error {
	return handler.closeAndWait(ctx, true)
}

func (handler *Handler) closeAndWait(ctx context.Context, force bool) error {
	if handler == nil || ctx == nil {
		return errors.New("credential broker close authority is incomplete")
	}
	handler.mu.Lock()
	handler.accepting = false
	if force {
		handler.cancelLifecycle()
	}
	if handler.settled {
		handler.mu.Unlock()
		return nil
	}
	activeZero := handler.activeZero
	handler.mu.Unlock()
	select {
	case <-activeZero:
	case <-ctx.Done():
		return errors.New("credential broker active request settlement exceeded its authority")
	}
	select {
	case <-handler.settlementGate:
		defer func() { handler.settlementGate <- struct{}{} }()
	case <-ctx.Done():
		return errors.New("credential broker settlement ownership wait exceeded its authority")
	}
	handler.mu.Lock()
	if handler.settled {
		handler.mu.Unlock()
		return nil
	}
	active := make([]*compositeLease, 0, len(handler.leases))
	for _, lease := range handler.leases {
		if lease.ready && !lease.terminal {
			active = append(active, lease)
		}
	}
	failed := make([]*acquisitionClaim, 0, len(handler.acquisitions))
	for _, claim := range handler.acquisitions {
		if claim.failed && (claim.controlOwned || claim.providerOwned) {
			failed = append(failed, claim)
		}
	}
	handler.mu.Unlock()
	var failures []error
	for _, claim := range failed {
		claim.operation.Lock()
		settlementErr := handler.settleFailedAcquisition(claim)
		claim.operation.Unlock()
		if settlementErr != nil {
			failures = append(failures, settlementErr)
		}
	}
	for _, lease := range active {
		scope := lease.scope
		scope.Operation = "revoke-and-wait"
		scope.RequestID = lease.scope.RevokeRequestID
		scope.LeaseID = lease.leaseID
		retirementContext, cancel := context.WithTimeout(context.Background(), handler.retirementTimeout)
		_, _, err := handler.executeRetirement(retirementContext, scope)
		cancel()
		if err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) != 0 {
		return errors.Join(failures...)
	}
	handler.mu.Lock()
	handler.settled = true
	erase(handler.signer)
	handler.mu.Unlock()
	return nil
}

func (handler *Handler) admit() bool {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if !handler.accepting {
		return false
	}
	if handler.activeRequests == 0 {
		handler.activeZero = make(chan struct{})
	}
	handler.activeRequests++
	return true
}

func (handler *Handler) finishRequest() {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if handler.activeRequests < 1 {
		panic("credential broker lost an admitted request")
	}
	handler.activeRequests--
	if handler.activeRequests == 0 {
		close(handler.activeZero)
	}
}

func (handler *Handler) pruneLocked(now time.Time) {
	for requestID, claim := range handler.acquisitions {
		if claim.inFlight == 0 && !claim.controlOwned && !claim.providerOwned &&
			!claim.expiresAt.IsZero() && !now.Before(claim.expiresAt) {
			delete(handler.acquisitions, requestID)
			delete(handler.requestOwners, claim.scope.RequestID)
			delete(handler.requestOwners, claim.scope.ReleaseRequestID)
			delete(handler.requestOwners, claim.scope.RevokeRequestID)
		}
	}
	for leaseID, lease := range handler.leases {
		if lease.terminal && !now.Before(lease.expiresAt) {
			delete(handler.leases, leaseID)
		}
	}
	for requestID, claim := range handler.retirementClaims {
		if claim.completed && !now.Before(claim.expiresAt) {
			delete(handler.retirementClaims, requestID)
		}
	}
}

func (handler *Handler) emit(scope requestScope, outcome string) {
	if handler.trace == nil {
		return
	}
	handler.trace(TraceEvent{
		Milestone: "credential-broker-operation", Operation: scope.Operation,
		RequestID: scope.RequestID, LeaseID: scope.LeaseID, Outcome: outcome,
	})
}

var (
	errOperationConflict     = errors.New("credential broker operation conflicts with existing authority")
	errCapacityExhausted     = errors.New("credential broker replay capacity is exhausted")
	errCapabilityUnavailable = errors.New("credential broker revocable capability is unavailable")
)

func brokerStatus(err error) int {
	switch {
	case errors.Is(err, errCapacityExhausted):
		return http.StatusTooManyRequests
	case errors.Is(err, errOperationConflict):
		return http.StatusConflict
	default:
		return http.StatusServiceUnavailable
	}
}

func writeBrokerError(writer http.ResponseWriter, status int) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Length", "0")
	writer.WriteHeader(status)
}

func validateControlLease(
	scope requestScope,
	lease browsermatrixpion.ControlCredentialLease,
	now time.Time,
	turn *browsermatrixpion.ControlTURNDeclaration,
) (time.Time, time.Time, error) {
	issuedAt, issuedErr := parseTimestamp(lease.IssuedAt)
	expiresAt, expiresErr := parseTimestamp(lease.ExpiresAt)
	if lease.RequestID != scope.RequestID || lease.RunID != scope.RunID ||
		lease.ProfileID != scope.ProfileID || lease.ProbeNonce != scope.ProbeNonce ||
		!validOpaqueID(lease.LeaseID) || !canonicalIDPattern.MatchString(lease.AuthorityInstanceID) ||
		!sha256Pattern.MatchString(lease.AttestationSHA256) || lease.MaxAttempts != 1 ||
		!validCredentialBytes(lease.Credential) || issuedErr != nil || expiresErr != nil ||
		issuedAt.After(now.Add(maximumAllowedClockSkew)) || !now.Before(expiresAt) ||
		!expiresAt.After(issuedAt) || expiresAt.Sub(issuedAt) > maximumControlLease {
		return time.Time{}, time.Time{}, errors.New("control credential lease is invalid")
	}
	if turn == nil {
		if lease.TURN != nil {
			return time.Time{}, time.Time{}, errors.New("control credential lease injected TURN authority")
		}
	} else if lease.TURN == nil || *lease.TURN != *turn || lease.ExpiresAt != turn.ExpiresAt {
		return time.Time{}, time.Time{}, errors.New("control credential lease changed TURN authority")
	}
	return issuedAt, expiresAt, nil
}

func exactTURNReservation(request TURNAcquireRequest, lease TURNReservation) bool {
	return validOpaqueID(lease.ProviderLeaseID) && lease.RequestID == request.RequestID &&
		lease.RunID == request.RunID && lease.ProfileID == request.ProfileID &&
		lease.ProbeNonce == request.ProbeNonce && canonicalIDPattern.MatchString(lease.CredentialID) &&
		lease.Username != "" && lease.ExpiresAt == request.ExpiresAt && lease.MaxAttempts == 1
}

func exactTURNBoundLease(request TURNBindRequest, lease TURNBoundLease) bool {
	return TURNBindRequest(lease) == request
}

func exactRetirementRequest(scope requestScope, lease *compositeLease) bool {
	if scope.ControllerOrigin != lease.scope.ControllerOrigin || scope.RunID != lease.scope.RunID ||
		scope.ProfileID != lease.scope.ProfileID || scope.ProbeNonce != lease.scope.ProbeNonce ||
		scope.Identity != lease.scope.Identity || scope.LeaseID != lease.leaseID {
		return false
	}
	if scope.Operation == "release" {
		return scope.RequestID == lease.scope.ReleaseRequestID
	}
	return scope.Operation == "revoke-and-wait" && scope.RequestID == lease.scope.RevokeRequestID
}

func exactRetirementScope(scope requestScope, lease *compositeLease) bool {
	return exactRetirementRequest(scope, lease) &&
		scope.ReleaseRequestID == lease.scope.ReleaseRequestID &&
		scope.RevokeRequestID == lease.scope.RevokeRequestID
}

func derivedRequestID(values ...string) string {
	digest := sha256.New()
	for _, value := range values {
		_, _ = digest.Write([]byte(value))
		_, _ = digest.Write([]byte{'\n'})
	}
	return base64.RawURLEncoding.EncodeToString(digest.Sum(nil))
}

func turnTerminal(required bool) string {
	if required {
		return "revoked"
	}
	return "not-required"
}

func validExpectedIdentity(identity WorkloadIdentityBinding) bool {
	return identity.ProtocolVersion == WorkloadIdentityProtocolVersion &&
		identity.Kind == "github-actions-oidc" && canonicalHTTPSOrigin(identity.Issuer) &&
		len(identity.Audience) >= 8 && validRepository(identity.Repository) &&
		validGitRef(identity.Ref) && validWorkflowRef(identity.WorkflowRef) &&
		canonicalHTTPSOrigin(identity.RequestOrigin) && identity.RequestPath != "" &&
		identity.RequestPath[0] == '/' && identity.RequestQuery != "" && identity.RequestQuery[0] == '?'
}

func canonicalControllerOrigin(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil &&
		parsed.Path == "/" && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.String() == value
}

func TargetsBrokerEndpoint(request *http.Request) bool {
	return request != nil && request.URL != nil &&
		(request.URL.Path == BrokerPath || request.URL.EscapedPath() == BrokerPath)
}

func (handler *Handler) exactHTTPRequest(request *http.Request) bool {
	if request == nil || request.URL == nil || request.TLS == nil || request.Method != http.MethodPost ||
		request.URL.Scheme != "" || request.URL.Host != "" || request.URL.User != nil ||
		request.URL.Path != BrokerPath || request.URL.RawPath != "" || request.URL.RawQuery != "" ||
		request.URL.ForceQuery || request.URL.Fragment != "" || request.URL.RawFragment != "" ||
		request.RequestURI != BrokerPath || request.ContentLength <= 0 ||
		request.ContentLength > MaximumFrameBytes || len(request.TransferEncoding) != 0 ||
		len(request.Header.Values("Transfer-Encoding")) != 0 ||
		len(request.Header.Values("Content-Encoding")) != 0 {
		return false
	}
	origin, _ := url.Parse(handler.controllerOrigin)
	return request.Host == origin.Host &&
		exactHeader(request.Header, "Content-Type", BrokerContentType) &&
		exactHeader(request.Header, "Accept", BrokerContentType)
}

func exactHeader(header http.Header, name string, expected string) bool {
	values := header.Values(name)
	return len(values) == 1 && values[0] == expected
}
