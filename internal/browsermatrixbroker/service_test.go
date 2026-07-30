package browsermatrixbroker

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/browsermatrixpion"
)

const (
	testControllerOrigin = "https://broker.example/"
	testRunID            = "scheduled-run"
	testProbeNonce       = "probe-nonce-00000001"
	testAcquireRequestID = "acquire-request-00000001"
	testReleaseRequestID = "release-request-00000001"
	testRevokeRequestID  = "revoke-request-00000001"
	testControlSecret    = "CONTROL_SECRET_0123456789_abcdefghij"
	testTURNSecret       = "TURN_SECRET_0123456789_abcdefghijkl"
)

type brokerTestClock struct{ now time.Time }

func (clock brokerTestClock) Now() time.Time { return clock.now }

type acceptingIdentityValidator struct {
	mu    sync.Mutex
	calls int
}

func (validator *acceptingIdentityValidator) Validate(context.Context, []byte) error {
	validator.mu.Lock()
	defer validator.mu.Unlock()
	validator.calls++
	return nil
}

func (validator *acceptingIdentityValidator) callCount() int {
	validator.mu.Lock()
	defer validator.mu.Unlock()
	return validator.calls
}

type brokerTestAdmin struct {
	mu           sync.Mutex
	now          time.Time
	leases       map[string]browsermatrixpion.ControlCredentialLease
	acquireCalls int
	bindCalls    int
	releaseCalls int
	revokeCalls  int
	acquireErr   error
	releaseErr   error
	revokeErr    error
}

func newBrokerTestAdmin(now time.Time) *brokerTestAdmin {
	return &brokerTestAdmin{now: now, leases: make(map[string]browsermatrixpion.ControlCredentialLease)}
}

func (admin *brokerTestAdmin) AcquireControlCredential(
	request browsermatrixpion.ControlCredentialAcquireRequest,
) (browsermatrixpion.ControlCredentialLease, error) {
	admin.mu.Lock()
	defer admin.mu.Unlock()
	admin.acquireCalls++
	if replay := admin.leases[request.RequestID]; replay.LeaseID != "" {
		replay.Credential = []byte(testControlSecret)
		return replay, nil
	}
	expiresAt := admin.now.Add(time.Duration(request.RequestedLeaseMillis) * time.Millisecond).
		Format(canonicalTimestampLayout)
	if request.TURN != nil {
		expiresAt = request.TURN.ExpiresAt
	}
	lease := browsermatrixpion.ControlCredentialLease{
		LeaseID: "control-lease-00000001", RequestID: request.RequestID,
		RunID: request.RunID, ProfileID: request.ProfileID,
		AuthorityInstanceID: "remote-authority", AttestationSHA256: string(bytes.Repeat([]byte{'a'}, 64)),
		ProbeNonce: request.ProbeNonce, IssuedAt: admin.now.Format(canonicalTimestampLayout),
		ExpiresAt: expiresAt, MaxAttempts: 1, Credential: []byte(testControlSecret),
		TURN: request.TURN,
	}
	admin.leases[request.RequestID] = lease
	return lease, admin.acquireErr
}

func (admin *brokerTestAdmin) BindControlTURNCredential(
	lease browsermatrixpion.ControlTURNCredentialLease,
) error {
	admin.mu.Lock()
	defer admin.mu.Unlock()
	admin.bindCalls++
	if lease.ControlLeaseID != "control-lease-00000001" ||
		!bytes.Equal(lease.Credential, []byte(testTURNSecret)) {
		return errors.New("unexpected TURN binding")
	}
	return nil
}

func (admin *brokerTestAdmin) ReleaseControlCredential(
	leaseID string,
) (browsermatrixpion.ControlCredentialReceipt, error) {
	admin.mu.Lock()
	defer admin.mu.Unlock()
	admin.releaseCalls++
	if admin.releaseErr != nil {
		return browsermatrixpion.ControlCredentialReceipt{}, admin.releaseErr
	}
	return browsermatrixpion.ControlCredentialReceipt{LeaseID: leaseID, Terminal: "revoked"}, nil
}

func (admin *brokerTestAdmin) RevokeControlCredentialAndWait(
	leaseID string,
) (browsermatrixpion.ControlCredentialReceipt, error) {
	admin.mu.Lock()
	defer admin.mu.Unlock()
	admin.revokeCalls++
	if admin.revokeErr != nil {
		return browsermatrixpion.ControlCredentialReceipt{}, admin.revokeErr
	}
	return browsermatrixpion.ControlCredentialReceipt{LeaseID: leaseID, Terminal: "revoked"}, nil
}

type brokerTestTURNProvider struct {
	mu          sync.Mutex
	request     TURNAcquireRequest
	bind        TURNBindRequest
	active      bool
	acquires    int
	binds       int
	revocations int
	revokeErr   error
}

func (provider *brokerTestTURNProvider) Acquire(
	_ context.Context,
	request TURNAcquireRequest,
) (TURNReservation, error) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.acquires++
	if provider.request != (TURNAcquireRequest{}) && provider.request != request {
		return TURNReservation{}, errors.New("TURN acquire replay changed scope")
	}
	provider.request = request
	return TURNReservation{
		ProviderLeaseID: "provider-lease-00000001", RequestID: request.RequestID,
		RunID: request.RunID, ProfileID: request.ProfileID, ProbeNonce: request.ProbeNonce,
		CredentialID: "dynamic-turn-credential", Username: "dynamic-turn-user",
		ExpiresAt: request.ExpiresAt, MaxAttempts: 1, Credential: []byte(testTURNSecret),
	}, nil
}

func (provider *brokerTestTURNProvider) BindAndWait(
	_ context.Context,
	request TURNBindRequest,
) (TURNBoundLease, error) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.binds++
	if provider.bind != (TURNBindRequest{}) && provider.bind != request {
		return TURNBoundLease{}, errors.New("TURN bind replay changed scope")
	}
	provider.bind = request
	provider.active = true
	return TURNBoundLease(request), nil
}

func (provider *brokerTestTURNProvider) RevokeAndWait(
	_ context.Context,
	request TURNRetirementRequest,
) (TURNRetirementReceipt, error) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.revocations++
	if provider.revokeErr != nil {
		return TURNRetirementReceipt{}, provider.revokeErr
	}
	provider.active = false
	return TURNRetirementReceipt{
		RequestID: request.RequestID, ProviderLeaseID: request.ProviderLeaseID, Terminal: "revoked",
	}, nil
}

func testIdentity() WorkloadIdentityBinding {
	return WorkloadIdentityBinding{
		ProtocolVersion: WorkloadIdentityProtocolVersion, Kind: "github-actions-oidc",
		Audience: "windshare-browser-matrix", Issuer: GitHubActionsOIDCIssuer,
		Repository: "windshare/windshare", Ref: "refs/heads/main",
		WorkflowRef:   "windshare/windshare/.github/workflows/network.yml@refs/heads/main",
		RequestOrigin: "https://github.example", RequestPath: "/oidc", RequestQuery: "?audience=windshare",
	}
}

func newBrokerTestHandler(
	t *testing.T,
	profileID string,
	admin *brokerTestAdmin,
	provider RevocableTURNProvider,
	validator WorkloadIdentityValidator,
) *Handler {
	t.Helper()
	seed := bytes.Repeat([]byte{7}, ed25519.SeedSize)
	handler, err := NewTestHandlerForHarness(Config{
		ControllerOrigin: testControllerOrigin, ProfileID: profileID, ExpectedIdentity: testIdentity(),
		LeaseDuration: time.Minute, RetirementTimeout: time.Second,
		TombstoneRetention: time.Hour, MaximumTombstones: 32, MaximumOIDCReplays: 32,
		Signer: ed25519.NewKeyFromSeed(seed), Admin: admin, TURNProvider: provider,
		Clock: brokerTestClock{now: admin.now},
	}, validator)
	erase(seed)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func acquireFrame(t *testing.T, profileID string) []byte {
	t.Helper()
	return requestFrame(t, acquireMetadata{
		SchemaVersion: ProtocolVersion, Operation: "acquire", RequestID: testAcquireRequestID,
		ReleaseRequestID: testReleaseRequestID, RevokeRequestID: testRevokeRequestID,
		ControllerOrigin: testControllerOrigin, RunID: testRunID, ProfileID: profileID,
		ProbeNonce: testProbeNonce, MaxAttempts: 1, WorkloadIdentity: testIdentity(),
		WorkloadIdentityByteLength: len("workload-assertion"),
	})
}

func retirementFrame(t *testing.T, profileID, operation, requestID, leaseID string) []byte {
	t.Helper()
	return requestFrame(t, retirementMetadata{
		SchemaVersion: ProtocolVersion, Operation: operation, RequestID: requestID,
		ControllerOrigin: testControllerOrigin, LeaseID: leaseID, RunID: testRunID,
		ProfileID: profileID, ProbeNonce: testProbeNonce, WorkloadIdentity: testIdentity(),
		WorkloadIdentityByteLength: len("workload-assertion"),
	})
}

func requestFrame(t *testing.T, metadata any) []byte {
	t.Helper()
	frame, err := encodeFrame(metadata, []byte("workload-assertion"))
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func exchangeFrame(handler *Handler, frame []byte) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, BrokerPath, bytes.NewReader(frame))
	request.Host = "broker.example"
	request.TLS = &tls.ConnectionState{}
	request.Header.Set("Content-Type", BrokerContentType)
	request.Header.Set("Accept", BrokerContentType)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeLeaseResponse(t *testing.T, response *httptest.ResponseRecorder) (leaseEnvelope, []byte) {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("broker status=%d", response.Code)
	}
	metadata, credential, err := splitFrame(response.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	var envelope leaseEnvelope
	if !decodeCanonicalMetadata(metadata, &envelope) {
		t.Fatal("lease envelope is not canonical")
	}
	return envelope, append([]byte(nil), credential...)
}

func TestHandlerExactReplayAndBoundRetirementIDs(t *testing.T) {
	now := time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)
	admin := newBrokerTestAdmin(now)
	validator := &acceptingIdentityValidator{}
	handler := newBrokerTestHandler(t, "scheduled-public-stun", admin, nil, validator)
	frame := acquireFrame(t, "scheduled-public-stun")
	first := exchangeFrame(handler, frame)
	firstBytes := append([]byte(nil), first.Body.Bytes()...)
	second := exchangeFrame(handler, frame)
	if !bytes.Equal(firstBytes, second.Body.Bytes()) {
		firstMetadata, firstPayload, firstErr := splitFrame(firstBytes)
		secondMetadata, secondPayload, secondErr := splitFrame(second.Body.Bytes())
		t.Fatalf("exact acquire replay changed response: first=(frame=%d metadata=%d payload=%d zeroes=%d valid=%t err=%v) second=(frame=%d metadata=%d payload=%d zeroes=%d valid=%t err=%v)",
			len(firstBytes), len(firstMetadata), len(firstPayload), bytes.Count(firstMetadata, []byte{0}), json.Valid(firstMetadata), firstErr,
			second.Body.Len(), len(secondMetadata), len(secondPayload), bytes.Count(secondMetadata, []byte{0}), json.Valid(secondMetadata), secondErr)
	}
	envelope, credential := decodeLeaseResponse(t, first)
	defer erase(credential)
	if !bytes.Equal(credential, []byte(testControlSecret)) ||
		envelope.Lease.ReleaseRequestID != testReleaseRequestID ||
		envelope.Lease.RevokeRequestID != testRevokeRequestID ||
		envelope.Lease.TURNCapability != "not-required" ||
		bytes.Contains(mustCanonicalJSON(t, envelope), []byte(testControlSecret)) {
		t.Fatal("lease response lost its authority boundary")
	}
	admin.mu.Lock()
	adminCallsBeforeHostile := admin.releaseCalls + admin.revokeCalls
	admin.mu.Unlock()
	hostile := exchangeFrame(handler, retirementFrame(
		t, "scheduled-public-stun", "release", testRevokeRequestID, envelope.Lease.LeaseID,
	))
	if hostile.Code != http.StatusConflict {
		t.Fatalf("swapped retirement ID status=%d", hostile.Code)
	}
	admin.mu.Lock()
	adminCallsAfterHostile := admin.releaseCalls + admin.revokeCalls
	admin.mu.Unlock()
	if adminCallsAfterHostile != adminCallsBeforeHostile {
		t.Fatal("hostile retirement reached the control authority")
	}
	release := retirementFrame(
		t, "scheduled-public-stun", "release", testReleaseRequestID, envelope.Lease.LeaseID,
	)
	retired := exchangeFrame(handler, release)
	replayed := exchangeFrame(handler, release)
	if retired.Code != http.StatusOK || !bytes.Equal(retired.Body.Bytes(), replayed.Body.Bytes()) {
		t.Fatal("terminal retirement was not exactly replayable")
	}
	metadata, payload, err := splitFrame(retired.Body.Bytes())
	if err != nil || len(payload) != 0 {
		t.Fatal("terminal receipt framing is invalid")
	}
	var receipt receiptEnvelope
	if !decodeCanonicalMetadata(metadata, &receipt) ||
		receipt.Receipt.ReleaseRequestID != testReleaseRequestID ||
		receipt.Receipt.RevokeRequestID != testRevokeRequestID ||
		receipt.Receipt.TURNTerminal != "not-required" {
		t.Fatal("terminal receipt lost the preallocated retirement pair")
	}
	admin.mu.Lock()
	releaseCalls := admin.releaseCalls
	admin.mu.Unlock()
	if releaseCalls != 1 {
		t.Fatalf("terminal replay called admin %d times", releaseCalls)
	}
	if err := handler.CloseAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := handler.CloseAndWait(context.Background()); err != nil {
		t.Fatal("idempotent close failed", err)
	}
}

func TestHandlerDynamicTURNIsBoundAndEarlyRevoked(t *testing.T) {
	now := time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)
	admin := newBrokerTestAdmin(now)
	provider := &brokerTestTURNProvider{}
	handler := newBrokerTestHandler(
		t, "scheduled-coturn", admin, provider, &acceptingIdentityValidator{},
	)
	frame := acquireFrame(t, "scheduled-coturn")
	response := exchangeFrame(handler, frame)
	replay := exchangeFrame(handler, frame)
	if replay.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), replay.Body.Bytes()) {
		t.Fatal("dynamic acquire replay changed the exact signed response")
	}
	envelope, controlCredential := decodeLeaseResponse(t, response)
	defer erase(controlCredential)
	if envelope.Lease.TURNCapability != "bound" ||
		envelope.Lease.TURNProviderLeaseID != "provider-lease-00000001" ||
		envelope.Lease.TURNCredentialID != "dynamic-turn-credential" ||
		envelope.Lease.TURNExpiresAt != envelope.Lease.ExpiresAt ||
		bytes.Contains(response.Body.Bytes(), []byte(testTURNSecret)) {
		t.Fatal("combined control/TURN lease is not an authenticated disjoint capability")
	}
	provider.mu.Lock()
	activeBeforeRetirement := provider.active
	providerAcquires := provider.acquires
	providerBinds := provider.binds
	provider.mu.Unlock()
	if !activeBeforeRetirement || providerAcquires != 1 || providerBinds != 1 {
		t.Fatal("provider did not activate the exact bound lease")
	}
	retired := exchangeFrame(handler, retirementFrame(
		t, "scheduled-coturn", "revoke-and-wait", testRevokeRequestID, envelope.Lease.LeaseID,
	))
	if retired.Code != http.StatusOK {
		t.Fatalf("TURN retirement status=%d", retired.Code)
	}
	provider.mu.Lock()
	activeAfterRetirement := provider.active
	revocations := provider.revocations
	provider.mu.Unlock()
	if activeAfterRetirement || revocations != 1 {
		t.Fatal("provider early revocation was not proven")
	}
}

func TestHandlerReleaseKeepsProviderUntilControlReapProof(t *testing.T) {
	now := time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)
	admin := newBrokerTestAdmin(now)
	admin.releaseErr = errors.New("control lease owns an active attempt")
	provider := &brokerTestTURNProvider{}
	handler := newBrokerTestHandler(
		t, "scheduled-coturn", admin, provider, &acceptingIdentityValidator{},
	)
	acquired := exchangeFrame(handler, acquireFrame(t, "scheduled-coturn"))
	envelope, controlCredential := decodeLeaseResponse(t, acquired)
	erase(controlCredential)
	release := exchangeFrame(handler, retirementFrame(
		t, "scheduled-coturn", "release", testReleaseRequestID, envelope.Lease.LeaseID,
	))
	if release.Code != http.StatusServiceUnavailable {
		t.Fatalf("active-attempt release status=%d", release.Code)
	}
	provider.mu.Lock()
	activeAfterRejectedRelease := provider.active
	revocationsAfterRejectedRelease := provider.revocations
	provider.mu.Unlock()
	if !activeAfterRejectedRelease || revocationsAfterRejectedRelease != 0 {
		t.Fatal("provider was revoked before the Pion control tree proved terminal")
	}
	revoked := exchangeFrame(handler, retirementFrame(
		t, "scheduled-coturn", "revoke-and-wait", testRevokeRequestID, envelope.Lease.LeaseID,
	))
	if revoked.Code != http.StatusOK {
		t.Fatalf("escalated revoke status=%d", revoked.Code)
	}
	provider.mu.Lock()
	activeAfterRevoke := provider.active
	revocationsAfterRevoke := provider.revocations
	provider.mu.Unlock()
	if activeAfterRevoke || revocationsAfterRevoke != 1 {
		t.Fatal("provider did not retire after exact control reap proof")
	}
}

func TestHandlerRejectsMalformedHTTPBeforeDependencies(t *testing.T) {
	now := time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)
	for _, mutate := range []func(*http.Request){
		func(request *http.Request) { request.Host = "hostile.example" },
		func(request *http.Request) {
			request.URL.RawPath = "/v2/credential%2Dbroker"
			request.RequestURI = request.URL.RawPath
		},
		func(request *http.Request) { request.URL.ForceQuery = true; request.RequestURI += "?" },
		func(request *http.Request) { request.URL.Scheme = "https"; request.URL.Host = "broker.example" },
		func(request *http.Request) { request.Header.Add("Content-Type", BrokerContentType) },
		func(request *http.Request) { request.Header.Set("Content-Encoding", "gzip") },
		func(request *http.Request) { request.TransferEncoding = []string{"chunked"} },
	} {
		admin := newBrokerTestAdmin(now)
		validator := &acceptingIdentityValidator{}
		handler := newBrokerTestHandler(t, "scheduled-public-stun", admin, nil, validator)
		frame := acquireFrame(t, "scheduled-public-stun")
		request := httptest.NewRequest(http.MethodPost, BrokerPath, bytes.NewReader(frame))
		request.Host = "broker.example"
		request.TLS = &tls.ConnectionState{}
		request.Header.Set("Content-Type", BrokerContentType)
		request.Header.Set("Accept", BrokerContentType)
		mutate(request)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		admin.mu.Lock()
		adminCalls := admin.acquireCalls + admin.bindCalls + admin.releaseCalls + admin.revokeCalls
		admin.mu.Unlock()
		if response.Code != http.StatusBadRequest || adminCalls != 0 || validator.callCount() != 0 {
			t.Fatalf("malformed request dispatched: status=%d admin=%d oidc=%d", response.Code, adminCalls, validator.callCount())
		}
	}
}

func TestHandlerFailedCloseRetainsAndRetriesOwnership(t *testing.T) {
	now := time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)
	admin := newBrokerTestAdmin(now)
	provider := &brokerTestTURNProvider{revokeErr: errors.New("provider unavailable")}
	handler := newBrokerTestHandler(
		t, "scheduled-coturn", admin, provider, &acceptingIdentityValidator{},
	)
	if response := exchangeFrame(handler, acquireFrame(t, "scheduled-coturn")); response.Code != http.StatusOK {
		t.Fatalf("acquire status=%d", response.Code)
	}
	if err := handler.ForceCloseAndWait(context.Background()); err == nil {
		t.Fatal("close settled without provider terminal proof")
	}
	provider.mu.Lock()
	provider.revokeErr = nil
	provider.mu.Unlock()
	if err := handler.ForceCloseAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := handler.CloseAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHandlerFailedAcquisitionRetiresControlBeforeTURNAndRetries(t *testing.T) {
	now := time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)
	admin := newBrokerTestAdmin(now)
	admin.acquireErr = errors.New("control authority failed after materialization")
	admin.revokeErr = errors.New("control authority retirement unavailable")
	provider := &brokerTestTURNProvider{}
	handler := newBrokerTestHandler(
		t, "scheduled-coturn", admin, provider, &acceptingIdentityValidator{},
	)

	first := exchangeFrame(handler, acquireFrame(t, "scheduled-coturn"))
	if first.Code != http.StatusConflict {
		t.Fatalf("partially materialized acquisition status=%d", first.Code)
	}
	provider.mu.Lock()
	providerRevocations := provider.revocations
	provider.mu.Unlock()
	if providerRevocations != 0 {
		t.Fatal("TURN was retired before control authority proved the failed acquisition terminal")
	}

	admin.mu.Lock()
	admin.revokeErr = nil
	admin.mu.Unlock()
	retry := exchangeFrame(handler, acquireFrame(t, "scheduled-coturn"))
	if retry.Code != http.StatusConflict {
		t.Fatalf("failed acquisition replay status=%d", retry.Code)
	}
	admin.mu.Lock()
	controlRevocations := admin.revokeCalls
	admin.mu.Unlock()
	provider.mu.Lock()
	providerRevocations = provider.revocations
	providerAcquires := provider.acquires
	providerBinds := provider.binds
	provider.mu.Unlock()
	if controlRevocations != 2 || providerRevocations != 1 || providerAcquires != 1 || providerBinds != 0 {
		t.Fatalf(
			"failed acquisition cleanup control=%d turn=%d acquires=%d binds=%d",
			controlRevocations, providerRevocations, providerAcquires, providerBinds,
		)
	}
	if err := handler.CloseAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func mustCanonicalJSON(t *testing.T, value any) []byte {
	t.Helper()
	document, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return document
}
