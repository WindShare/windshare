package browsermatrixbroker

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/http"
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
