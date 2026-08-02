package browsermatrixpion

import (
	"bytes"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestNewServiceRejectsIncompleteAuthority(t *testing.T) {
	base := completeServiceConfig(&fakeAttemptFactory{}, &fakeProber{}, &fakeLeaseClock{now: time.Now().UTC()})
	base.MaximumActive = 2
	base.MaximumTombstones = 4
	tests := map[string]func(*ServiceConfig){
		"instance":        func(config *ServiceConfig) { config.Fixture.RemoteServiceInstanceID = "INVALID" },
		"profile":         func(config *ServiceConfig) { config.Fixture.ProfileID = "unknown" },
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
