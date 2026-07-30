package browsermatrixpion

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

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
