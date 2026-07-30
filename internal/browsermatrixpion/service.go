package browsermatrixpion

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
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
