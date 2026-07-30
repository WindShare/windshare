package browsermatrixpion

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	minimumCredentialBytes          = 32
	attemptIdentifierBytes          = 24
	attemptChallengeBytes           = 32
	attestationLeaseIdentifierBytes = 24
	probePath                       = "/v2/stun-probe"
	authorityProbePath              = "/v2/authority-probe"
	turnCredentialPath              = "/v2/turn-credential"
	attemptsPath                    = "/v2/attempts"
	ControlLeaseIDHeader            = "X-WindShare-Control-Lease-ID"
	maximumLeaseLimit               = 5 * time.Minute
	maximumOperationTimeout         = time.Minute
	maximumTombstoneLife            = 24 * time.Hour
	maximumAttemptCapacity          = 65_535
	maximumTombstoneCapacity        = 65_535
)

type STUNProber interface {
	Probe(context.Context, string) error
}

type LeaseTimer interface {
	Stop() bool
}

type LeaseClock interface {
	Now() time.Time
	AfterFunc(time.Duration, func()) LeaseTimer
}

type ServiceConfig struct {
	Fixture                  ExternalFixture
	AttestationSigner        ed25519.PrivateKey
	MaximumLease             time.Duration
	AttemptStartTimeout      time.Duration
	OfferTimeout             time.Duration
	ProbeTimeout             time.Duration
	BodyReadTimeout          time.Duration
	TombstoneRetention       time.Duration
	MaximumActive            int
	MaximumTombstones        int
	Credential               []byte
	AttemptFactory           AttemptFactory
	STUNProber               STUNProber
	Clock                    LeaseClock
	AttemptIDSource          io.Reader
	ChallengeSource          io.Reader
	AttestationLeaseIDSource io.Reader
	ControlLeaseIDSource     io.Reader
	ControlCredentialSource  io.Reader
	Trace                    TraceSink
}

type Service struct {
	instanceID          string
	profileID           string
	fixture             ExternalFixture
	stunEndpoint        string
	controlCredentials  *ControlCredentialAuthority
	controlTURNLeases   map[string]*controlTURNCredentialLease
	maximumLease        time.Duration
	attemptStartTimeout time.Duration
	offerTimeout        time.Duration
	probeTimeout        time.Duration
	bodyReadTimeout     time.Duration
	tombstoneRetention  time.Duration
	maximumActive       int
	maximumTombstones   int
	credential          []byte
	attemptFactory      AttemptFactory
	stunProber          STUNProber
	clock               LeaseClock
	attemptIDSource     io.Reader
	challengeSource     io.Reader
	trace               TraceSink
	entropyMu           sync.Mutex
	attestationMu       sync.Mutex
	lifecycleContext    context.Context
	cancelLifecycle     context.CancelFunc

	mu                    sync.Mutex
	condition             *sync.Cond
	active                map[string]*leasedAttempt
	retiring              map[string]*leasedAttempt
	tombstones            map[string]attemptTombstone
	tombstoneOrder        []string
	requestOwners         map[string]string
	requestTombstones     map[string]string
	requestLeases         map[string]time.Duration
	requestStarts         map[string]chan struct{}
	requestCancels        map[string]context.CancelFunc
	attemptReservations   map[string]string
	requestBindings       map[string]AttemptBinding
	requestControlLeases  map[string]string
	retiringControlLeases map[string]bool
	controlRevocations    map[string]*controlLeaseRevocation
	authorityLeases       map[string]authorityLease
	occupied              int
	starting              int
	containmentFailures   []error
	closed                bool
}

type requestAuthorization struct {
	controlLeaseID string
	finish         func()
	dynamic        bool
}

func (authorization requestAuthorization) owns(controlLeaseID string) bool {
	return !authorization.dynamic || authorization.controlLeaseID == controlLeaseID
}

type attemptState uint8

const (
	attemptActive attemptState = iota + 1
	attemptRetiring
	attemptReaped
)

type leasedAttempt struct {
	attempt            Attempt
	attemptID          string
	requestID          string
	challenge          string
	binding            AttemptBinding
	controlLeaseID     string
	leaseLength        time.Duration
	leaseIssuedAt      time.Time
	expiresAt          time.Time
	authorityIssuedAt  time.Time
	authorityExpiresAt time.Time
	controlExpiresAt   time.Time
	lease              context.Context
	leaseCancel        context.CancelFunc
	timer              LeaseTimer
	operation          sync.Mutex
	terminalReceipt    *SignedAttemptTerminalReceipt
	state              attemptState
	precloseErr        error
	preclosed          bool
	reaped             chan struct{}
	retireErr          error
}

type attemptTombstone struct {
	attemptID       string
	requestID       string
	challenge       string
	controlLeaseID  string
	expiresAt       time.Time
	terminal        string
	err             error
	binding         AttemptBinding
	terminalReceipt *SignedAttemptTerminalReceipt
}

type attemptReservation struct {
	attemptID      string
	requestID      string
	challenge      string
	binding        AttemptBinding
	controlLeaseID string
}

type controlLeaseRevocation struct {
	done      chan struct{}
	receipt   ControlCredentialReceipt
	err       error
	expiresAt time.Time
	completed bool
}

type authorityLease struct {
	response         AuthorityProbeResponse
	binding          AttemptBinding
	controlLeaseID   string
	controlExpiresAt time.Time
	issuedAt         time.Time
	expiresAt        time.Time
	requested        time.Duration
}

func attemptAuthorityFromParts(
	binding AttemptBinding,
	requestID string,
	attemptID string,
	challenge string,
) AttemptAuthority {
	return AttemptAuthority{
		SchemaVersion: AttemptAuthoritySchemaVersion,
		RequestAuthority: AttemptRequestAuthority{
			SchemaVersion:    AttemptRequestAuthoritySchemaVersion,
			ControlAuthority: binding.ControlAuthority,
			RequestID:        requestID,
			FixtureBinding:   binding.FixtureBinding,
		},
		AttemptID: attemptID,
		Challenge: challenge,
	}
}

func NewService(config ServiceConfig) (*Service, error) {
	fixture := cloneExternalFixture(config.Fixture)
	if err := ValidateExternalFixture(fixture); err != nil {
		return nil, err
	}
	portCapacity := int(fixture.RemotePeerUDPPortMax-fixture.RemotePeerUDPPortMin) + 1
	if len(config.AttestationSigner) != ed25519.PrivateKeySize ||
		config.MaximumLease <= 0 || config.MaximumLease > maximumLeaseLimit ||
		config.AttemptStartTimeout <= 0 || config.AttemptStartTimeout > maximumOperationTimeout ||
		config.OfferTimeout <= 0 || config.OfferTimeout > maximumOperationTimeout ||
		config.ProbeTimeout <= 0 || config.ProbeTimeout > maximumOperationTimeout ||
		config.BodyReadTimeout <= 0 || config.BodyReadTimeout > maximumOperationTimeout ||
		config.TombstoneRetention <= 0 || config.TombstoneRetention > maximumTombstoneLife ||
		config.MaximumActive < 1 || config.MaximumActive > portCapacity ||
		config.MaximumActive > maximumAttemptCapacity ||
		config.MaximumTombstones < config.MaximumActive ||
		config.MaximumTombstones > maximumTombstoneCapacity ||
		len(config.Credential) < minimumCredentialBytes ||
		config.AttemptFactory == nil || config.STUNProber == nil {
		return nil, errors.New("remote Pion service authority is incomplete")
	}
	clock := config.Clock
	if clock == nil {
		clock = realLeaseClock{}
	}
	if fixture.ProfileID == "scheduled-coturn" {
		credentialExpiry, _ := parseCanonicalTimestamp(fixture.NetworkSemantics.TURNCredentialExpiresAt)
		if !clock.Now().UTC().Before(credentialExpiry) {
			return nil, errors.New("remote Pion TURN credential declaration is expired")
		}
	}
	attemptIDSource := config.AttemptIDSource
	if attemptIDSource == nil {
		attemptIDSource = rand.Reader
	}
	challengeSource := config.ChallengeSource
	if challengeSource == nil {
		challengeSource = rand.Reader
	}
	stunEndpoint := ""
	if fixture.NetworkSemantics.Kind == NetworkSemanticsPublicSTUN ||
		fixture.NetworkSemantics.Kind == NetworkSemanticsManualRealNAT {
		stunEndpoint = fixture.NetworkSemantics.STUNEndpoint
	}
	controlCredentials, err := NewControlCredentialAuthority(ControlCredentialAuthorityConfig{
		Fixture: fixture, AttestationSigner: config.AttestationSigner,
		MaximumLease: config.MaximumLease, TombstoneRetention: config.TombstoneRetention,
		MaximumClaims: config.MaximumTombstones,
		Clock:         clock, CredentialSource: config.ControlCredentialSource,
		ControlLeaseIDSource:     config.ControlLeaseIDSource,
		AttestationLeaseIDSource: config.AttestationLeaseIDSource,
	})
	if err != nil {
		return nil, err
	}
	lifecycleContext, cancelLifecycle := context.WithCancel(context.Background())
	service := &Service{
		instanceID: fixture.RemoteServiceInstanceID, profileID: fixture.ProfileID,
		fixture: fixture, stunEndpoint: stunEndpoint,
		controlCredentials: controlCredentials,
		controlTURNLeases:  make(map[string]*controlTURNCredentialLease),
		maximumLease:       config.MaximumLease, attemptStartTimeout: config.AttemptStartTimeout,
		offerTimeout: config.OfferTimeout, probeTimeout: config.ProbeTimeout,
		bodyReadTimeout: config.BodyReadTimeout, tombstoneRetention: config.TombstoneRetention,
		maximumActive: config.MaximumActive, maximumTombstones: config.MaximumTombstones,
		credential: append([]byte(nil), config.Credential...), attemptFactory: config.AttemptFactory,
		stunProber: config.STUNProber, clock: clock,
		attemptIDSource: attemptIDSource, challengeSource: challengeSource,
		trace:            config.Trace,
		lifecycleContext: lifecycleContext, cancelLifecycle: cancelLifecycle,
		active: make(map[string]*leasedAttempt), retiring: make(map[string]*leasedAttempt),
		tombstones: make(map[string]attemptTombstone), requestOwners: make(map[string]string),
		requestTombstones: make(map[string]string),
		requestLeases:     make(map[string]time.Duration), requestStarts: make(map[string]chan struct{}),
		requestCancels:      make(map[string]context.CancelFunc),
		attemptReservations: make(map[string]string), requestBindings: make(map[string]AttemptBinding),
		requestControlLeases:  make(map[string]string),
		retiringControlLeases: make(map[string]bool),
		controlRevocations:    make(map[string]*controlLeaseRevocation),
		authorityLeases:       make(map[string]authorityLease),
	}
	service.condition = sync.NewCond(&service.mu)
	return service, nil
}

func (service *Service) AcquireControlCredential(
	request ControlCredentialAcquireRequest,
) (ControlCredentialLease, error) {
	if service.unavailable() {
		return ControlCredentialLease{}, errors.New("remote Pion service is unavailable")
	}
	service.mu.Lock()
	service.pruneControlRetirementsLocked(service.clock.Now().UTC())
	service.mu.Unlock()
	return service.controlCredentials.Acquire(request)
}

func (service *Service) ReleaseControlCredential(
	leaseID string,
) (ControlCredentialReceipt, error) {
	if !validOpaqueID(leaseID) {
		return ControlCredentialReceipt{}, errors.New("control credential lease ID is invalid")
	}
	service.mu.Lock()
	service.pruneControlRetirementsLocked(service.clock.Now().UTC())
	if current := service.controlRevocations[leaseID]; current != nil {
		service.mu.Unlock()
		<-current.done
		return current.receipt, current.err
	}
	if service.closed || len(service.containmentFailures) != 0 ||
		service.controlLeaseOwnsAttemptLocked(leaseID) {
		service.mu.Unlock()
		return ControlCredentialReceipt{}, errors.New("control credential lease still owns an attempt")
	}
	authorityState, exists := service.controlCredentials.retirementState(leaseID)
	if !exists {
		service.mu.Unlock()
		return ControlCredentialReceipt{}, errors.New("control credential lease is not active")
	}
	if len(service.controlRevocations) >= service.maximumTombstones {
		service.mu.Unlock()
		return ControlCredentialReceipt{}, errors.New("control credential retirement capacity is exhausted")
	}
	retirement := &controlLeaseRevocation{
		done: make(chan struct{}), expiresAt: authorityState.expiresAt,
	}
	service.controlRevocations[leaseID] = retirement
	service.retiringControlLeases[leaseID] = true
	if authorityState.completed {
		retirement.receipt = authorityState.receipt
		retirement.completed = true
		service.retireControlTURNCredentialLocked(leaseID)
		close(retirement.done)
		service.mu.Unlock()
		return retirement.receipt, nil
	}
	service.mu.Unlock()

	receipt, err := service.controlCredentials.Release(leaseID)
	service.completeControlLeaseRetirement(leaseID, retirement, receipt, err, false)
	return receipt, err
}

func (service *Service) RevokeControlCredentialAndWait(
	leaseID string,
) (ControlCredentialReceipt, error) {
	if !validOpaqueID(leaseID) {
		return ControlCredentialReceipt{}, errors.New("control credential lease ID is invalid")
	}
	service.mu.Lock()
	service.pruneControlRetirementsLocked(service.clock.Now().UTC())
	if current := service.controlRevocations[leaseID]; current != nil {
		done := current.done
		service.mu.Unlock()
		<-done
		return current.receipt, current.err
	}
	authorityState, exists := service.controlCredentials.retirementState(leaseID)
	if !exists {
		service.mu.Unlock()
		return ControlCredentialReceipt{}, errors.New("control credential lease is not active")
	}
	if len(service.controlRevocations) >= service.maximumTombstones {
		service.mu.Unlock()
		return ControlCredentialReceipt{}, errors.New("control credential retirement capacity is exhausted")
	}
	revocation := &controlLeaseRevocation{
		done: make(chan struct{}), expiresAt: authorityState.expiresAt,
	}
	service.controlRevocations[leaseID] = revocation
	service.retiringControlLeases[leaseID] = true
	if authorityState.completed {
		revocation.receipt = authorityState.receipt
		revocation.completed = true
		service.retireControlTURNCredentialLocked(leaseID)
		close(revocation.done)
		service.mu.Unlock()
		return revocation.receipt, nil
	}
	service.mu.Unlock()

	var receipt ControlCredentialReceipt
	replayed, completed, err := service.controlCredentials.beginRevocation(leaseID)
	if err == nil && completed {
		receipt = replayed
	}
	if err == nil && !completed {
		containmentErr := service.revokeControlLeaseAttemptsAndWait(leaseID)
		requestErr := service.controlCredentials.waitForRevocationRequests(leaseID)
		err = errors.Join(containmentErr, requestErr)
	}
	if err == nil && !completed {
		receipt, err = service.controlCredentials.finishRevocation(leaseID)
	}
	service.completeControlLeaseRetirement(leaseID, revocation, receipt, err, true)
	return receipt, err
}

func (service *Service) completeControlLeaseRetirement(
	leaseID string,
	retirement *controlLeaseRevocation,
	receipt ControlCredentialReceipt,
	err error,
	retainFailure bool,
) {
	expiresAt := retirement.expiresAt
	if authorityState, exists := service.controlCredentials.retirementState(leaseID); exists {
		expiresAt = authorityState.expiresAt
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.controlRevocations[leaseID] != retirement || retirement.completed {
		panic("control credential retirement lost its exact registry owner")
	}
	retirement.receipt = receipt
	retirement.err = err
	retirement.expiresAt = expiresAt
	retirement.completed = true
	if err == nil {
		service.retireControlTURNCredentialLocked(leaseID)
	}
	close(retirement.done)
	if err != nil && !retainFailure {
		delete(service.controlRevocations, leaseID)
		delete(service.retiringControlLeases, leaseID)
	}
}

func (service *Service) pruneControlRetirementsLocked(now time.Time) {
	for leaseID, retirement := range service.controlRevocations {
		if retirement.completed && !now.Before(retirement.expiresAt) {
			delete(service.controlRevocations, leaseID)
			delete(service.retiringControlLeases, leaseID)
		}
	}
}

func (service *Service) controlLeaseRetiringLocked(leaseID string) bool {
	service.pruneControlRetirementsLocked(service.clock.Now().UTC())
	return service.retiringControlLeases[leaseID]
}

func (service *Service) controlLeaseOwnsAttemptLocked(leaseID string) bool {
	for _, ownerLeaseID := range service.requestControlLeases {
		if ownerLeaseID == leaseID {
			return true
		}
	}
	return false
}

// revokeControlLeaseAttemptsAndWait closes the registry race in both directions:
// a startup sees the retirement marker before publication, while revocation waits
// for the request's exact startup/reap signal before it can return a receipt.
func (service *Service) revokeControlLeaseAttemptsAndWait(leaseID string) error {
	for {
		var cancellations []context.CancelFunc
		startups := make(map[<-chan struct{}]struct{})
		reaping := make(map[*leasedAttempt]bool)

		service.mu.Lock()
		owned := service.controlLeaseOwnsAttemptLocked(leaseID)
		for requestID, ownerLeaseID := range service.requestControlLeases {
			if ownerLeaseID != leaseID {
				continue
			}
			if cancel := service.requestCancels[requestID]; cancel != nil {
				cancellations = append(cancellations, cancel)
			}
			if startup := service.requestStarts[requestID]; startup != nil {
				startups[startup] = struct{}{}
			}
		}
		for attemptID, entry := range service.active {
			if entry.controlLeaseID != leaseID {
				continue
			}
			if transitioned, owner := service.transitionToRetiringLocked(attemptID, entry); transitioned != nil {
				reaping[transitioned] = owner
			}
		}
		for _, entry := range service.retiring {
			if entry.controlLeaseID == leaseID {
				if _, exists := reaping[entry]; !exists {
					reaping[entry] = false
				}
			}
		}
		service.mu.Unlock()

		for _, cancel := range cancellations {
			cancel()
		}
		for entry, owner := range reaping {
			if owner {
				go service.reap(entry)
			}
		}
		for startup := range startups {
			<-startup
		}
		for entry := range reaping {
			<-entry.reaped
		}
		if !owned {
			break
		}
	}

	service.mu.Lock()
	defer service.mu.Unlock()
	if service.controlLeaseOwnsAttemptLocked(leaseID) ||
		len(service.containmentFailures) != 0 {
		return errors.New("control credential attempt containment failed")
	}
	return nil
}

func (service *Service) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if status, code := service.exactHTTPRequestStatus(request); status != 0 {
		writeProtocolError(w, status, code)
		return
	}
	if status, code := service.exactAttemptRequestStatus(request); status != 0 {
		writeProtocolError(w, status, code)
		return
	}
	authorization, authorized := service.authorizeRequest(request)
	if !authorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeProtocolError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if authorization.finish != nil {
		defer authorization.finish()
	}
	if service.unavailable() && request.Method != http.MethodDelete {
		writeProtocolError(w, http.StatusServiceUnavailable, "service-containment-required")
		return
	}
	switch {
	case request.URL.Path == authorityProbePath:
		service.handleAuthorityProbe(w, request, authorization)
	case request.URL.Path == probePath:
		service.handleProbe(w, request, authorization)
	case request.URL.Path == turnCredentialPath:
		service.handleTURNCredential(w, request, authorization)
	case request.URL.Path == attemptsPath:
		service.handleAttempts(w, request, authorization)
	case strings.HasPrefix(request.URL.Path, attemptsPath+"/"):
		service.handleAttempt(w, request, authorization)
	default:
		writeProtocolError(w, http.StatusNotFound, "not-found")
	}
}

func (service *Service) handleAuthorityProbe(
	w http.ResponseWriter,
	request *http.Request,
	authorization requestAuthorization,
) {
	if request.Method != http.MethodPost {
		writeProtocolError(w, http.StatusMethodNotAllowed, "method-not-allowed")
		return
	}
	var input AuthorityProbeRequest
	if service.decodeBody(w, request, &input) != nil || validateAuthorityProbeRequest(input) != nil {
		writeProtocolError(w, http.StatusBadRequest, "invalid-authority-probe")
		return
	}
	credential, _ := bearerCredential(request)
	response, status := service.issueAuthorityLease(
		input,
		authorization.controlLeaseID,
		credential,
	)
	if status != 0 {
		outcome := "credential-claim-rejected"
		if status == http.StatusTooManyRequests {
			outcome = "lease-capacity-rejected"
		} else if status == http.StatusServiceUnavailable {
			outcome = "issuance-unavailable"
		}
		emitTrace(service.trace, TraceEvent{
			Milestone: traceAuthorityProbe, InstanceID: service.instanceID,
			RunID: input.ControlAuthority.SampleAuthority.RunID, Outcome: outcome,
		})
		writeProtocolError(w, status, "authority-attestation-unavailable")
		return
	}
	emitTrace(service.trace, TraceEvent{
		Milestone: traceAuthorityProbe, InstanceID: service.instanceID,
		RunID:             input.ControlAuthority.SampleAuthority.RunID,
		AttestationSHA256: response.AttestationSHA256, Outcome: "available",
	})
	writeJSON(w, http.StatusOK, response)
}

func (service *Service) issueAuthorityLease(
	input AuthorityProbeRequest,
	controlLeaseID string,
	credential string,
) (AuthorityProbeResponse, int) {
	service.attestationMu.Lock()
	defer service.attestationMu.Unlock()

	now := service.clock.Now().UTC().Truncate(time.Millisecond)
	service.mu.Lock()
	service.pruneAuthorityLeasesLocked(now)
	if service.closed || len(service.containmentFailures) != 0 {
		service.mu.Unlock()
		return AuthorityProbeResponse{}, http.StatusServiceUnavailable
	}
	if len(service.authorityLeases) >= service.maximumTombstones {
		service.mu.Unlock()
		return AuthorityProbeResponse{}, http.StatusTooManyRequests
	}
	service.mu.Unlock()

	response, claimedControlLeaseID, expiresAt, accepted :=
		service.controlCredentials.consumeAuthorityProbe(
			controlLeaseID,
			credential,
			input,
			service.profileID,
		)
	if !accepted {
		return AuthorityProbeResponse{}, http.StatusConflict
	}
	if response.Attestation.Fixture.ProfileID != service.profileID ||
		response.Attestation.Fixture.RemoteServiceInstanceID != service.instanceID {
		return AuthorityProbeResponse{}, http.StatusInternalServerError
	}
	issuedAt, err := parseCanonicalTimestamp(response.Attestation.IssuedAt)
	if err != nil || response.Attestation.ExpiresAt != expiresAt.UTC().Format(canonicalTimestampLayout) {
		return AuthorityProbeResponse{}, http.StatusInternalServerError
	}
	requested := time.Duration(input.RequestedLeaseMillis) * time.Millisecond
	networkBinding, bindingErr := NetworkBindingSHA256(response.Attestation.Fixture)
	remotePeerBinding, remoteBindingErr := RemotePeerBindingSHA256FromFixture(response.Attestation.Fixture)
	if bindingErr != nil || remoteBindingErr != nil {
		return AuthorityProbeResponse{}, http.StatusInternalServerError
	}
	lease := authorityLease{
		response: response,
		binding: AttemptBinding{
			ControlAuthority: input.ControlAuthority,
			FixtureBinding: AttemptFixtureBinding{
				AttestationSHA256:       response.AttestationSHA256,
				AuthorityInstanceID:     response.Attestation.Fixture.AuthorityInstanceID,
				RemoteServiceInstanceID: service.instanceID,
				NetworkBindingSHA256:    networkBinding,
				RemotePeerBindingSHA256: remotePeerBinding,
			},
		},
		controlLeaseID:   claimedControlLeaseID,
		controlExpiresAt: expiresAt,
		issuedAt:         issuedAt,
		expiresAt:        expiresAt,
		requested:        requested,
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.closed || len(service.containmentFailures) != 0 {
		return AuthorityProbeResponse{}, http.StatusServiceUnavailable
	}
	if _, collision := service.authorityLeases[response.AttestationSHA256]; collision {
		return AuthorityProbeResponse{}, http.StatusInternalServerError
	}
	service.authorityLeases[response.AttestationSHA256] = lease
	return response, 0
}

func (service *Service) pruneAuthorityLeasesLocked(now time.Time) {
	for digest, lease := range service.authorityLeases {
		if !now.Before(lease.expiresAt) {
			delete(service.authorityLeases, digest)
		}
	}
}

func (service *Service) Close() error {
	service.mu.Lock()
	if !service.closed {
		service.closed = true
		service.cancelLifecycle()
	}
	owners := make([]*leasedAttempt, 0, len(service.active))
	for _, entry := range service.active {
		if transitioned, owner := service.transitionToRetiringLocked(entry.attemptID, entry); owner {
			owners = append(owners, transitioned)
		}
	}
	service.mu.Unlock()
	for _, entry := range owners {
		go service.reap(entry)
	}

	service.mu.Lock()
	for service.starting != 0 || len(service.retiring) != 0 {
		service.condition.Wait()
	}
	failures := append([]error(nil), service.containmentFailures...)
	for index := range service.credential {
		service.credential[index] = 0
	}
	for controlLeaseID := range service.controlTURNLeases {
		service.retireControlTURNCredentialLocked(controlLeaseID)
	}
	service.mu.Unlock()
	service.controlCredentials.Close()
	if len(failures) == 0 {
		return nil
	}
	return errors.New("remote Pion containment failed")
}

func (service *Service) handleProbe(
	w http.ResponseWriter,
	request *http.Request,
	authorization requestAuthorization,
) {
	if request.Method != http.MethodPost {
		writeProtocolError(w, http.StatusMethodNotAllowed, "method-not-allowed")
		return
	}
	var input ProbeRequest
	if service.decodeBody(w, request, &input) != nil || validateProbeRequest(input) != nil {
		writeProtocolError(w, http.StatusBadRequest, "invalid-probe")
		return
	}
	lease, ok := service.authorityLeaseForBinding(input.AttemptBinding)
	if !ok || !authorization.owns(lease.controlLeaseID) ||
		service.stunEndpoint == "" || input.STUNURI != service.stunEndpoint ||
		input.Nonce != lease.response.Attestation.Nonce {
		emitTrace(service.trace, TraceEvent{
			Milestone: traceSTUNProbe, InstanceID: service.instanceID,
			RunID:             input.ControlAuthority.SampleAuthority.RunID,
			AttestationSHA256: input.FixtureBinding.AttestationSHA256,
			Outcome:           "binding-mismatch",
		})
		writeProtocolError(w, http.StatusConflict, "authority-binding-mismatch")
		return
	}
	probeDeadline := service.clock.Now().UTC().Add(service.probeTimeout)
	if lease.expiresAt.Before(probeDeadline) {
		probeDeadline = lease.expiresAt
	}
	ctx, cancel := context.WithDeadline(request.Context(), probeDeadline)
	stop := context.AfterFunc(service.lifecycleContext, cancel)
	defer func() { stop(); cancel() }()
	if err := service.stunProber.Probe(ctx, service.stunEndpoint); err != nil ||
		!service.clock.Now().UTC().Before(lease.expiresAt) {
		emitTrace(service.trace, TraceEvent{
			Milestone: traceSTUNProbe, InstanceID: service.instanceID,
			RunID:             input.ControlAuthority.SampleAuthority.RunID,
			AttestationSHA256: input.FixtureBinding.AttestationSHA256,
			Outcome:           "failed",
		})
		writeProtocolError(w, http.StatusBadGateway, "stun-probe-failed")
		return
	}
	emitTrace(service.trace, TraceEvent{
		Milestone: traceSTUNProbe, InstanceID: service.instanceID,
		RunID:             input.ControlAuthority.SampleAuthority.RunID,
		AttestationSHA256: input.FixtureBinding.AttestationSHA256,
		Outcome:           "server-reflexive-observed",
	})
	writeJSON(w, http.StatusOK, ProbeResponse{
		ProtocolVersion: ProtocolVersion, AttemptBinding: input.AttemptBinding,
		Nonce:                   input.Nonce,
		ServerReflexiveObserved: true,
	})
}

func (service *Service) handleTURNCredential(
	w http.ResponseWriter,
	request *http.Request,
	authorization requestAuthorization,
) {
	if request.Method != http.MethodPost {
		writeProtocolError(w, http.StatusMethodNotAllowed, "method-not-allowed")
		return
	}
	var input TURNCredentialRequest
	if service.decodeBody(w, request, &input) != nil || validateTURNCredentialRequest(input) != nil {
		writeProtocolError(w, http.StatusBadRequest, "invalid-turn-credential-request")
		return
	}
	lease, ok := service.authorityLeaseForBinding(input.AttemptBinding)
	if !ok || !authorization.dynamic || !authorization.owns(lease.controlLeaseID) ||
		service.profileID != "scheduled-coturn" {
		emitTrace(service.trace, TraceEvent{
			Milestone: traceTURNCredential, InstanceID: service.instanceID,
			RunID:             input.ControlAuthority.SampleAuthority.RunID,
			AttestationSHA256: input.FixtureBinding.AttestationSHA256,
			Outcome:           "binding-mismatch",
		})
		writeProtocolError(w, http.StatusConflict, "authority-binding-mismatch")
		return
	}
	service.mu.Lock()
	if service.closed || len(service.containmentFailures) != 0 {
		service.mu.Unlock()
		writeProtocolError(w, http.StatusServiceUnavailable, "service-containment-required")
		return
	}
	turnLease, credentialBytes, available := service.beginControlTURNCredentialDeliveryLocked(
		lease.controlLeaseID,
		input.FixtureBinding.AttestationSHA256,
		service.clock.Now().UTC(),
	)
	service.mu.Unlock()
	if !available {
		writeProtocolError(w, http.StatusConflict, "turn-credential-capability-consumed")
		return
	}
	defer eraseCredentialBytes(credentialBytes)
	deliveryErr := writeControlTURNCredentialResponse(w, input.AttemptBinding, turnLease, credentialBytes)
	service.mu.Lock()
	service.finishControlTURNCredentialDeliveryLocked(lease.controlLeaseID)
	service.mu.Unlock()
	outcome := "issued"
	if deliveryErr != nil {
		outcome = "delivery-ambiguous-revoking"
	}
	emitTrace(service.trace, TraceEvent{
		Milestone: traceTURNCredential, InstanceID: service.instanceID,
		RunID:             input.ControlAuthority.SampleAuthority.RunID,
		AttestationSHA256: input.FixtureBinding.AttestationSHA256,
		Outcome:           outcome,
	})
	if deliveryErr != nil {
		// Retirement waits for this request's authorization owner to return, so the
		// asynchronous call cannot deadlock the response path it is containing.
		go func() { _, _ = service.RevokeControlCredentialAndWait(lease.controlLeaseID) }()
	}
}

func (service *Service) authorityLeaseForBinding(binding AttemptBinding) (authorityLease, bool) {
	if validateAttemptBinding(binding) != nil {
		return authorityLease{}, false
	}
	now := service.clock.Now().UTC()
	service.mu.Lock()
	defer service.mu.Unlock()
	service.pruneAuthorityLeasesLocked(now)
	lease, exists := service.authorityLeases[binding.FixtureBinding.AttestationSHA256]
	return lease, exists && lease.binding == binding && now.Before(lease.expiresAt)
}

func (service *Service) handleAttempts(
	w http.ResponseWriter,
	request *http.Request,
	authorization requestAuthorization,
) {
	if request.Method != http.MethodPost {
		writeProtocolError(w, http.StatusMethodNotAllowed, "method-not-allowed")
		return
	}
	var input CreateAttemptRequest
	if service.decodeBody(w, request, &input) != nil {
		writeProtocolError(w, http.StatusBadRequest, "invalid-attempt")
		return
	}
	lease, err := validateCreateRequest(input, service.maximumLease)
	if err != nil {
		writeProtocolError(w, http.StatusBadRequest, "invalid-attempt")
		return
	}
	requestAuthority := input.RequestAuthority
	binding := AttemptBinding{
		ControlAuthority: requestAuthority.ControlAuthority,
		FixtureBinding:   requestAuthority.FixtureBinding,
	}
	requestID := requestAuthority.RequestID
	authority, authorized := service.authorityLeaseForBinding(binding)
	admittedAt := service.clock.Now().UTC()
	leaseIssuedAt := admittedAt.Truncate(time.Millisecond)
	requestedLeaseDeadline := leaseIssuedAt.Add(lease)
	controlLeaseID := authority.controlLeaseID
	if !authorized || controlLeaseID == "" || !authorization.owns(controlLeaseID) ||
		!service.controlCredentials.leaseActive(controlLeaseID) ||
		leaseIssuedAt.Before(authority.issuedAt) ||
		requestedLeaseDeadline.After(authority.expiresAt) || !requestedLeaseDeadline.After(admittedAt) {
		emitTrace(service.trace, TraceEvent{
			Milestone: traceAttemptStarting, InstanceID: service.instanceID,
			RunID:             requestAuthority.ControlAuthority.SampleAuthority.RunID,
			AttestationSHA256: requestAuthority.FixtureBinding.AttestationSHA256,
			RequestID:         requestID, Outcome: "authority-lease-rejected",
		})
		writeProtocolError(w, http.StatusConflict, "authority-binding-mismatch")
		return
	}
	if authorization.dynamic {
		if err := service.controlCredentials.claimAttempt(controlLeaseID, requestID); err != nil {
			writeProtocolError(w, http.StatusConflict, "attempt-authority-consumed")
			return
		}
	}
	reservation, startup, status := service.reserveAttempt(
		requestID,
		lease,
		binding,
		controlLeaseID,
	)
	if status != 0 {
		writeProtocolError(w, status, "attempt-admission-rejected")
		return
	}
	if reservation.attemptID == "" {
		service.replayCreateAttempt(
			w,
			request,
			requestID,
			lease,
			binding,
			controlLeaseID,
			startup,
		)
		return
	}
	emitTrace(service.trace, TraceEvent{
		Milestone: traceAttemptStarting, InstanceID: service.instanceID,
		RunID:             reservation.binding.ControlAuthority.SampleAuthority.RunID,
		AttestationSHA256: reservation.binding.FixtureBinding.AttestationSHA256,
		RequestID:         reservation.requestID, AttemptID: reservation.attemptID,
	})
	startupDeadline := service.clock.Now().UTC().Add(service.attemptStartTimeout)
	if authority.expiresAt.Before(startupDeadline) {
		startupDeadline = authority.expiresAt
	}
	if requestedLeaseDeadline.Before(startupDeadline) {
		startupDeadline = requestedLeaseDeadline
	}
	ctx, cancel := context.WithDeadline(request.Context(), startupDeadline)
	stop := context.AfterFunc(service.lifecycleContext, cancel)
	if !service.registerStartupCancellation(reservation, cancel) {
		stop()
		cancel()
		service.releaseFailedReservation(reservation, nil)
		writeProtocolError(w, http.StatusConflict, "attempt-rejected")
		return
	}
	attemptAuthority := attemptAuthorityFromParts(
		reservation.binding,
		reservation.requestID,
		reservation.attemptID,
		reservation.challenge,
	)
	attempt, createErr := service.attemptFactory.Create(ctx, attemptAuthority)
	createDeadlineErr := ctx.Err()
	service.unregisterStartupCancellation(reservation)
	stop()
	cancel()
	if createErr != nil || createDeadlineErr != nil || attempt == nil {
		var cleanupErr error
		if attempt != nil {
			cleanupErr = attempt.Close()
		}
		service.releaseFailedReservation(reservation, cleanupErr)
		writeProtocolError(w, http.StatusInternalServerError, "attempt-start-failed")
		return
	}
	now := service.clock.Now().UTC()
	expiresAt := requestedLeaseDeadline
	if authority.expiresAt.Before(expiresAt) {
		expiresAt = authority.expiresAt
	}
	leaseContext, leaseCancel := context.WithDeadline(service.lifecycleContext, expiresAt)
	entry := &leasedAttempt{
		attempt: attempt, attemptID: reservation.attemptID, requestID: reservation.requestID,
		challenge: reservation.challenge, binding: reservation.binding,
		controlLeaseID: reservation.controlLeaseID, leaseLength: lease,
		leaseIssuedAt: leaseIssuedAt, expiresAt: expiresAt,
		authorityIssuedAt: authority.issuedAt, authorityExpiresAt: authority.expiresAt,
		controlExpiresAt: authority.controlExpiresAt,
		lease:            leaseContext, leaseCancel: leaseCancel,
		state: attemptActive, reaped: make(chan struct{}),
	}
	service.mu.Lock()
	service.starting--
	delete(service.attemptReservations, entry.attemptID)
	if service.closed || len(service.containmentFailures) != 0 ||
		service.controlLeaseRetiringLocked(entry.controlLeaseID) || !now.Before(expiresAt) {
		service.retiring[entry.attemptID] = entry
		entry.state = attemptRetiring
		service.condition.Broadcast()
		service.mu.Unlock()
		service.reap(entry)
		writeProtocolError(w, http.StatusConflict, "attempt-rejected")
		return
	}
	service.active[entry.attemptID] = entry
	service.condition.Broadcast()
	service.mu.Unlock()
	emitTrace(service.trace, TraceEvent{
		Milestone: traceAttemptActive, InstanceID: service.instanceID,
		RunID:             entry.binding.ControlAuthority.SampleAuthority.RunID,
		AttestationSHA256: entry.binding.FixtureBinding.AttestationSHA256,
		RequestID:         entry.requestID, AttemptID: entry.attemptID,
	})

	timer := service.clock.AfterFunc(expiresAt.Sub(service.clock.Now().UTC()), func() { service.expireAttempt(entry) })
	service.mu.Lock()
	if timer == nil {
		transitioned, owner := service.transitionToRetiringLocked(entry.attemptID, entry)
		service.mu.Unlock()
		if owner {
			service.reap(transitioned)
		}
		writeProtocolError(w, http.StatusInternalServerError, "attempt-lease-failed")
		return
	}
	activeAfterTimer := entry.state == attemptActive
	if activeAfterTimer {
		entry.timer = timer
		service.signalStartupLocked(entry.requestID)
	} else {
		timer.Stop()
	}
	service.mu.Unlock()
	if !activeAfterTimer {
		<-entry.reaped
		if entry.retireErr != nil {
			writeProtocolError(w, http.StatusInternalServerError, "attempt-containment-failed")
		} else {
			writeProtocolError(w, http.StatusConflict, "attempt-rejected")
		}
		return
	}

	writeJSON(w, http.StatusCreated, CreateAttemptResponse{
		ProtocolVersion: ProtocolVersion, AttemptAuthority: attemptAuthority,
		LeaseIssuedAt:  entry.leaseIssuedAt.Format(canonicalTimestampLayout),
		LeaseExpiresAt: entry.expiresAt.Format(canonicalTimestampLayout),
		LeaseMillis:    entry.leaseLength.Milliseconds(),
	})
}

func (service *Service) replayCreateAttempt(
	w http.ResponseWriter,
	request *http.Request,
	requestID string,
	lease time.Duration,
	binding AttemptBinding,
	controlLeaseID string,
	startup <-chan struct{},
) {
	if startup != nil {
		select {
		case <-startup:
		case <-request.Context().Done():
			writeProtocolError(w, http.StatusRequestTimeout, "attempt-replay-canceled")
			return
		case <-service.lifecycleContext.Done():
			writeProtocolError(w, http.StatusConflict, "attempt-rejected")
			return
		}
	}
	service.mu.Lock()
	attemptID := service.requestOwners[requestID]
	entry := service.active[attemptID]
	if entry == nil || entry.requestID != requestID || entry.leaseLength != lease ||
		entry.binding != binding || entry.controlLeaseID != controlLeaseID ||
		service.controlLeaseRetiringLocked(controlLeaseID) ||
		!service.clock.Now().UTC().Before(entry.expiresAt) {
		service.mu.Unlock()
		writeProtocolError(w, http.StatusConflict, "attempt-admission-rejected")
		return
	}
	response := CreateAttemptResponse{
		ProtocolVersion: ProtocolVersion,
		AttemptAuthority: attemptAuthorityFromParts(
			entry.binding, entry.requestID, entry.attemptID, entry.challenge,
		),
		LeaseIssuedAt:  entry.leaseIssuedAt.Format(canonicalTimestampLayout),
		LeaseExpiresAt: entry.expiresAt.Format(canonicalTimestampLayout),
		LeaseMillis:    entry.leaseLength.Milliseconds(),
	}
	service.mu.Unlock()
	writeJSON(w, http.StatusOK, response)
}

func (service *Service) handleAttempt(
	w http.ResponseWriter,
	request *http.Request,
	authorization requestAuthorization,
) {
	remainder := strings.TrimPrefix(request.URL.Path, attemptsPath+"/")
	parts := strings.Split(remainder, "/")
	if len(parts) == 2 && parts[1] == "offer" {
		service.handleOffer(w, request, parts[0], authorization)
		return
	}
	if len(parts) != 1 || !validOpaqueID(parts[0]) {
		writeProtocolError(w, http.StatusNotFound, "not-found")
		return
	}
	switch request.Method {
	case http.MethodGet:
		entry, tombstone := service.attemptForRead(parts[0], authorization)
		if tombstone != nil {
			result, replayable := terminalAttemptResultFromTombstone(*tombstone)
			if !replayable {
				writeProtocolError(w, http.StatusNotFound, "attempt-not-found")
				return
			}
			writeJSON(w, http.StatusOK, result)
			return
		}
		if entry == nil {
			writeProtocolError(w, http.StatusNotFound, "attempt-not-found")
			return
		}
		if !service.clock.Now().UTC().Before(entry.expiresAt) {
			service.retireRejectedAttempt(parts[0])
			writeProtocolError(w, http.StatusNotFound, "attempt-not-found")
			return
		}
		entry.operation.Lock()
		now := service.clock.Now().UTC()
		if service.activeAttempt(parts[0]) != entry || !now.Before(entry.expiresAt) {
			entry.operation.Unlock()
			service.retireRejectedAttempt(parts[0])
			writeProtocolError(w, http.StatusNotFound, "attempt-not-found")
			return
		}
		result := entry.attempt.Result()
		result.ProtocolVersion = ProtocolVersion
		result.AttemptAuthority = attemptAuthorityFromParts(
			entry.binding, entry.requestID, entry.attemptID, entry.challenge,
		)
		result.ChallengeBindingSHA256 = challengeBindingSHA256(result.AttemptAuthority)
		result.TerminalReceipt = nil
		receiptIssued := false
		if entry.terminalReceipt != nil {
			applyAttemptTerminalReceipt(&result, *entry.terminalReceipt)
		} else if result.State == attemptStateEstablished || result.State == attemptStateFailed {
			signed, err := service.signAttemptTerminalResult(entry, result, now)
			if err != nil {
				entry.operation.Unlock()
				service.rejectInvalidAttemptResult(entry)
				writeProtocolError(w, http.StatusInternalServerError, "attempt-result-invalid")
				return
			}
			entry.terminalReceipt = &signed
			applyAttemptTerminalReceipt(&result, signed)
			receiptIssued = true
		} else if validatePendingAttemptResult(result) != nil {
			entry.operation.Unlock()
			service.rejectInvalidAttemptResult(entry)
			writeProtocolError(w, http.StatusInternalServerError, "attempt-result-invalid")
			return
		}
		entry.operation.Unlock()
		if receiptIssued {
			emitTrace(service.trace, TraceEvent{
				Milestone: traceTerminalReceipt, InstanceID: service.instanceID,
				RunID:             entry.binding.ControlAuthority.SampleAuthority.RunID,
				AttestationSHA256: entry.binding.FixtureBinding.AttestationSHA256,
				RequestID:         entry.requestID, AttemptID: entry.attemptID, Outcome: result.State,
			})
		}
		writeJSON(w, http.StatusOK, result)
	case http.MethodDelete:
		service.deleteAttempt(w, parts[0], authorization)
	default:
		writeProtocolError(w, http.StatusMethodNotAllowed, "method-not-allowed")
	}
}

func (service *Service) rejectInvalidAttemptResult(entry *leasedAttempt) {
	emitTrace(service.trace, TraceEvent{
		Milestone: traceTerminalReceipt, InstanceID: service.instanceID,
		RunID:             entry.binding.ControlAuthority.SampleAuthority.RunID,
		AttestationSHA256: entry.binding.FixtureBinding.AttestationSHA256,
		RequestID:         entry.requestID, AttemptID: entry.attemptID, Outcome: "rejected",
	})
	service.retireRejectedAttempt(entry.attemptID)
}

func validatePendingAttemptResult(result AttemptResult) error {
	if result.State != attemptStatePending || result.FailureCode != nil {
		return errors.New("pending attempt result is invalid")
	}
	if result.SelectedPair != nil {
		return validateSelectedPairEvidence(*result.SelectedPair)
	}
	return nil
}

func (service *Service) signAttemptTerminalResult(
	entry *leasedAttempt,
	result AttemptResult,
	now time.Time,
) (SignedAttemptTerminalReceipt, error) {
	terminalAt := now.UTC().Truncate(time.Millisecond)
	if terminalAt.Before(entry.authorityIssuedAt) || !terminalAt.Before(entry.authorityExpiresAt) {
		return SignedAttemptTerminalReceipt{}, errors.New("attempt terminal time crossed its attestation lease")
	}
	receipt := AttemptTerminalReceipt{
		ProtocolVersion: ProtocolVersion,
		AttemptAuthority: attemptAuthorityFromParts(
			entry.binding, entry.requestID, entry.attemptID, entry.challenge,
		),
		TerminalAt:             terminalAt.Format(canonicalTimestampLayout),
		AttemptLeaseIssuedAt:   entry.leaseIssuedAt.Format(canonicalTimestampLayout),
		AttemptLeaseExpiresAt:  entry.expiresAt.Format(canonicalTimestampLayout),
		AttemptLeaseMillis:     entry.expiresAt.Sub(entry.leaseIssuedAt).Milliseconds(),
		State:                  result.State,
		SelectedPair:           result.SelectedPair,
		ChallengeBindingSHA256: result.ChallengeBindingSHA256,
		FailureCode:            result.FailureCode,
	}
	return service.controlCredentials.signAttemptTerminalReceipt(receipt)
}

func applyAttemptTerminalReceipt(
	result *AttemptResult,
	signed SignedAttemptTerminalReceipt,
) {
	receipt := cloneAttemptTerminalReceipt(signed.Receipt)
	result.State = receipt.State
	result.SelectedPair = receipt.SelectedPair
	result.ChallengeBindingSHA256 = receipt.ChallengeBindingSHA256
	result.FailureCode = receipt.FailureCode
	signed = cloneSignedAttemptTerminalReceipt(signed)
	result.TerminalReceipt = &signed
}

func terminalAttemptResultFromTombstone(
	tombstone attemptTombstone,
) (AttemptResult, bool) {
	if tombstone.terminalReceipt == nil {
		return AttemptResult{}, false
	}
	result := AttemptResult{
		ProtocolVersion: ProtocolVersion,
		AttemptAuthority: attemptAuthorityFromParts(
			tombstone.binding, tombstone.requestID, tombstone.attemptID, tombstone.challenge,
		),
	}
	applyAttemptTerminalReceipt(&result, *tombstone.terminalReceipt)
	return result, true
}

func (service *Service) handleOffer(
	w http.ResponseWriter,
	request *http.Request,
	attemptID string,
	authorization requestAuthorization,
) {
	if request.Method != http.MethodPost || !validOpaqueID(attemptID) {
		writeProtocolError(w, http.StatusMethodNotAllowed, "method-not-allowed")
		return
	}
	entry := service.activeAttempt(attemptID)
	if entry == nil || !authorization.owns(entry.controlLeaseID) {
		writeProtocolError(w, http.StatusNotFound, "attempt-not-found")
		return
	}
	var input OfferRequest
	if service.decodeBody(w, request, &input) != nil {
		service.retireRejectedAttempt(attemptID)
		writeProtocolError(w, http.StatusBadRequest, "invalid-offer")
		return
	}
	attemptAuthority := attemptAuthorityFromParts(
		entry.binding, entry.requestID, entry.attemptID, entry.challenge,
	)
	if validateOfferRequest(input, attemptAuthority) != nil {
		service.retireRejectedAttempt(attemptID)
		writeProtocolError(w, http.StatusBadRequest, "invalid-offer")
		return
	}
	entry.operation.Lock()
	if service.activeAttempt(attemptID) != entry || !service.clock.Now().Before(entry.expiresAt) {
		entry.operation.Unlock()
		service.retireRejectedAttempt(attemptID)
		writeProtocolError(w, http.StatusNotFound, "attempt-not-found")
		return
	}
	deadline := service.clock.Now().Add(service.offerTimeout)
	if entry.expiresAt.Before(deadline) {
		deadline = entry.expiresAt
	}
	ctx, cancel := context.WithDeadline(request.Context(), deadline)
	stopLease := context.AfterFunc(entry.lease, cancel)
	answer, offerErr, closeErr, preclosed := offerWithForcedDeadline(ctx, entry.attempt, input.SDP)
	stopLease()
	cancel()
	if closeErr != nil {
		entry.precloseErr = errors.New("remote Pion forced offer containment failed")
	}
	entry.preclosed = preclosed
	entry.operation.Unlock()
	offerOutcome := "accepted"
	if offerErr != nil {
		offerOutcome = "rejected"
	}
	emitTrace(service.trace, TraceEvent{
		Milestone: traceOfferTerminal, InstanceID: service.instanceID,
		RunID:             entry.binding.ControlAuthority.SampleAuthority.RunID,
		AttestationSHA256: entry.binding.FixtureBinding.AttestationSHA256,
		RequestID:         entry.requestID, AttemptID: entry.attemptID, Outcome: offerOutcome,
	})
	if offerErr != nil {
		service.retireRejectedAttempt(attemptID)
		writeProtocolError(w, http.StatusUnprocessableEntity, "offer-rejected")
		return
	}
	writeJSON(w, http.StatusOK, OfferResponse{
		ProtocolVersion: ProtocolVersion, AttemptAuthority: attemptAuthority,
		Type: "answer", SDP: answer,
	})
}

func (service *Service) deleteAttempt(
	w http.ResponseWriter,
	attemptID string,
	authorization requestAuthorization,
) {
	entry, owner, tombstone := service.transitionOrObserveAuthorized(attemptID, authorization)
	if tombstone != nil {
		service.writeCleanupReceipt(w, *tombstone)
		return
	}
	if entry == nil {
		writeProtocolError(w, http.StatusNotFound, "attempt-not-found")
		return
	}
	if owner {
		service.reap(entry)
	} else {
		<-entry.reaped
	}
	service.writeCleanupReceipt(w, attemptTombstone{
		attemptID: attemptID, requestID: entry.requestID, challenge: entry.challenge,
		controlLeaseID: entry.controlLeaseID,
		terminal:       "reaped", err: entry.retireErr, binding: entry.binding,
	})
}

func (service *Service) writeCleanupReceipt(w http.ResponseWriter, tombstone attemptTombstone) {
	if tombstone.err != nil {
		writeProtocolError(w, http.StatusInternalServerError, "attempt-containment-failed")
		return
	}
	writeJSON(w, http.StatusOK, CleanupReceipt{
		ProtocolVersion: ProtocolVersion,
		AttemptAuthority: attemptAuthorityFromParts(
			tombstone.binding, tombstone.requestID, tombstone.attemptID, tombstone.challenge,
		),
		Terminal: "reaped",
	})
}

func (service *Service) reserveAttempt(
	requestID string,
	lease time.Duration,
	binding AttemptBinding,
	controlLeaseID string,
) (attemptReservation, <-chan struct{}, int) {
	service.mu.Lock()
	service.pruneTombstonesLocked(service.clock.Now())
	if service.closed || len(service.containmentFailures) != 0 ||
		service.controlLeaseRetiringLocked(controlLeaseID) {
		service.mu.Unlock()
		return attemptReservation{}, nil, http.StatusServiceUnavailable
	}
	if _, exists := service.requestOwners[requestID]; exists {
		if service.requestLeases[requestID] == lease &&
			service.requestBindings[requestID] == binding &&
			service.requestControlLeases[requestID] == controlLeaseID {
			startup := service.requestStarts[requestID]
			service.mu.Unlock()
			return attemptReservation{}, startup, 0
		}
		service.mu.Unlock()
		return attemptReservation{}, nil, http.StatusConflict
	}
	if _, exists := service.requestTombstones[requestID]; exists {
		service.mu.Unlock()
		return attemptReservation{}, nil, http.StatusConflict
	}
	if service.occupied >= service.maximumActive ||
		len(service.tombstones)+service.occupied >= service.maximumTombstones {
		service.mu.Unlock()
		return attemptReservation{}, nil, http.StatusTooManyRequests
	}
	service.occupied++
	service.starting++
	// The request claim precedes entropy and factory work so duplicate calls and
	// capacity storms cannot manufacture unowned Pion state.
	service.requestOwners[requestID] = ""
	service.requestLeases[requestID] = lease
	service.requestBindings[requestID] = binding
	service.requestControlLeases[requestID] = controlLeaseID
	service.requestStarts[requestID] = make(chan struct{})
	service.mu.Unlock()

	attemptID, err := service.newAttemptID()
	if err != nil {
		service.releaseUnidentifiedReservation(requestID)
		return attemptReservation{}, nil, http.StatusInternalServerError
	}
	service.mu.Lock()
	if service.closed || len(service.containmentFailures) != 0 ||
		service.controlLeaseRetiringLocked(controlLeaseID) {
		service.releaseUnidentifiedReservationLocked(requestID)
		service.mu.Unlock()
		return attemptReservation{}, nil, http.StatusServiceUnavailable
	}
	_, collidesWithRequest := service.requestOwners[attemptID]
	_, collidesWithRetiredRequest := service.requestTombstones[attemptID]
	if attemptID == requestID || collidesWithRequest || collidesWithRetiredRequest ||
		service.attemptReservations[attemptID] != "" || service.active[attemptID] != nil || service.retiring[attemptID] != nil ||
		service.tombstones[attemptID].attemptID != "" {
		service.releaseUnidentifiedReservationLocked(requestID)
		service.mu.Unlock()
		return attemptReservation{}, nil, http.StatusInternalServerError
	}
	service.requestOwners[requestID] = attemptID
	service.attemptReservations[attemptID] = requestID
	service.mu.Unlock()

	challenge, err := service.newAttemptChallenge()
	if err != nil {
		service.releaseIdentifiedReservation(requestID, attemptID)
		return attemptReservation{}, nil, http.StatusInternalServerError
	}
	service.mu.Lock()
	if service.closed || len(service.containmentFailures) != 0 ||
		service.controlLeaseRetiringLocked(controlLeaseID) {
		service.releaseIdentifiedReservationLocked(requestID, attemptID)
		service.mu.Unlock()
		return attemptReservation{}, nil, http.StatusServiceUnavailable
	}
	service.mu.Unlock()
	emitTrace(service.trace, TraceEvent{
		Milestone: traceAttemptReserved, InstanceID: service.instanceID,
		RunID:             binding.ControlAuthority.SampleAuthority.RunID,
		AttestationSHA256: binding.FixtureBinding.AttestationSHA256,
		RequestID:         requestID, AttemptID: attemptID,
	})
	return attemptReservation{
		attemptID: attemptID, requestID: requestID, challenge: challenge, binding: binding,
		controlLeaseID: controlLeaseID,
	}, nil, 0
}

func (service *Service) registerStartupCancellation(
	reservation attemptReservation,
	cancel context.CancelFunc,
) bool {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.closed || len(service.containmentFailures) != 0 ||
		service.controlLeaseRetiringLocked(reservation.controlLeaseID) ||
		service.requestOwners[reservation.requestID] != reservation.attemptID {
		return false
	}
	service.requestCancels[reservation.requestID] = cancel
	return true
}

func (service *Service) unregisterStartupCancellation(reservation attemptReservation) {
	service.mu.Lock()
	delete(service.requestCancels, reservation.requestID)
	service.mu.Unlock()
}

func (service *Service) releaseUnidentifiedReservation(requestID string) {
	service.mu.Lock()
	service.releaseUnidentifiedReservationLocked(requestID)
	service.mu.Unlock()
}

func (service *Service) releaseUnidentifiedReservationLocked(requestID string) {
	service.starting--
	service.occupied--
	delete(service.requestOwners, requestID)
	delete(service.requestLeases, requestID)
	delete(service.requestBindings, requestID)
	delete(service.requestControlLeases, requestID)
	delete(service.requestCancels, requestID)
	service.signalAndDeleteStartupLocked(requestID)
	service.condition.Broadcast()
}

func (service *Service) releaseIdentifiedReservation(requestID, attemptID string) {
	service.mu.Lock()
	service.releaseIdentifiedReservationLocked(requestID, attemptID)
	service.mu.Unlock()
}

func (service *Service) releaseIdentifiedReservationLocked(requestID, attemptID string) {
	delete(service.attemptReservations, attemptID)
	service.releaseUnidentifiedReservationLocked(requestID)
}

func (service *Service) releaseFailedReservation(
	reservation attemptReservation,
	cleanupErr error,
) {
	service.mu.Lock()
	service.starting--
	service.occupied--
	delete(service.attemptReservations, reservation.attemptID)
	delete(service.requestOwners, reservation.requestID)
	delete(service.requestLeases, reservation.requestID)
	delete(service.requestBindings, reservation.requestID)
	delete(service.requestControlLeases, reservation.requestID)
	delete(service.requestCancels, reservation.requestID)
	service.signalAndDeleteStartupLocked(reservation.requestID)
	var containmentOwners []*leasedAttempt
	if cleanupErr != nil {
		service.containmentFailures = append(
			service.containmentFailures,
			errors.New("remote Pion attempt containment failed"),
		)
		service.cancelLifecycle()
		containmentOwners = service.containAllActiveLocked()
	}
	service.condition.Broadcast()
	service.mu.Unlock()
	if cleanupErr != nil {
		emitTrace(service.trace, TraceEvent{
			Milestone: traceContainmentFailed, InstanceID: service.instanceID,
			RunID:             reservation.binding.ControlAuthority.SampleAuthority.RunID,
			AttestationSHA256: reservation.binding.FixtureBinding.AttestationSHA256,
			RequestID:         reservation.requestID, AttemptID: reservation.attemptID,
			Outcome: "startup-cleanup-failed",
		})
	}
	for _, entry := range containmentOwners {
		go service.reap(entry)
	}
}

func (service *Service) retireRejectedAttempt(attemptID string) {
	entry, owner, _ := service.transitionOrObserve(attemptID)
	if entry == nil {
		return
	}
	if owner {
		service.reap(entry)
	} else {
		<-entry.reaped
	}
}

func (service *Service) expireAttempt(expected *leasedAttempt) {
	service.mu.Lock()
	entry, owner := service.transitionToRetiringLocked(expected.attemptID, expected)
	service.mu.Unlock()
	if owner {
		service.reap(entry)
	}
}

func (service *Service) transitionOrObserve(
	attemptID string,
) (*leasedAttempt, bool, *attemptTombstone) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.pruneTombstonesLocked(service.clock.Now())
	if tombstone, found := service.tombstones[attemptID]; found {
		return nil, false, &tombstone
	}
	if entry := service.retiring[attemptID]; entry != nil {
		return entry, false, nil
	}
	entry, owner := service.transitionToRetiringLocked(attemptID, nil)
	return entry, owner, nil
}

func (service *Service) transitionOrObserveAuthorized(
	attemptID string,
	authorization requestAuthorization,
) (*leasedAttempt, bool, *attemptTombstone) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.pruneTombstonesLocked(service.clock.Now())
	if tombstone, found := service.tombstones[attemptID]; found {
		if !authorization.owns(tombstone.controlLeaseID) {
			return nil, false, nil
		}
		return nil, false, &tombstone
	}
	if entry := service.retiring[attemptID]; entry != nil {
		if !authorization.owns(entry.controlLeaseID) {
			return nil, false, nil
		}
		return entry, false, nil
	}
	entry := service.active[attemptID]
	if entry == nil || !authorization.owns(entry.controlLeaseID) {
		return nil, false, nil
	}
	transitioned, owner := service.transitionToRetiringLocked(attemptID, entry)
	return transitioned, owner, nil
}

func (service *Service) transitionToRetiringLocked(
	attemptID string,
	expected *leasedAttempt,
) (*leasedAttempt, bool) {
	entry := service.active[attemptID]
	if entry == nil || expected != nil && entry != expected {
		if retiring := service.retiring[attemptID]; retiring != nil {
			return retiring, false
		}
		return nil, false
	}
	delete(service.active, attemptID)
	entry.state = attemptRetiring
	entry.leaseCancel()
	if entry.timer != nil {
		entry.timer.Stop()
	}
	service.retiring[attemptID] = entry
	return entry, true
}

func (service *Service) reap(entry *leasedAttempt) {
	emitTrace(service.trace, TraceEvent{
		Milestone: traceAttemptRetiring, InstanceID: service.instanceID,
		RunID:             entry.binding.ControlAuthority.SampleAuthority.RunID,
		AttestationSHA256: entry.binding.FixtureBinding.AttestationSHA256,
		RequestID:         entry.requestID, AttemptID: entry.attemptID,
	})
	entry.operation.Lock()
	var closeErr error
	if !entry.preclosed {
		closeErr = entry.attempt.Close()
	}
	var terminalReceipt *SignedAttemptTerminalReceipt
	if entry.terminalReceipt != nil {
		cloned := cloneSignedAttemptTerminalReceipt(*entry.terminalReceipt)
		terminalReceipt = &cloned
	}
	entry.operation.Unlock()
	entry.retireErr = errors.Join(entry.precloseErr, closeErr)

	service.mu.Lock()
	if service.retiring[entry.attemptID] != entry {
		service.mu.Unlock()
		panic("remote Pion retirement lost its exact registry owner")
	}
	delete(service.retiring, entry.attemptID)
	entry.state = attemptReaped
	service.occupied--
	delete(service.attemptReservations, entry.attemptID)
	delete(service.requestOwners, entry.requestID)
	delete(service.requestLeases, entry.requestID)
	delete(service.requestBindings, entry.requestID)
	delete(service.requestControlLeases, entry.requestID)
	delete(service.requestCancels, entry.requestID)
	service.signalAndDeleteStartupLocked(entry.requestID)
	now := service.clock.Now().UTC()
	tombstone := attemptTombstone{
		attemptID: entry.attemptID, requestID: entry.requestID, challenge: entry.challenge,
		controlLeaseID: entry.controlLeaseID,
		expiresAt:      service.attemptTombstoneExpiresAt(entry, now), terminal: "reaped",
		err: entry.retireErr, binding: entry.binding, terminalReceipt: terminalReceipt,
	}
	service.addTombstoneLocked(tombstone)
	var containmentOwners []*leasedAttempt
	if entry.retireErr != nil {
		service.containmentFailures = append(
			service.containmentFailures,
			errors.New("remote Pion attempt containment failed"),
		)
		service.cancelLifecycle()
		containmentOwners = service.containAllActiveLocked()
	}
	close(entry.reaped)
	service.condition.Broadcast()
	service.mu.Unlock()
	outcome := "clean"
	if entry.retireErr != nil {
		outcome = "containment-failed"
	}
	emitTrace(service.trace, TraceEvent{
		Milestone: traceAttemptReaped, InstanceID: service.instanceID,
		RunID:             entry.binding.ControlAuthority.SampleAuthority.RunID,
		AttestationSHA256: entry.binding.FixtureBinding.AttestationSHA256,
		RequestID:         entry.requestID, AttemptID: entry.attemptID, Outcome: outcome,
	})
	if entry.retireErr != nil {
		emitTrace(service.trace, TraceEvent{
			Milestone: traceContainmentFailed, InstanceID: service.instanceID,
			RunID:             entry.binding.ControlAuthority.SampleAuthority.RunID,
			AttestationSHA256: entry.binding.FixtureBinding.AttestationSHA256,
			RequestID:         entry.requestID, AttemptID: entry.attemptID,
			Outcome: "retirement-failed",
		})
	}
	for _, owner := range containmentOwners {
		go service.reap(owner)
	}
}

func (service *Service) containAllActiveLocked() []*leasedAttempt {
	owners := make([]*leasedAttempt, 0, len(service.active))
	for attemptID, entry := range service.active {
		if transitioned, owner := service.transitionToRetiringLocked(attemptID, entry); owner {
			owners = append(owners, transitioned)
		}
	}
	return owners
}

func (service *Service) addTombstoneLocked(tombstone attemptTombstone) {
	service.tombstones[tombstone.attemptID] = tombstone
	service.requestTombstones[tombstone.requestID] = tombstone.attemptID
	service.tombstoneOrder = append(service.tombstoneOrder, tombstone.attemptID)
}

func (service *Service) attemptTombstoneExpiresAt(
	entry *leasedAttempt,
	now time.Time,
) time.Time {
	expiresAt := now.Add(service.tombstoneRetention)
	for _, governingExpiry := range []time.Time{
		entry.expiresAt,
		entry.authorityExpiresAt,
		entry.controlExpiresAt,
	} {
		if governingExpiry.After(expiresAt) {
			expiresAt = governingExpiry
		}
	}
	return expiresAt
}

func (service *Service) pruneTombstonesLocked(now time.Time) {
	kept := service.tombstoneOrder[:0]
	for _, attemptID := range service.tombstoneOrder {
		tombstone, found := service.tombstones[attemptID]
		if !found {
			continue
		}
		if !now.Before(tombstone.expiresAt) {
			delete(service.tombstones, attemptID)
			delete(service.requestTombstones, tombstone.requestID)
			continue
		}
		kept = append(kept, attemptID)
	}
	service.tombstoneOrder = kept
}

func (service *Service) activeAttempt(attemptID string) *leasedAttempt {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.active[attemptID]
}

func (service *Service) attemptForRead(
	attemptID string,
	authorization requestAuthorization,
) (*leasedAttempt, *attemptTombstone) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.pruneTombstonesLocked(service.clock.Now().UTC())
	if tombstone, exists := service.tombstones[attemptID]; exists {
		if !authorization.owns(tombstone.controlLeaseID) {
			return nil, nil
		}
		return nil, &tombstone
	}
	entry := service.active[attemptID]
	if entry == nil || !authorization.owns(entry.controlLeaseID) {
		return nil, nil
	}
	return entry, nil
}

func (service *Service) signalStartupLocked(requestID string) {
	startup, exists := service.requestStarts[requestID]
	if exists && startup != nil {
		close(startup)
		service.requestStarts[requestID] = nil
	}
}

func (service *Service) signalAndDeleteStartupLocked(requestID string) {
	service.signalStartupLocked(requestID)
	delete(service.requestStarts, requestID)
}

func (service *Service) newAttemptID() (string, error) {
	service.entropyMu.Lock()
	defer service.entropyMu.Unlock()
	identifier := make([]byte, attemptIdentifierBytes)
	if _, err := io.ReadFull(service.attemptIDSource, identifier); err != nil {
		return "", errors.New("remote Pion attempt ID source failed")
	}
	value := base64.RawURLEncoding.EncodeToString(identifier)
	if !validOpaqueID(value) {
		return "", errors.New("remote Pion attempt ID source is invalid")
	}
	return value, nil
}

func (service *Service) newAttemptChallenge() (string, error) {
	service.entropyMu.Lock()
	defer service.entropyMu.Unlock()
	challenge := make([]byte, attemptChallengeBytes)
	if _, err := io.ReadFull(service.challengeSource, challenge); err != nil {
		return "", errors.New("remote Pion challenge source failed")
	}
	challengeValue := base64.RawURLEncoding.EncodeToString(challenge)
	if !validOpaqueID(challengeValue) {
		return "", errors.New("remote Pion challenge source is invalid")
	}
	return challengeValue, nil
}

func (service *Service) decodeBody(w http.ResponseWriter, request *http.Request, destination any) error {
	controller := http.NewResponseController(w)
	deadline := time.Now().Add(service.bodyReadTimeout)
	if err := controller.SetReadDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return errors.New("request body deadline could not be established")
	}
	defer func() { _ = controller.SetReadDeadline(time.Time{}) }()
	return decodeRequest(w, request, destination)
}

func (service *Service) authorizeRequest(
	request *http.Request,
) (requestAuthorization, bool) {
	provided, ok := bearerCredential(request)
	if !ok {
		return requestAuthorization{}, false
	}
	controlLeaseID := request.Header.Get(ControlLeaseIDHeader)
	service.mu.Lock()
	staticAuthorized := len(provided) == len(service.credential) &&
		subtle.ConstantTimeCompare([]byte(provided), service.credential) == 1
	service.mu.Unlock()
	if staticAuthorized {
		return requestAuthorization{}, controlLeaseID == ""
	}
	finish, authorized := service.controlCredentials.beginRequest(controlLeaseID, provided)
	if !authorized {
		return requestAuthorization{}, false
	}
	return requestAuthorization{
		controlLeaseID: controlLeaseID,
		finish:         finish,
		dynamic:        true,
	}, true
}

func bearerCredential(request *http.Request) (string, bool) {
	provided, ok := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
	return provided, ok && provided != ""
}

func (service *Service) unavailable() bool {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.closed || len(service.containmentFailures) != 0
}

type offerResult struct {
	answer string
	err    error
}

func offerWithForcedDeadline(
	ctx context.Context,
	attempt Attempt,
	sdp string,
) (string, error, error, bool) {
	result := make(chan offerResult, 1)
	go func() {
		answer, err := attempt.Offer(ctx, sdp)
		result <- offerResult{answer: answer, err: err}
	}()
	select {
	case completed := <-result:
		return completed.answer, completed.err, nil, false
	case <-ctx.Done():
		closeErr := attempt.Close()
		<-result
		return "", errors.New("remote Pion offer deadline exceeded"), closeErr, true
	}
}

type realLeaseClock struct{}

func (realLeaseClock) Now() time.Time { return time.Now() }

func (realLeaseClock) AfterFunc(delay time.Duration, callback func()) LeaseTimer {
	return time.AfterFunc(delay, callback)
}

func (service *Service) String() string {
	return fmt.Sprintf("remote-pion-service(instance=%s)", service.instanceID)
}

func cloneExternalFixture(fixture ExternalFixture) ExternalFixture {
	cloned := fixture
	cloned.NetworkSemantics.TURNURLs = append([]string(nil), fixture.NetworkSemantics.TURNURLs...)
	return cloned
}
