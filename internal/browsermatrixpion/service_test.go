package browsermatrixpion

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/testnetwork"
)

const testCredential = "control_credential_AAAAAAAAAAAAAAAAAAAAAAAA"

type fakeAttempt struct {
	mu                 sync.Mutex
	answer             string
	offerErr           error
	closeErr           error
	offers             []string
	closeCount         int
	result             AttemptResult
	offerStarted       chan struct{}
	offerStartedOnce   sync.Once
	offerRelease       chan struct{}
	offerReleaseOnce   sync.Once
	offerHonorsContext bool
	closeReleasesOffer bool
	closeStarted       chan struct{}
	closeStartedOnce   sync.Once
	closeRelease       <-chan struct{}
}

func (attempt *fakeAttempt) Offer(ctx context.Context, offer string) (string, error) {
	attempt.mu.Lock()
	attempt.offers = append(attempt.offers, offer)
	answer, offerErr := attempt.answer, attempt.offerErr
	started, release, honorsContext := attempt.offerStarted, attempt.offerRelease, attempt.offerHonorsContext
	attempt.mu.Unlock()
	if started != nil {
		attempt.offerStartedOnce.Do(func() { close(started) })
	}
	if release != nil {
		if honorsContext {
			select {
			case <-release:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		} else {
			<-release
		}
	}
	return answer, offerErr
}

func (attempt *fakeAttempt) Result() AttemptResult { return attempt.result }

func (attempt *fakeAttempt) Close() error {
	attempt.mu.Lock()
	attempt.closeCount++
	closeErr, started, release := attempt.closeErr, attempt.closeStarted, attempt.closeRelease
	closeReleasesOffer, offerRelease := attempt.closeReleasesOffer, attempt.offerRelease
	attempt.mu.Unlock()
	if started != nil {
		attempt.closeStartedOnce.Do(func() { close(started) })
	}
	if closeReleasesOffer && offerRelease != nil {
		attempt.offerReleaseOnce.Do(func() { close(offerRelease) })
	}
	if release != nil {
		<-release
	}
	return closeErr
}

func (attempt *fakeAttempt) closeCountValue() int {
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	return attempt.closeCount
}

type fakeAttemptFactory struct {
	mu         sync.Mutex
	createErr  error
	created    []*fakeAttempt
	challenges []string
}

func (factory *fakeAttemptFactory) Create(_ context.Context, authority AttemptAuthority) (Attempt, error) {
	if factory.createErr != nil {
		return nil, factory.createErr
	}
	attempt := &fakeAttempt{
		answer: "v=0\r\nanswer", result: AttemptResult{
			ProtocolVersion: ProtocolVersion, AttemptAuthority: authority,
			State: attemptStatePending,
		},
	}
	factory.mu.Lock()
	factory.created = append(factory.created, attempt)
	factory.challenges = append(factory.challenges, authority.Challenge)
	factory.mu.Unlock()
	return attempt, nil
}

type gatedAttemptFactory struct {
	mu           sync.Mutex
	started      chan string
	release      <-chan struct{}
	honorContext bool
	createErr    error
	configure    func(*fakeAttempt)
	created      []*fakeAttempt
	challenges   []string
}

type gatedChallengeReader struct {
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
	mu      sync.Mutex
	data    []byte
	offset  int
}

func (reader *gatedChallengeReader) Read(destination []byte) (int, error) {
	reader.once.Do(func() { close(reader.started) })
	<-reader.release
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.offset == len(reader.data) {
		return 0, io.EOF
	}
	copied := copy(destination, reader.data[reader.offset:])
	reader.offset += copied
	return copied, nil
}

func (factory *gatedAttemptFactory) Create(
	ctx context.Context,
	authority AttemptAuthority,
) (Attempt, error) {
	attempt := &fakeAttempt{
		answer: "v=0\r\nanswer",
		result: AttemptResult{
			ProtocolVersion: ProtocolVersion, AttemptAuthority: authority,
			State: attemptStatePending,
		},
	}
	if factory.configure != nil {
		factory.configure(attempt)
	}
	factory.mu.Lock()
	factory.created = append(factory.created, attempt)
	factory.challenges = append(factory.challenges, authority.Challenge)
	factory.mu.Unlock()
	if factory.started != nil {
		factory.started <- authority.AttemptID
	}
	if factory.release != nil {
		if factory.honorContext {
			select {
			case <-factory.release:
			case <-ctx.Done():
				return attempt, ctx.Err()
			}
		} else {
			<-factory.release
		}
	}
	return attempt, factory.createErr
}

func (factory *gatedAttemptFactory) createdAttempts() []*fakeAttempt {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	return append([]*fakeAttempt(nil), factory.created...)
}

type fakeProber struct {
	err   error
	uri   string
	calls int
}

type failingResponseWriter struct {
	header http.Header
	status int
	writes int
}

func (writer *failingResponseWriter) Header() http.Header {
	if writer.header == nil {
		writer.header = make(http.Header)
	}
	return writer.header
}

func (writer *failingResponseWriter) WriteHeader(status int) { writer.status = status }

func (writer *failingResponseWriter) Write([]byte) (int, error) {
	writer.writes++
	return 0, errors.New("simulated connection loss")
}

func (prober *fakeProber) Probe(_ context.Context, uri string) error {
	prober.calls++
	prober.uri = uri
	return prober.err
}

type fakeLeaseClock struct {
	mu              sync.Mutex
	now             time.Time
	timers          []*fakeLeaseTimer
	nilTimer        bool
	fireImmediately bool
}

func (clock *fakeLeaseClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeLeaseClock) AfterFunc(_ time.Duration, callback func()) LeaseTimer {
	clock.mu.Lock()
	if clock.nilTimer {
		clock.mu.Unlock()
		return nil
	}
	timer := &fakeLeaseTimer{callback: callback}
	clock.timers = append(clock.timers, timer)
	fireImmediately := clock.fireImmediately
	clock.mu.Unlock()
	if fireImmediately {
		timer.mu.Lock()
		timer.fired = true
		timer.mu.Unlock()
		callback()
	}
	return timer
}

type fakeLeaseTimer struct {
	mu       sync.Mutex
	callback func()
	stopped  bool
	fired    bool
}

func (timer *fakeLeaseTimer) Stop() bool {
	timer.mu.Lock()
	defer timer.mu.Unlock()
	wasActive := !timer.stopped && !timer.fired
	timer.stopped = true
	return wasActive
}

func (timer *fakeLeaseTimer) Fire() {
	timer.mu.Lock()
	if timer.stopped || timer.fired {
		timer.mu.Unlock()
		return
	}
	timer.fired = true
	callback := timer.callback
	timer.mu.Unlock()
	callback()
}

type serviceHarness struct {
	service *Service
	factory *fakeAttemptFactory
	prober  *fakeProber
	clock   *fakeLeaseClock
}

func newServiceHarness(t *testing.T) serviceHarness {
	t.Helper()
	factory := &fakeAttemptFactory{}
	prober := &fakeProber{}
	clock := &fakeLeaseClock{now: time.Now().UTC()}
	service, err := NewService(completeServiceConfig(factory, prober, clock))
	if err != nil {
		t.Fatal(err)
	}
	return serviceHarness{service, factory, prober, clock}
}

func completeServiceConfig(
	factory AttemptFactory,
	prober STUNProber,
	clock LeaseClock,
) ServiceConfig {
	return ServiceConfig{
		Fixture:           testExternalFixture("scheduled-public-stun"),
		AttestationSigner: testAttestationPrivateKey(),
		MaximumLease:      time.Minute, AttemptStartTimeout: time.Second, OfferTimeout: time.Second,
		ProbeTimeout: time.Second, BodyReadTimeout: time.Second, TombstoneRetention: time.Minute,
		MaximumActive: 4, MaximumTombstones: 16,
		Credential: []byte(testCredential), AttemptFactory: factory,
		STUNProber: prober, Clock: clock,
		AttemptIDSource: attemptIDStream(128), ChallengeSource: challengeStream(128),
		AttestationLeaseIDSource: attemptIDStream(128),
		ControlLeaseIDSource:     controlLeaseIDStream(128), ControlCredentialSource: challengeStream(128),
	}
}

func attemptIDStream(count int) io.Reader {
	identifiers := make([]byte, 0, count*attemptIdentifierBytes)
	for identifier := 1; identifier <= count; identifier++ {
		identifiers = append(identifiers, bytes.Repeat([]byte{byte(identifier)}, attemptIdentifierBytes)...)
	}
	return bytes.NewReader(identifiers)
}

func controlLeaseIDStream(count int) io.Reader {
	identifiers := make([]byte, 0, count*controlLeaseIdentifierBytes)
	for identifier := 1; identifier <= count; identifier++ {
		value := bytes.Repeat([]byte{byte(identifier)}, controlLeaseIdentifierBytes)
		value[0] = 0xfe
		identifiers = append(identifiers, value...)
	}
	return bytes.NewReader(identifiers)
}

func challengeStream(count int) io.Reader {
	challenges := make([]byte, 0, count*attemptChallengeBytes)
	for challenge := 1; challenge <= count; challenge++ {
		challenges = append(challenges, bytes.Repeat([]byte{byte(challenge)}, attemptChallengeBytes)...)
	}
	return bytes.NewReader(challenges)
}

func serveRequest(t *testing.T, service *Service, method, path string, body any, credential string) *httptest.ResponseRecorder {
	t.Helper()
	return serveRequestWithControlLease(t, service, method, path, body, credential, "")
}

func serveControlLeaseRequest(
	t *testing.T,
	service *Service,
	method string,
	path string,
	body any,
	lease ControlCredentialLease,
) *httptest.ResponseRecorder {
	t.Helper()
	return serveRequestWithControlLease(
		t, service, method, path, body, string(lease.Credential), lease.LeaseID,
	)
}

func serveRequestWithControlLease(
	t *testing.T,
	service *Service,
	method string,
	path string,
	body any,
	credential string,
	controlLeaseID string,
) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, reader)
	origin, err := url.Parse(service.fixture.ControllerOrigin)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = origin.Host
	request.TLS = &tls.ConnectionState{}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if credential != "" {
		request.Header.Set("Authorization", "Bearer "+credential)
	}
	if controlLeaseID != "" {
		request.Header.Set(ControlLeaseIDHeader, controlLeaseID)
	}
	response := httptest.NewRecorder()
	service.ServeHTTP(response, request)
	return response
}

var testAuthorityBindingMutex sync.Mutex
var testAuthorityBindingSequence uint64

func acquireTestControlLease(
	t *testing.T,
	service *Service,
	request *AuthorityProbeRequest,
) ControlCredentialLease {
	t.Helper()
	sampleAuthority := request.ControlAuthority.SampleAuthority
	lease, err := service.AcquireControlCredential(ControlCredentialAcquireRequest{
		RequestID: "acquire-" + request.Nonce,
		RunID:     sampleAuthority.RunID, ProfileID: sampleAuthority.ProfileID, ProbeNonce: request.Nonce,
		RequestedLeaseMillis: request.RequestedLeaseMillis,
	})
	if err != nil {
		t.Fatal(err)
	}
	request.ControlAuthority.ControlLeaseID = lease.LeaseID
	return lease
}

func newTestAuthorityProbeRequest(
	t *testing.T,
	service *Service,
	runID string,
	nonce string,
	requestedLeaseMillis int64,
) AuthorityProbeRequest {
	t.Helper()
	sampleAuthority, err := NewSampleAuthority(
		runID,
		service.profileID,
		"chromium",
		1,
		"process-"+runID,
	)
	if err != nil {
		t.Fatal(err)
	}
	return AuthorityProbeRequest{
		ProtocolVersion: ProtocolVersion,
		ControlAuthority: ControlAuthority{
			SchemaVersion:   ControlAuthoritySchemaVersion,
			SampleAuthority: sampleAuthority,
		},
		Nonce:                nonce,
		RequestedLeaseMillis: requestedLeaseMillis,
	}
}

func ensureTestAuthorityBinding(t *testing.T, service *Service) AttemptBinding {
	t.Helper()
	testAuthorityBindingMutex.Lock()
	defer testAuthorityBindingMutex.Unlock()
	testAuthorityBindingSequence++
	request := newTestAuthorityProbeRequest(
		t,
		service,
		fmt.Sprintf("service-test-run-%08d", testAuthorityBindingSequence),
		fmt.Sprintf("service-test-nonce-%016d", testAuthorityBindingSequence),
		60_000,
	)
	controlLease := acquireTestControlLease(t, service, &request)
	response := serveControlLeaseRequest(
		t, service, http.MethodPost, authorityProbePath, request, controlLease,
	)
	eraseCredentialBytes(controlLease.Credential)
	if response.Code != http.StatusOK {
		t.Fatalf("test authority lease issuance status=%d body=%s", response.Code, response.Body.String())
	}
	var signed AuthorityProbeResponse
	if err := json.Unmarshal(response.Body.Bytes(), &signed); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	lease := service.authorityLeases[signed.AttestationSHA256]
	service.mu.Unlock()
	if lease.binding == (AttemptBinding{}) {
		t.Fatal("test authority lease did not publish its binding")
	}
	return lease.binding
}

func newTestCreateAttemptRequest(
	t *testing.T,
	service *Service,
	requestID string,
	leaseMillis int64,
) CreateAttemptRequest {
	t.Helper()
	return testCreateAttemptRequest(
		ensureTestAuthorityBinding(t, service),
		requestID,
		leaseMillis,
	)
}

func createAttempt(t *testing.T, harness serviceHarness, requestID string) CreateAttemptResponse {
	t.Helper()
	request := newTestCreateAttemptRequest(t, harness.service, requestID, 5_000)
	return createAttemptFromRequest(t, harness, request)
}

func createAttemptFromRequest(
	t *testing.T,
	harness serviceHarness,
	request CreateAttemptRequest,
) CreateAttemptResponse {
	t.Helper()
	response := serveRequest(
		t, harness.service, http.MethodPost, attemptsPath, request, testCredential,
	)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	var result CreateAttemptResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	requestID := request.RequestAuthority.RequestID
	if result.AttemptAuthority.AttemptID == requestID ||
		!validOpaqueID(result.AttemptAuthority.AttemptID) ||
		result.AttemptAuthority.RequestAuthority != request.RequestAuthority {
		t.Fatalf("server attempt authority was not bound to request %q: %#v", requestID, result.AttemptAuthority)
	}
	if created := harness.factory.created[len(harness.factory.created)-1]; created.result.AttemptAuthority != result.AttemptAuthority {
		t.Fatalf("factory authority differs from response: factory=%#v response=%#v",
			created.result.AttemptAuthority, result.AttemptAuthority)
	}
	return result
}

func TestNewServiceRejectsIncompleteAuthority(t *testing.T) {
	base := completeServiceConfig(&fakeAttemptFactory{}, &fakeProber{}, &fakeLeaseClock{now: time.Now().UTC()})
	base.MaximumActive = 2
	base.MaximumTombstones = 4
	tests := map[string]func(*ServiceConfig){
		"instance":        func(config *ServiceConfig) { config.Fixture.RemoteServiceInstanceID = "INVALID" },
		"profile":         func(config *ServiceConfig) { config.Fixture.ProfileID = "unknown" },
		"implementation":  func(config *ServiceConfig) { config.Fixture.ImplementationSHA256 = "invalid" },
		"endpoint":        func(config *ServiceConfig) { config.Fixture.RemotePeerPublicIP = "" },
		"signer":          func(config *ServiceConfig) { config.AttestationSigner = nil },
		"lease":           func(config *ServiceConfig) { config.MaximumLease = 0 },
		"start":           func(config *ServiceConfig) { config.AttemptStartTimeout = 0 },
		"offer":           func(config *ServiceConfig) { config.OfferTimeout = 0 },
		"probe":           func(config *ServiceConfig) { config.ProbeTimeout = 0 },
		"body":            func(config *ServiceConfig) { config.BodyReadTimeout = 0 },
		"tombstone":       func(config *ServiceConfig) { config.TombstoneRetention = 0 },
		"active":          func(config *ServiceConfig) { config.MaximumActive = 12 },
		"lease-limit":     func(config *ServiceConfig) { config.MaximumLease = maximumLeaseLimit + time.Nanosecond },
		"operation-limit": func(config *ServiceConfig) { config.OfferTimeout = maximumOperationTimeout + time.Nanosecond },
		"retention-limit": func(config *ServiceConfig) { config.TombstoneRetention = maximumTombstoneLife + time.Nanosecond },
		"tombstone-limit": func(config *ServiceConfig) { config.MaximumTombstones = maximumTombstoneCapacity + 1 },
		"credential":      func(config *ServiceConfig) { config.Credential = []byte("short") },
		"factory":         func(config *ServiceConfig) { config.AttemptFactory = nil },
		"prober":          func(config *ServiceConfig) { config.STUNProber = nil },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			config := base
			mutate(&config)
			if _, err := NewService(config); err == nil {
				t.Fatal("invalid authority accepted")
			}
		})
	}
}

func TestAuthorityProbeBindsLiveFixtureIdentity(t *testing.T) {
	harness := newServiceHarness(t)
	nonce := strings.Repeat("a", 48)
	request := newTestAuthorityProbeRequest(
		t, harness.service, "authority-probe-run", nonce, 30_000,
	)
	controlLease := acquireTestControlLease(t, harness.service, &request)
	response := serveControlLeaseRequest(
		t, harness.service, http.MethodPost, authorityProbePath, request, controlLease,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("authority probe status=%d body=%s", response.Code, response.Body.String())
	}
	var proof AuthorityProbeResponse
	if err := json.Unmarshal(response.Body.Bytes(), &proof); err != nil {
		t.Fatal(err)
	}
	verified, err := VerifyAuthorityProbeResponse(
		proof, testAttestationPrivateKey().Public().(ed25519.PublicKey), harness.clock.Now().Add(time.Second),
		request.ControlAuthority.SampleAuthority.RunID, request.Nonce,
	)
	if err != nil || verified.Attestation.LeaseMillis != request.RequestedLeaseMillis ||
		verified.Attestation.Fixture.AuthorityInstanceID != harness.service.fixture.AuthorityInstanceID ||
		controlLease.ProbeNonce != request.Nonce ||
		controlLease.AttestationSHA256 != proof.AttestationSHA256 {
		t.Fatalf("authority proof is not exact: leaseDigest=%q proofDigest=%q err=%v", controlLease.AttestationSHA256, proof.AttestationSHA256, err)
	}
	replayed := serveControlLeaseRequest(
		t, harness.service, http.MethodPost, authorityProbePath, request, controlLease,
	)
	if replayed.Code != http.StatusConflict {
		t.Fatalf("one-shot probe credential replay status=%d", replayed.Code)
	}
	request.RequestedLeaseMillis++
	conflict := serveControlLeaseRequest(
		t, harness.service, http.MethodPost, authorityProbePath, request, controlLease,
	)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("consumed claim mismatch status=%d body=%s", conflict.Code, conflict.Body.String())
	}
	eraseCredentialBytes(controlLease.Credential)
}

func TestServiceAuthorizationAndErrorsNeverReflectCredential(t *testing.T) {
	harness := newServiceHarness(t)
	defer harness.service.Close() //nolint:errcheck
	for _, credential := range []string{"", "wrong", testCredential + "x"} {
		response := serveRequest(t, harness.service, http.MethodPost, probePath, ProbeRequest{}, credential)
		if response.Code != http.StatusUnauthorized || strings.Contains(response.Body.String(), credential) && credential != "" {
			t.Fatalf("authorization response leaked or accepted credential: %d %q", response.Code, response.Body.String())
		}
	}
	if text := harness.service.String(); strings.Contains(text, testCredential) || !strings.Contains(text, "remote-a") {
		t.Fatalf("unsafe service representation: %q", text)
	}
}

func TestExactHTTPGateRejectsBeforeOneShotControlDispatch(t *testing.T) {
	mutations := []func(*http.Request){
		func(request *http.Request) { request.TLS = nil },
		func(request *http.Request) { request.Host = "hostile.example" },
		func(request *http.Request) {
			request.URL.RawPath = "/v2/authority%2Dprobe"
			request.RequestURI = request.URL.RawPath
		},
		func(request *http.Request) { request.URL.ForceQuery = true; request.RequestURI += "?" },
		func(request *http.Request) { request.URL.Scheme = "https"; request.URL.Host = request.Host },
		func(request *http.Request) { request.Header.Add("Content-Type", "application/json") },
		func(request *http.Request) { request.Header.Set("Content-Encoding", "gzip") },
		func(request *http.Request) { request.TransferEncoding = []string{"chunked"} },
	}
	for index, mutate := range mutations {
		harness := newServiceHarness(t)
		authorityRequest := newTestAuthorityProbeRequest(
			t,
			harness.service,
			"exact-gate-run",
			fmt.Sprintf("exact-gate-probe-%016d", index),
			30_000,
		)
		controlLease := acquireTestControlLease(t, harness.service, &authorityRequest)
		encoded, err := json.Marshal(authorityRequest)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, authorityProbePath, bytes.NewReader(encoded))
		origin, err := url.Parse(harness.service.fixture.ControllerOrigin)
		if err != nil {
			t.Fatal(err)
		}
		request.Host = origin.Host
		request.TLS = &tls.ConnectionState{}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+string(controlLease.Credential))
		request.Header.Set(ControlLeaseIDHeader, controlLease.LeaseID)
		mutate(request)
		response := httptest.NewRecorder()
		harness.service.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("hostile gate mutation %d status=%d", index, response.Code)
		}
		valid := serveControlLeaseRequest(
			t, harness.service, http.MethodPost, authorityProbePath, authorityRequest, controlLease,
		)
		if valid.Code != http.StatusOK {
			t.Fatalf("hostile gate mutation %d consumed one-shot authority: status=%d", index, valid.Code)
		}
		eraseCredentialBytes(controlLease.Credential)
		if err := harness.service.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestServiceProbeRequiresRealProberSuccess(t *testing.T) {
	harness := newServiceHarness(t)
	defer harness.service.Close() //nolint:errcheck
	request := ProbeRequest{
		ProtocolVersion: ProtocolVersion, AttemptBinding: ensureTestAuthorityBinding(t, harness.service),
		STUNURI: harness.service.stunEndpoint,
	}
	harness.service.mu.Lock()
	request.Nonce = harness.service.authorityLeases[request.FixtureBinding.AttestationSHA256].response.Attestation.Nonce
	harness.service.mu.Unlock()
	response := serveRequest(t, harness.service, http.MethodPost, probePath, request, testCredential)
	if response.Code != http.StatusOK || harness.prober.calls != 1 || harness.prober.uri != request.STUNURI {
		t.Fatalf("probe was not delegated: status=%d calls=%d uri=%q", response.Code, harness.prober.calls, harness.prober.uri)
	}
	var result ProbeResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || !result.ServerReflexiveObserved ||
		result.Nonce != request.Nonce || result.AttemptBinding != request.AttemptBinding {
		t.Fatalf("probe response is not bound: %#v err=%v", result, err)
	}
	harness.prober.err = errors.New("offline")
	if failed := serveRequest(t, harness.service, http.MethodPost, probePath, request, testCredential); failed.Code != http.StatusBadGateway {
		t.Fatalf("failed STUN probe status=%d", failed.Code)
	}
	if malformed := serveRequest(t, harness.service, http.MethodPost, probePath, map[string]string{"unexpected": "x"}, testCredential); malformed.Code != http.StatusBadRequest {
		t.Fatalf("malformed probe status=%d", malformed.Code)
	}
	if wrongMethod := serveRequest(t, harness.service, http.MethodGet, probePath, nil, testCredential); wrongMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("wrong probe method status=%d", wrongMethod.Code)
	}
}

func TestAttemptAdmissionAndEvidenceRejectCrossAttestationSplicing(t *testing.T) {
	harness := newServiceHarness(t)
	defer harness.service.Close() //nolint:errcheck
	binding := ensureTestAuthorityBinding(t, harness.service)
	hostile := binding
	hostile.FixtureBinding.NetworkBindingSHA256 = strings.Repeat("f", 64)
	rejected := serveRequest(
		t,
		harness.service,
		http.MethodPost,
		attemptsPath,
		testCreateAttemptRequest(hostile, matrixRequestID(70), 1_000),
		testCredential,
	)
	if rejected.Code != http.StatusConflict || len(harness.factory.created) != 0 {
		t.Fatalf("spliced attestation admitted: status=%d attempts=%d", rejected.Code, len(harness.factory.created))
	}
	created := createAttemptFromRequest(
		t,
		harness,
		testCreateAttemptRequest(binding, matrixRequestID(71), 5_000),
	)
	createdBinding := attemptBindingFromAuthority(created.AttemptAuthority)
	if createdBinding != binding {
		t.Fatalf("create response lost its exact authority binding: %#v", createdBinding)
	}
	resultResponse := serveRequest(
		t, harness.service, http.MethodGet,
		attemptsPath+"/"+created.AttemptAuthority.AttemptID, nil, testCredential,
	)
	var result AttemptResult
	if resultResponse.Code != http.StatusOK || json.Unmarshal(resultResponse.Body.Bytes(), &result) != nil ||
		result.AttemptAuthority != created.AttemptAuthority ||
		result.ChallengeBindingSHA256 != challengeBindingSHA256(created.AttemptAuthority) {
		t.Fatalf("attempt result is not bound to the creating attestation: %s", resultResponse.Body.String())
	}
	hostileAuthority := created.AttemptAuthority
	hostileAuthority.RequestAuthority.FixtureBinding = hostile.FixtureBinding
	badOffer := serveRequest(t, harness.service, http.MethodPost,
		attemptsPath+"/"+created.AttemptAuthority.AttemptID+"/offer", OfferRequest{
			ProtocolVersion: ProtocolVersion, AttemptAuthority: hostileAuthority,
			Type: "offer", SDP: "v=0\r\n",
		}, testCredential)
	if badOffer.Code != http.StatusBadRequest || harness.factory.created[0].closeCountValue() != 1 {
		t.Fatalf("cross-attestation offer was not contained: status=%d", badOffer.Code)
	}
}

func TestExpiredAuthorityLeaseCannotCreateOrReportAnAttempt(t *testing.T) {
	harness := newServiceHarness(t)
	binding := ensureTestAuthorityBinding(t, harness.service)
	harness.clock.mu.Lock()
	harness.clock.now = harness.clock.now.Add(time.Minute)
	harness.clock.mu.Unlock()
	response := serveRequest(
		t,
		harness.service,
		http.MethodPost,
		attemptsPath,
		testCreateAttemptRequest(binding, matrixRequestID(72), 1),
		testCredential,
	)
	if response.Code != http.StatusConflict || len(harness.factory.created) != 0 {
		t.Fatalf("expired authority created state: status=%d attempts=%d", response.Code, len(harness.factory.created))
	}
	if err := harness.service.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCoturnCredentialIsLeaseBoundAndAbsentFromAttestationAndEvidence(t *testing.T) {
	const turnSecret = "ephemeral-turn-secret-value"
	clock := &fakeLeaseClock{now: time.Date(2029, 12, 31, 23, 59, 30, 0, time.UTC)}
	factory := &fakeAttemptFactory{}
	var traces []TraceEvent
	config := completeServiceConfig(factory, &fakeProber{}, clock)
	config.Fixture = testExternalFixture("scheduled-coturn")
	config.Trace = func(event TraceEvent) { traces = append(traces, event) }
	service, err := NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	authorityRequest := newTestAuthorityProbeRequest(
		t, service, "coturn-service-run", "coturn-probe-nonce", 60_000,
	)
	turnDeclaration := ControlTURNDeclaration{
		CredentialID: "request-scoped-turn", Username: "request-scoped-user",
		ExpiresAt: clock.now.Add(time.Minute).Format(canonicalTimestampLayout),
	}
	controlLease, err := service.AcquireControlCredential(ControlCredentialAcquireRequest{
		RequestID: "acquire-" + authorityRequest.Nonce,
		RunID:     authorityRequest.ControlAuthority.SampleAuthority.RunID,
		ProfileID: service.profileID, ProbeNonce: authorityRequest.Nonce,
		RequestedLeaseMillis: authorityRequest.RequestedLeaseMillis, TURN: &turnDeclaration,
	})
	if err != nil {
		t.Fatal(err)
	}
	authorityRequest.ControlAuthority.ControlLeaseID = controlLease.LeaseID
	defer eraseCredentialBytes(controlLease.Credential)
	turnLease := ControlTURNCredentialLease{
		RequestID: controlLease.RequestID, RunID: controlLease.RunID,
		ProfileID: controlLease.ProfileID, ProbeNonce: controlLease.ProbeNonce,
		ControlLeaseID: controlLease.LeaseID, AttestationSHA256: controlLease.AttestationSHA256,
		CredentialID: turnDeclaration.CredentialID,
		Username:     turnDeclaration.Username,
		ExpiresAt:    turnDeclaration.ExpiresAt,
		MaxAttempts:  1, Credential: []byte(turnSecret),
	}
	if err := service.BindControlTURNCredential(turnLease); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	ownedCredential := service.controlTURNLeases[controlLease.LeaseID].credential
	service.mu.Unlock()
	probe := serveControlLeaseRequest(
		t, service, http.MethodPost, authorityProbePath, authorityRequest, controlLease,
	)
	if probe.Code != http.StatusOK || strings.Contains(probe.Body.String(), turnSecret) {
		t.Fatalf("Coturn attestation leaked a credential or failed: status=%d body=%s", probe.Code, probe.Body.String())
	}
	var signed AuthorityProbeResponse
	if err := json.Unmarshal(probe.Body.Bytes(), &signed); err != nil {
		t.Fatal(err)
	}
	if signed.Attestation.ExpiresAt != turnDeclaration.ExpiresAt ||
		signed.Attestation.Fixture.NetworkSemantics.TURNCredentialID != turnDeclaration.CredentialID ||
		signed.Attestation.Fixture.NetworkSemantics.TURNUsername != turnDeclaration.Username ||
		signed.Attestation.Fixture.NetworkSemantics.TURNCredentialExpiresAt != turnDeclaration.ExpiresAt {
		t.Fatalf("credential and attestation leases diverged: %q != %q",
			signed.Attestation.ExpiresAt, turnDeclaration.ExpiresAt)
	}
	service.mu.Lock()
	binding := service.authorityLeases[signed.AttestationSHA256].binding
	service.mu.Unlock()
	credentialResponse := serveControlLeaseRequest(t, service, http.MethodPost, turnCredentialPath, TURNCredentialRequest{
		ProtocolVersion: ProtocolVersion, AttemptBinding: binding,
	}, controlLease)
	var credential TURNCredentialResponse
	if credentialResponse.Code != http.StatusOK || json.Unmarshal(credentialResponse.Body.Bytes(), &credential) != nil ||
		credential.Credential != turnSecret || credential.CredentialID != turnDeclaration.CredentialID ||
		credential.ExpiresAt != signed.Attestation.ExpiresAt || credential.AttemptBinding != binding {
		t.Fatalf("TURN credential response is not lease-bound: %s", credentialResponse.Body.String())
	}
	for _, value := range ownedCredential {
		if value != 0 {
			t.Fatal("one-shot TURN delivery retained its stored provider password")
		}
	}
	if replay := serveControlLeaseRequest(t, service, http.MethodPost, turnCredentialPath, TURNCredentialRequest{
		ProtocolVersion: ProtocolVersion, AttemptBinding: binding,
	}, controlLease); replay.Code != http.StatusConflict || strings.Contains(replay.Body.String(), turnSecret) {
		t.Fatalf("TURN credential delivery replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	traceDocument, _ := json.Marshal(traces)
	if strings.Contains(string(traceDocument), turnSecret) {
		t.Fatal("TURN credential entered attempt evidence or traces")
	}
	hostile := binding
	hostile.FixtureBinding.RemotePeerBindingSHA256 = strings.Repeat("e", 64)
	if response := serveControlLeaseRequest(t, service, http.MethodPost, turnCredentialPath, TURNCredentialRequest{
		ProtocolVersion: ProtocolVersion, AttemptBinding: hostile,
	}, controlLease); response.Code != http.StatusConflict {
		t.Fatalf("spliced TURN credential binding status=%d", response.Code)
	}
	if _, err := service.ReleaseControlCredential(controlLease.LeaseID); err != nil {
		t.Fatal(err)
	}
	if response := serveControlLeaseRequest(t, service, http.MethodPost, turnCredentialPath, TURNCredentialRequest{
		ProtocolVersion: ProtocolVersion, AttemptBinding: binding,
	}, controlLease); response.Code != http.StatusUnauthorized {
		t.Fatalf("retired control lease retained TURN authority: status=%d", response.Code)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAmbiguousCoturnDeliveryBurnsCredentialAndRevokesControlLease(t *testing.T) {
	const turnSecret = "ambiguous-ephemeral-turn-secret"
	clock := &fakeLeaseClock{now: time.Date(2029, 12, 31, 23, 59, 30, 0, time.UTC)}
	config := completeServiceConfig(&fakeAttemptFactory{}, &fakeProber{}, clock)
	config.Fixture = testExternalFixture("scheduled-coturn")
	service, err := NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := service.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	authorityRequest := newTestAuthorityProbeRequest(
		t, service, "ambiguous-coturn-run", "ambiguous-coturn-probe-nonce", 60_000,
	)
	turnDeclaration := ControlTURNDeclaration{
		CredentialID: "ambiguous-turn-credential", Username: "ambiguous-turn-user",
		ExpiresAt: clock.now.Add(time.Minute).Format(canonicalTimestampLayout),
	}
	controlLease, err := service.AcquireControlCredential(ControlCredentialAcquireRequest{
		RequestID: "acquire-" + authorityRequest.Nonce,
		RunID:     authorityRequest.ControlAuthority.SampleAuthority.RunID,
		ProfileID: service.profileID, ProbeNonce: authorityRequest.Nonce,
		RequestedLeaseMillis: authorityRequest.RequestedLeaseMillis, TURN: &turnDeclaration,
	})
	if err != nil {
		t.Fatal(err)
	}
	authorityRequest.ControlAuthority.ControlLeaseID = controlLease.LeaseID
	defer eraseCredentialBytes(controlLease.Credential)
	if err := service.BindControlTURNCredential(ControlTURNCredentialLease{
		RequestID: controlLease.RequestID, RunID: controlLease.RunID,
		ProfileID: controlLease.ProfileID, ProbeNonce: controlLease.ProbeNonce,
		ControlLeaseID: controlLease.LeaseID, AttestationSHA256: controlLease.AttestationSHA256,
		CredentialID: turnDeclaration.CredentialID, Username: turnDeclaration.Username,
		ExpiresAt: turnDeclaration.ExpiresAt, MaxAttempts: 1, Credential: []byte(turnSecret),
	}); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	ownedCredential := service.controlTURNLeases[controlLease.LeaseID].credential
	service.mu.Unlock()
	probe := serveControlLeaseRequest(
		t, service, http.MethodPost, authorityProbePath, authorityRequest, controlLease,
	)
	var signed AuthorityProbeResponse
	if probe.Code != http.StatusOK || json.Unmarshal(probe.Body.Bytes(), &signed) != nil {
		t.Fatalf("authority probe status=%d body=%s", probe.Code, probe.Body.String())
	}
	service.mu.Lock()
	binding := service.authorityLeases[signed.AttestationSHA256].binding
	service.mu.Unlock()
	body, err := json.Marshal(TURNCredentialRequest{
		ProtocolVersion: ProtocolVersion, AttemptBinding: binding,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, turnCredentialPath, bytes.NewReader(body))
	origin, err := url.Parse(service.fixture.ControllerOrigin)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = origin.Host
	request.TLS = &tls.ConnectionState{}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+string(controlLease.Credential))
	request.Header.Set(ControlLeaseIDHeader, controlLease.LeaseID)
	writer := &failingResponseWriter{}
	service.ServeHTTP(writer, request)
	if writer.status != http.StatusOK || writer.writes != 1 {
		t.Fatalf("ambiguous delivery status=%d writes=%d", writer.status, writer.writes)
	}
	for _, value := range ownedCredential {
		if value != 0 {
			t.Fatal("ambiguous TURN delivery retained the authority-owned credential")
		}
	}
	receipt, err := service.RevokeControlCredentialAndWait(controlLease.LeaseID)
	if err != nil || receipt.LeaseID != controlLease.LeaseID || receipt.Terminal != "revoked" {
		t.Fatalf("ambiguous delivery retirement receipt=%#v err=%v", receipt, err)
	}
	service.mu.Lock()
	_, turnAuthorityRetained := service.controlTURNLeases[controlLease.LeaseID]
	service.mu.Unlock()
	if turnAuthorityRetained {
		t.Fatal("ambiguous TURN delivery retained its delivery authority after revocation")
	}
	replay := serveControlLeaseRequest(t, service, http.MethodPost, turnCredentialPath, TURNCredentialRequest{
		ProtocolVersion: ProtocolVersion, AttemptBinding: binding,
	}, controlLease)
	if replay.Code != http.StatusUnauthorized || strings.Contains(replay.Body.String(), turnSecret) {
		t.Fatalf("ambiguous delivery replay status=%d body=%s", replay.Code, replay.Body.String())
	}
}

func TestAttemptLifecycleOffersReportsAndReapsExactlyOnce(t *testing.T) {
	harness := newServiceHarness(t)
	created := createAttempt(t, harness, strings.Repeat("r", 16))
	attemptID := created.AttemptAuthority.AttemptID
	if created.LeaseExpiresAt != harness.clock.now.Truncate(time.Millisecond).Add(5*time.Second).Format(canonicalTimestampLayout) {
		t.Fatalf("wrong expiry: %q", created.LeaseExpiresAt)
	}
	resultResponse := serveRequest(t, harness.service, http.MethodGet, attemptsPath+"/"+attemptID, nil, testCredential)
	if resultResponse.Code != http.StatusOK {
		t.Fatalf("result status=%d", resultResponse.Code)
	}
	offer := OfferRequest{
		ProtocolVersion: ProtocolVersion, AttemptAuthority: created.AttemptAuthority,
		Type: "offer", SDP: "v=0\r\noffer",
	}
	offerResponse := serveRequest(t, harness.service, http.MethodPost, attemptsPath+"/"+attemptID+"/offer", offer, testCredential)
	if offerResponse.Code != http.StatusOK || !strings.Contains(offerResponse.Body.String(), "answer") {
		t.Fatalf("offer status=%d body=%s", offerResponse.Code, offerResponse.Body.String())
	}
	deleteResponse := serveRequest(t, harness.service, http.MethodDelete, attemptsPath+"/"+attemptID, nil, testCredential)
	if deleteResponse.Code != http.StatusOK || harness.factory.created[0].closeCount != 1 || !harness.clock.timers[0].stopped {
		t.Fatalf("attempt was not reaped exactly once: status=%d closes=%d", deleteResponse.Code, harness.factory.created[0].closeCount)
	}
	harness.clock.timers[0].Fire()
	if harness.factory.created[0].closeCount != 1 {
		t.Fatal("stopped lease timer reaped attempt twice")
	}
	if missing := serveRequest(t, harness.service, http.MethodGet, attemptsPath+"/"+attemptID, nil, testCredential); missing.Code != http.StatusNotFound {
		t.Fatalf("deleted attempt remains visible: %d", missing.Code)
	}
	if err := harness.service.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestServiceEmitsStructuredLifecycleTraceWithoutSecrets(t *testing.T) {
	factory := &fakeAttemptFactory{}
	clock := &fakeLeaseClock{now: time.Now().UTC()}
	var events []TraceEvent
	config := completeServiceConfig(factory, &fakeProber{}, clock)
	config.Trace = func(event TraceEvent) { events = append(events, event) }
	service, err := NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	harness := serviceHarness{service: service, factory: factory, clock: clock}
	created := createAttempt(t, harness, matrixRequestID(70))
	attemptID := created.AttemptAuthority.AttemptID
	offer := serveRequest(t, service, http.MethodPost, attemptsPath+"/"+attemptID+"/offer", OfferRequest{
		ProtocolVersion: ProtocolVersion, AttemptAuthority: created.AttemptAuthority,
		Type: "offer", SDP: "v=0\r\noffer",
	}, testCredential)
	if offer.Code != http.StatusOK {
		t.Fatalf("offer status=%d", offer.Code)
	}
	cleanup := serveRequest(t, service, http.MethodDelete, attemptsPath+"/"+attemptID, nil, testCredential)
	if cleanup.Code != http.StatusOK {
		t.Fatalf("cleanup status=%d", cleanup.Code)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	milestones := make([]string, len(events))
	for index, event := range events {
		milestones[index] = event.Milestone
		encoded, encodeErr := json.Marshal(event)
		if encodeErr != nil || bytes.Contains(encoded, []byte(testCredential)) ||
			bytes.Contains(encoded, []byte(created.AttemptAuthority.Challenge)) {
			t.Fatalf("trace reflected secret authority: %s", encoded)
		}
	}
	want := []string{
		traceAuthorityProbe,
		traceAttemptReserved, traceAttemptStarting, traceAttemptActive,
		traceOfferTerminal, traceAttemptRetiring, traceAttemptReaped,
	}
	if !slices.Equal(milestones, want) {
		t.Fatalf("trace milestones=%v want=%v", milestones, want)
	}
}

func TestInvalidOfferAndFactoryFailureReapStartedState(t *testing.T) {
	harness := newServiceHarness(t)
	created := createAttempt(t, harness, strings.Repeat("r", 16))
	attemptID := created.AttemptAuthority.AttemptID
	invalid := OfferRequest{
		ProtocolVersion: ProtocolVersion, AttemptAuthority: created.AttemptAuthority,
		Type: "answer", SDP: "v=0",
	}
	response := serveRequest(t, harness.service, http.MethodPost, attemptsPath+"/"+attemptID+"/offer", invalid, testCredential)
	if response.Code != http.StatusBadRequest || harness.factory.created[0].closeCount != 1 {
		t.Fatalf("invalid offer did not reap: status=%d closes=%d", response.Code, harness.factory.created[0].closeCount)
	}
	harness.factory.createErr = errors.New("start failed")
	response = serveRequest(
		t,
		harness.service,
		http.MethodPost,
		attemptsPath,
		newTestCreateAttemptRequest(t, harness.service, strings.Repeat("s", 16), 100),
		testCredential,
	)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("factory error status=%d", response.Code)
	}
	_ = harness.service.Close()
}

func TestFactoryHandleReturnedWithErrorIsReapedBeforeReservationRelease(t *testing.T) {
	factory := &gatedAttemptFactory{createErr: errors.New("partial start failed")}
	clock := &fakeLeaseClock{now: time.Now().UTC()}
	service, err := NewService(completeServiceConfig(factory, &fakeProber{}, clock))
	if err != nil {
		t.Fatal(err)
	}
	response := serveRequest(
		t,
		service,
		http.MethodPost,
		attemptsPath,
		newTestCreateAttemptRequest(t, service, matrixRequestID(76), 1_000),
		testCredential,
	)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("partial factory failure status=%d", response.Code)
	}
	attempts := factory.createdAttempts()
	if len(attempts) != 1 || attempts[0].closeCountValue() != 1 {
		t.Fatalf("partial factory state was not fenced exactly once: attempts=%d", len(attempts))
	}
	service.mu.Lock()
	occupied, starting := service.occupied, service.starting
	reservations := len(service.attemptReservations)
	service.mu.Unlock()
	if occupied != 0 || starting != 0 || reservations != 0 {
		t.Fatalf("failed factory retained reservation: occupied=%d starting=%d attempts=%d", occupied, starting, reservations)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAttemptIDCollisionWithRequestIdentityReleasesAdmissionBeforeFactory(t *testing.T) {
	factory := &fakeAttemptFactory{}
	clock := &fakeLeaseClock{now: time.Now().UTC()}
	service, err := NewService(completeServiceConfig(factory, &fakeProber{}, clock))
	if err != nil {
		t.Fatal(err)
	}
	collidingRequestID := base64.RawURLEncoding.EncodeToString(
		bytes.Repeat([]byte{1}, attemptIdentifierBytes),
	)
	response := serveRequest(
		t,
		service,
		http.MethodPost,
		attemptsPath,
		newTestCreateAttemptRequest(t, service, collidingRequestID, 1_000),
		testCredential,
	)
	if response.Code != http.StatusInternalServerError || len(factory.created) != 0 {
		t.Fatalf("colliding identities reached factory: status=%d attempts=%d", response.Code, len(factory.created))
	}
	service.mu.Lock()
	occupied, starting, reservations := service.occupied, service.starting, len(service.attemptReservations)
	service.mu.Unlock()
	if occupied != 0 || starting != 0 || reservations != 0 {
		t.Fatalf("identity collision retained admission: occupied=%d starting=%d attempts=%d", occupied, starting, reservations)
	}
	harness := serviceHarness{service: service, factory: factory, clock: clock}
	createAttempt(t, harness, matrixRequestID(77))
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRejectionAfterStartAlwaysClosesAttempt(t *testing.T) {
	harness := newServiceHarness(t)
	requestID := strings.Repeat("r", 16)
	created := createAttempt(t, harness, requestID)
	duplicateRequest := CreateAttemptRequest{
		ProtocolVersion:  ProtocolVersion,
		RequestAuthority: created.AttemptAuthority.RequestAuthority,
		LeaseMillis:      100,
	}
	duplicate := serveRequest(
		t, harness.service, http.MethodPost, attemptsPath, duplicateRequest, testCredential,
	)
	if duplicate.Code != http.StatusConflict || len(harness.factory.created) != 1 {
		t.Fatalf("duplicate admission created state: status=%d created=%d", duplicate.Code, len(harness.factory.created))
	}
	harness.clock.nilTimer = true
	failedTimer := serveRequest(
		t,
		harness.service,
		http.MethodPost,
		attemptsPath,
		newTestCreateAttemptRequest(t, harness.service, strings.Repeat("t", 16), 100),
		testCredential,
	)
	if failedTimer.Code != http.StatusInternalServerError || harness.factory.created[1].closeCount != 1 {
		t.Fatalf("timer rejection leaked state: status=%d closes=%d", failedTimer.Code, harness.factory.created[1].closeCount)
	}
	_ = harness.service.Close()
}

func TestLeaseAndDeleteCleanupFailuresContaminateService(t *testing.T) {
	for _, trigger := range []string{"lease", "delete"} {
		t.Run(trigger, func(t *testing.T) {
			harness := newServiceHarness(t)
			created := createAttempt(t, harness, strings.Repeat("r", 16))
			harness.factory.created[0].closeErr = errors.New("close failed")
			if trigger == "lease" {
				harness.clock.timers[0].Fire()
			} else {
				response := serveRequest(
					t, harness.service, http.MethodDelete,
					attemptsPath+"/"+created.AttemptAuthority.AttemptID, nil, testCredential,
				)
				if response.Code != http.StatusInternalServerError {
					t.Fatalf("cleanup failure status=%d", response.Code)
				}
			}
			blocked := serveRequest(t, harness.service, http.MethodPost, probePath, ProbeRequest{}, testCredential)
			if blocked.Code != http.StatusServiceUnavailable {
				t.Fatalf("contaminated service accepted work: %d", blocked.Code)
			}
			if err := harness.service.Close(); err == nil || strings.Contains(err.Error(), "close failed") {
				t.Fatalf("containment verdict missing or reflected implementation error: %v", err)
			}
		})
	}
}

func TestCloseReapsAllAttemptsAndErasesCredential(t *testing.T) {
	harness := newServiceHarness(t)
	createAttempt(t, harness, strings.Repeat("r", 16))
	createAttempt(t, harness, strings.Repeat("s", 16))
	if err := harness.service.Close(); err != nil {
		t.Fatal(err)
	}
	for _, attempt := range harness.factory.created {
		if attempt.closeCount != 1 {
			t.Fatalf("close count=%d", attempt.closeCount)
		}
	}
	if !bytes.Equal(harness.service.credential, make([]byte, len(testCredential))) {
		t.Fatal("service retained control credential after close")
	}
	if err := harness.service.Close(); err != nil {
		t.Fatal(err)
	}
	if response := serveRequest(t, harness.service, http.MethodGet, attemptsPath+"/aaaaaaaaaaaaaaaa", nil, testCredential); response.Code != http.StatusUnauthorized {
		// Erasing the credential intentionally makes every post-close request unauthorized.
		t.Fatalf("closed service still accepted its former credential: %d", response.Code)
	}
}

func TestMalformedRoutesLeasesAndIdentityFailureAreRejected(t *testing.T) {
	harness := newServiceHarness(t)
	defer harness.service.Close() //nolint:errcheck
	shortRequest := newTestCreateAttemptRequest(t, harness.service, "short", 1)
	oversizedRequest := newTestCreateAttemptRequest(
		t, harness.service, strings.Repeat("r", 16), 61_000,
	)
	for _, request := range []struct {
		method, path string
		body         any
		status       int
	}{
		{http.MethodGet, "/unknown", nil, http.StatusNotFound},
		{http.MethodGet, attemptsPath, nil, http.StatusMethodNotAllowed},
		{http.MethodGet, attemptsPath + "/bad/id/more", nil, http.StatusNotFound},
		{http.MethodPost, attemptsPath, shortRequest, http.StatusBadRequest},
		{http.MethodPost, attemptsPath, oversizedRequest, http.StatusBadRequest},
	} {
		response := serveRequest(t, harness.service, request.method, request.path, request.body, testCredential)
		if response.Code != request.status {
			t.Fatalf("%s %s status=%d want=%d", request.method, request.path, response.Code, request.status)
		}
	}
	harness.service.attemptIDSource = bytes.NewReader(nil)
	response := serveRequest(
		t,
		harness.service,
		http.MethodPost,
		attemptsPath,
		newTestCreateAttemptRequest(t, harness.service, strings.Repeat("z", 16), 1),
		testCredential,
	)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("attempt ID failure status=%d", response.Code)
	}
	harness.service.attemptIDSource = attemptIDStream(1)
	harness.service.challengeSource = bytes.NewReader(nil)
	response = serveRequest(
		t,
		harness.service,
		http.MethodPost,
		attemptsPath,
		newTestCreateAttemptRequest(t, harness.service, strings.Repeat("y", 16), 1),
		testCredential,
	)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("challenge failure status=%d", response.Code)
	}
}

func TestConcurrentDeleteExpiryAndCloseKeepRetiringOwnerUntilCloseTerminal(t *testing.T) {
	harness := newServiceHarness(t)
	created := createAttempt(t, harness, matrixRequestID(1))
	attemptID := created.AttemptAuthority.AttemptID
	attempt := harness.factory.created[0]
	closeRelease := make(chan struct{})
	attempt.mu.Lock()
	attempt.closeStarted = make(chan struct{})
	attempt.closeRelease = closeRelease
	closeStarted := attempt.closeStarted
	attempt.mu.Unlock()

	firstDelete := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDelete <- serveRequest(t, harness.service, http.MethodDelete, attemptsPath+"/"+attemptID, nil, testCredential)
	}()
	awaitSignal(t, closeStarted, "attempt close start")
	harness.clock.timers[0].Fire()

	closeDone := make(chan error, 1)
	go func() { closeDone <- harness.service.Close() }()
	secondDelete := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		secondDelete <- serveRequest(t, harness.service, http.MethodDelete, attemptsPath+"/"+attemptID, nil, testCredential)
	}()
	assertBlocked(t, closeDone, "service close")
	assertBlocked(t, firstDelete, "owning delete")
	assertBlocked(t, secondDelete, "observing delete")

	harness.service.mu.Lock()
	activeCount, retiringOwner := len(harness.service.active), harness.service.retiring[attemptID]
	harness.service.mu.Unlock()
	if activeCount != 0 || retiringOwner == nil {
		t.Fatalf("retirement lost registry ownership: active=%d retiring=%v", activeCount, retiringOwner != nil)
	}
	close(closeRelease)
	if response := awaitResponse(t, firstDelete, "owning delete"); response.Code != http.StatusOK {
		t.Fatalf("owning delete status=%d", response.Code)
	}
	if response := awaitResponse(t, secondDelete, "observing delete"); response.Code != http.StatusOK {
		t.Fatalf("observing delete status=%d", response.Code)
	}
	if err := awaitError(t, closeDone, "service close"); err != nil {
		t.Fatal(err)
	}
	if attempt.closeCountValue() != 1 {
		t.Fatalf("racing retirement closed attempt %d times", attempt.closeCountValue())
	}
}

func TestCloseWaitsForLateFactoryAndLateAttemptClose(t *testing.T) {
	factoryRelease := make(chan struct{})
	attemptCloseRelease := make(chan struct{})
	attemptCloseStarted := make(chan struct{})
	factory := &gatedAttemptFactory{
		started: make(chan string, 1), release: factoryRelease,
		configure: func(attempt *fakeAttempt) {
			attempt.closeStarted = attemptCloseStarted
			attempt.closeRelease = attemptCloseRelease
		},
	}
	clock := &fakeLeaseClock{now: time.Now().UTC()}
	config := completeServiceConfig(factory, &fakeProber{}, clock)
	config.AttemptStartTimeout = 20 * time.Millisecond
	service, err := NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	createRequest := newTestCreateAttemptRequest(t, service, matrixRequestID(2), 1_000)
	createDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		createDone <- serveRequest(
			t, service, http.MethodPost, attemptsPath, createRequest, testCredential,
		)
	}()
	awaitSignal(t, factory.started, "attempt factory start")
	time.Sleep(30 * time.Millisecond)
	closeDone := make(chan error, 1)
	go func() { closeDone <- service.Close() }()
	assertBlocked(t, closeDone, "service close during attempt start")
	close(factoryRelease)
	awaitSignal(t, attemptCloseStarted, "rejected attempt close start")
	assertBlocked(t, closeDone, "service close during attempt close")
	assertBlocked(t, createDone, "create rejection during attempt close")
	close(attemptCloseRelease)
	if response := awaitResponse(t, createDone, "create rejection"); response.Code != http.StatusInternalServerError {
		t.Fatalf("late factory result status=%d", response.Code)
	}
	if err := awaitError(t, closeDone, "service close"); err != nil {
		t.Fatal(err)
	}
	attempts := factory.createdAttempts()
	if len(attempts) != 1 || attempts[0].closeCountValue() != 1 {
		t.Fatalf("late factory result was not reaped exactly once: attempts=%d", len(attempts))
	}
}

func TestCloseFencesChallengeAllocationAfterAdmissionReservation(t *testing.T) {
	challengeRelease := make(chan struct{})
	challenge := &gatedChallengeReader{
		started: make(chan struct{}), release: challengeRelease,
		data: bytes.Repeat([]byte{7}, attemptChallengeBytes),
	}
	factory := &fakeAttemptFactory{}
	config := completeServiceConfig(factory, &fakeProber{}, &fakeLeaseClock{now: time.Now().UTC()})
	config.ChallengeSource = challenge
	service, err := NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	createRequest := newTestCreateAttemptRequest(t, service, matrixRequestID(75), 1_000)
	createDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		createDone <- serveRequest(
			t, service, http.MethodPost, attemptsPath, createRequest, testCredential,
		)
	}()
	awaitSignal(t, challenge.started, "challenge allocation")
	closeDone := make(chan error, 1)
	go func() { closeDone <- service.Close() }()
	assertBlocked(t, closeDone, "service close during challenge allocation")
	close(challengeRelease)
	if response := awaitResponse(t, createDone, "challenge allocation rejection"); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("challenge allocation after close status=%d", response.Code)
	}
	if err := awaitError(t, closeDone, "identity-fenced close"); err != nil {
		t.Fatal(err)
	}
	if len(factory.created) != 0 {
		t.Fatalf("challenge allocation raced factory creation: %d", len(factory.created))
	}
}

func TestOfferDeadlineForcesCloseAndWaitsForOfferTerminal(t *testing.T) {
	harness := newServiceHarness(t)
	harness.service.offerTimeout = 25 * time.Millisecond
	created := createAttempt(t, harness, matrixRequestID(3))
	attempt := harness.factory.created[0]
	attempt.mu.Lock()
	attempt.offerStarted = make(chan struct{})
	attempt.offerRelease = make(chan struct{})
	attempt.closeReleasesOffer = true
	offerStarted := attempt.offerStarted
	attempt.mu.Unlock()

	responseDone := make(chan *httptest.ResponseRecorder, 1)
	startedAt := time.Now()
	attemptID := created.AttemptAuthority.AttemptID
	go func() {
		responseDone <- serveRequest(t, harness.service, http.MethodPost,
			attemptsPath+"/"+attemptID+"/offer", OfferRequest{
				ProtocolVersion: ProtocolVersion, AttemptAuthority: created.AttemptAuthority,
				Type: "offer", SDP: "v=0\r\noffer",
			}, testCredential)
	}()
	awaitSignal(t, offerStarted, "offer start")
	response := awaitResponse(t, responseDone, "bounded offer")
	if response.Code != http.StatusUnprocessableEntity || time.Since(startedAt) > time.Second {
		t.Fatalf("offer deadline was not bounded: status=%d elapsed=%s", response.Code, time.Since(startedAt))
	}
	if attempt.closeCountValue() != 1 {
		t.Fatalf("forced offer containment closed attempt %d times", attempt.closeCountValue())
	}
	if err := harness.service.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSlowRequestBodyIsBoundedByLiveHTTPConnectionDeadline(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	harness := newServiceHarness(t)
	harness.service.bodyReadTimeout = 25 * time.Millisecond
	server := httptest.NewTLSServer(harness.service)
	harness.service.fixture.ControllerOrigin = "https://" + server.Listener.Addr().String() + "/"
	dialer := &net.Dialer{Timeout: time.Second}
	connection, err := tls.DialWithDialer(dialer, "tcp", server.Listener.Addr().String(), &tls.Config{
		MinVersion: tls.VersionTLS13,
		// The live deadline, not PKI, is the subject of this loopback-only test.
		InsecureSkipVerify: true, //nolint:gosec
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	requestHead := fmt.Sprintf(
		"POST %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: 512\r\nConnection: close\r\n\r\n{\"protocolVersion\":",
		attemptsPath, server.Listener.Addr().String(), testCredential,
	)
	startedAt := time.Now()
	if _, err := io.WriteString(connection, requestHead); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusBadRequest || time.Since(startedAt) > time.Second {
		t.Fatalf("slow body was not rejected within its authority: status=%d elapsed=%s", response.StatusCode, time.Since(startedAt))
	}
	_ = response.Body.Close()
	_ = connection.Close()
	server.Close()
	if err := harness.service.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAdmissionReservesCapacityBeforeFactoryAndRejectsDuplicateWithoutCreation(t *testing.T) {
	t.Run("storm", func(t *testing.T) {
		release := make(chan struct{})
		factory := &gatedAttemptFactory{started: make(chan string, 16), release: release}
		config := completeServiceConfig(factory, &fakeProber{}, &fakeLeaseClock{now: time.Now().UTC()})
		config.MaximumActive = 2
		config.MaximumTombstones = 16
		service, err := NewService(config)
		if err != nil {
			t.Fatal(err)
		}
		const requestCount = 12
		responses := make(chan *httptest.ResponseRecorder, requestCount)
		for index := 0; index < requestCount; index++ {
			request := newTestCreateAttemptRequest(
				t, service, matrixRequestID(index+10), 1_000,
			)
			go func(request CreateAttemptRequest) {
				responses <- serveRequest(
					t, service, http.MethodPost, attemptsPath, request, testCredential,
				)
			}(request)
		}
		awaitSignal(t, factory.started, "first admitted factory")
		awaitSignal(t, factory.started, "second admitted factory")
		close(release)
		created, rejected := 0, 0
		for index := 0; index < requestCount; index++ {
			switch response := awaitResponse(t, responses, "admission response"); response.Code {
			case http.StatusCreated:
				created++
			case http.StatusTooManyRequests:
				rejected++
			default:
				t.Fatalf("unexpected admission status=%d", response.Code)
			}
		}
		if len(factory.createdAttempts()) != 2 || created != 2 || rejected != requestCount-2 {
			t.Fatalf("admission escaped capacity: factory=%d created=%d rejected=%d", len(factory.createdAttempts()), created, rejected)
		}
		if err := service.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("duplicate", func(t *testing.T) {
		release := make(chan struct{})
		factory := &gatedAttemptFactory{started: make(chan string, 1), release: release}
		service, err := NewService(completeServiceConfig(factory, &fakeProber{}, &fakeLeaseClock{now: time.Now().UTC()}))
		if err != nil {
			t.Fatal(err)
		}
		requestID := matrixRequestID(30)
		request := newTestCreateAttemptRequest(t, service, requestID, 1_000)
		first := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			first <- serveRequest(
				t, service, http.MethodPost, attemptsPath, request, testCredential,
			)
		}()
		awaitSignal(t, factory.started, "duplicate owner factory")
		duplicateDone := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			duplicateDone <- serveRequest(
				t, service, http.MethodPost, attemptsPath, request, testCredential,
			)
		}()
		assertBlocked(t, duplicateDone, "duplicate replay during startup")
		if len(factory.createdAttempts()) != 1 {
			t.Fatalf("duplicate allocated %d factory states", len(factory.createdAttempts()))
		}
		close(release)
		ownerResponse := awaitResponse(t, first, "duplicate owner")
		if ownerResponse.Code != http.StatusCreated {
			t.Fatalf("duplicate owner status=%d", ownerResponse.Code)
		}
		duplicate := awaitResponse(t, duplicateDone, "duplicate replay")
		if duplicate.Code != http.StatusOK {
			t.Fatalf("duplicate replay status=%d body=%s", duplicate.Code, duplicate.Body.String())
		}
		var owner, replay CreateAttemptResponse
		if err := json.Unmarshal(ownerResponse.Body.Bytes(), &owner); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(duplicate.Body.Bytes(), &replay); err != nil {
			t.Fatal(err)
		}
		if owner != replay || owner.AttemptAuthority.AttemptID == requestID {
			t.Fatalf("duplicate did not replay the distinct server attempt: owner=%#v replay=%#v", owner, replay)
		}
		if err := service.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestTombstonesBoundMemoryAndBlockReuseUntilRetentionExpires(t *testing.T) {
	factory := &fakeAttemptFactory{}
	clock := &fakeLeaseClock{now: time.Now().UTC()}
	config := completeServiceConfig(factory, &fakeProber{}, clock)
	config.MaximumActive = 1
	config.MaximumTombstones = 3
	service, err := NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	harness := serviceHarness{service: service, factory: factory, clock: clock}
	firstRequests := []CreateAttemptRequest{
		newTestCreateAttemptRequest(t, service, matrixRequestID(40), 5_000),
		newTestCreateAttemptRequest(t, service, matrixRequestID(41), 5_000),
	}
	blockedID := matrixRequestID(42)
	blockedRequest := newTestCreateAttemptRequest(t, service, blockedID, 1_000)
	service.maximumTombstones = 2
	for _, request := range firstRequests {
		created := createAttemptFromRequest(t, harness, request)
		response := serveRequest(
			t, service, http.MethodDelete,
			attemptsPath+"/"+created.AttemptAuthority.AttemptID, nil, testCredential,
		)
		if response.Code != http.StatusOK {
			t.Fatalf("delete status=%d", response.Code)
		}
	}
	blocked := serveRequest(
		t, service, http.MethodPost, attemptsPath, blockedRequest, testCredential,
	)
	if blocked.Code != http.StatusTooManyRequests {
		t.Fatalf("full tombstone authority status=%d", blocked.Code)
	}
	service.mu.Lock()
	if len(service.tombstones) != 2 || len(service.requestTombstones) != 2 {
		t.Fatalf("tombstones are not bounded: attempts=%d requests=%d", len(service.tombstones), len(service.requestTombstones))
	}
	service.mu.Unlock()
	clock.mu.Lock()
	clock.now = clock.now.Add(2 * time.Minute)
	retirementObservedAt := clock.now
	clock.mu.Unlock()
	// Expired control claims enter retained retirement only when observed. Model
	// that second lifetime before asking the authority to issue a new lease.
	service.controlCredentials.mu.Lock()
	service.controlCredentials.pruneLocked(retirementObservedAt)
	service.controlCredentials.mu.Unlock()
	clock.mu.Lock()
	clock.now = clock.now.Add(time.Minute + time.Millisecond)
	clock.mu.Unlock()
	service.maximumTombstones = 3
	acceptedRequest := newTestCreateAttemptRequest(t, service, blockedID, 1_000)
	service.maximumTombstones = 2
	accepted := serveRequest(
		t, service, http.MethodPost, attemptsPath, acceptedRequest, testCredential,
	)
	if accepted.Code != http.StatusCreated {
		t.Fatalf("expired tombstones retained admission authority: status=%d", accepted.Code)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestContainmentFailureImmediatelyReapsEveryActiveAttempt(t *testing.T) {
	harness := newServiceHarness(t)
	first := createAttempt(t, harness, matrixRequestID(50))
	createAttempt(t, harness, matrixRequestID(51))
	firstAttempt, secondAttempt := harness.factory.created[0], harness.factory.created[1]
	secondCloseStarted := make(chan struct{})
	firstAttempt.mu.Lock()
	firstAttempt.closeErr = errors.New("close failed")
	firstAttempt.mu.Unlock()
	secondAttempt.mu.Lock()
	secondAttempt.closeStarted = secondCloseStarted
	secondAttempt.mu.Unlock()
	response := serveRequest(
		t, harness.service, http.MethodDelete,
		attemptsPath+"/"+first.AttemptAuthority.AttemptID, nil, testCredential,
	)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("cleanup failure status=%d", response.Code)
	}
	awaitSignal(t, secondCloseStarted, "sibling containment")
	if secondAttempt.closeCountValue() != 1 {
		t.Fatalf("sibling attempt close count=%d", secondAttempt.closeCountValue())
	}
	harness.service.mu.Lock()
	activeCount := len(harness.service.active)
	harness.service.mu.Unlock()
	if activeCount != 0 {
		t.Fatalf("containment failure left %d active attempts", activeCount)
	}
	if err := harness.service.Close(); err == nil {
		t.Fatal("containment failure was not retained by service close")
	}
}

func TestImmediateLeaseCallbackCannotPublishReapedAttempt(t *testing.T) {
	harness := newServiceHarness(t)
	harness.clock.mu.Lock()
	harness.clock.fireImmediately = true
	harness.clock.mu.Unlock()
	response := serveRequest(
		t,
		harness.service,
		http.MethodPost,
		attemptsPath,
		newTestCreateAttemptRequest(t, harness.service, matrixRequestID(60), 1_000),
		testCredential,
	)
	if response.Code != http.StatusConflict || len(harness.factory.created) != 1 || harness.factory.created[0].closeCountValue() != 1 {
		t.Fatalf("synchronous expiry published state: status=%d attempts=%d", response.Code, len(harness.factory.created))
	}
	if err := harness.service.Close(); err != nil {
		t.Fatal(err)
	}
}

func matrixRequestID(index int) string {
	return fmt.Sprintf("request-%08d", index)
}

func awaitSignal[T any](t *testing.T, channel <-chan T, label string) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(time.Second):
		var zero T
		t.Fatalf("timed out waiting for %s", label)
		return zero
	}
}

func assertBlocked[T any](t *testing.T, channel <-chan T, label string) {
	t.Helper()
	select {
	case <-channel:
		t.Fatalf("%s completed before owned cleanup was terminal", label)
	case <-time.After(20 * time.Millisecond):
	}
}

func awaitResponse(
	t *testing.T,
	channel <-chan *httptest.ResponseRecorder,
	label string,
) *httptest.ResponseRecorder {
	return awaitSignal(t, channel, label)
}

func awaitError(t *testing.T, channel <-chan error, label string) error {
	return awaitSignal(t, channel, label)
}
