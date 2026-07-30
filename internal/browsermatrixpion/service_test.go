package browsermatrixpion

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
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
