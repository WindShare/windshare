package browsermatrixpion

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

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
		for index := range requestCount {
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
		for range requestCount {
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
