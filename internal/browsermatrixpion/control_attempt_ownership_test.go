package browsermatrixpion

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

type testDynamicControlAuthority struct {
	lease   ControlCredentialLease
	binding AttemptBinding
}

func acquireTestDynamicControlAuthority(
	t *testing.T,
	service *Service,
	runID string,
	probeNonce string,
) testDynamicControlAuthority {
	return acquireTestDynamicControlAuthorityWithLease(
		t, service, runID, probeNonce, 60_000,
	)
}

func acquireTestDynamicControlAuthorityWithLease(
	t *testing.T,
	service *Service,
	runID string,
	probeNonce string,
	leaseMillis int64,
) testDynamicControlAuthority {
	t.Helper()
	request := newTestAuthorityProbeRequest(t, service, runID, probeNonce, leaseMillis)
	lease := acquireTestControlLease(t, service, &request)
	t.Cleanup(func() { eraseCredentialBytes(lease.Credential) })
	response := serveControlLeaseRequest(
		t,
		service,
		http.MethodPost,
		authorityProbePath,
		request,
		lease,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("dynamic authority probe status=%d body=%s", response.Code, response.Body.String())
	}
	var signed AuthorityProbeResponse
	if err := json.Unmarshal(response.Body.Bytes(), &signed); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	authority := service.authorityLeases[signed.AttestationSHA256]
	service.mu.Unlock()
	if authority.binding == (AttemptBinding{}) || authority.controlLeaseID != lease.LeaseID {
		t.Fatal("dynamic authority probe lost its control lease owner")
	}
	return testDynamicControlAuthority{lease: lease, binding: authority.binding}
}

func createDynamicAttemptRequest(
	t *testing.T,
	service *Service,
	authority testDynamicControlAuthority,
	requestID string,
) *httptest.ResponseRecorder {
	t.Helper()
	return serveControlLeaseRequest(
		t,
		service,
		http.MethodPost,
		attemptsPath,
		testCreateAttemptRequest(authority.binding, requestID, 5_000),
		authority.lease,
	)
}

func TestForcedCredentialRevocationReapsLeafAttemptBeforeReceiptAndReplays(t *testing.T) {
	harness := newServiceHarness(t)
	authority := acquireTestDynamicControlAuthority(
		t,
		harness.service,
		"leaf-killed-run",
		"leaf-killed-probe-nonce",
	)
	createdResponse := createDynamicAttemptRequest(
		t,
		harness.service,
		authority,
		matrixRequestID(90),
	)
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("dynamic create status=%d body=%s", createdResponse.Code, createdResponse.Body.String())
	}
	var created CreateAttemptResponse
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.service.ReleaseControlCredential(authority.lease.LeaseID); err == nil {
		t.Fatal("graceful credential release ignored its live Pion attempt")
	}

	receipt, err := harness.service.RevokeControlCredentialAndWait(authority.lease.LeaseID)
	if err != nil || receipt.LeaseID != authority.lease.LeaseID || receipt.Terminal != "revoked" {
		t.Fatalf("forced revocation receipt=%#v err=%v", receipt, err)
	}
	if harness.factory.created[0].closeCountValue() != 1 {
		t.Fatalf("forced revocation closes=%d", harness.factory.created[0].closeCountValue())
	}
	if response := serveRequest(
		t,
		harness.service,
		http.MethodGet,
		attemptsPath+"/"+created.AttemptAuthority.AttemptID,
		nil,
		testCredential,
	); response.Code != http.StatusNotFound {
		t.Fatalf("revoked leaf attempt remains visible: %d", response.Code)
	}

	replayed, replayErr := harness.service.RevokeControlCredentialAndWait(authority.lease.LeaseID)
	if replayErr != nil || !reflect.DeepEqual(replayed, receipt) ||
		harness.factory.created[0].closeCountValue() != 1 {
		t.Fatalf("revocation replay receipt=%#v err=%v closes=%d",
			replayed, replayErr, harness.factory.created[0].closeCountValue())
	}
	if err := harness.service.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDynamicControlLeaseAllowsExactCreateReplayButOnlyOneAttempt(t *testing.T) {
	harness := newServiceHarness(t)
	authority := acquireTestDynamicControlAuthority(
		t,
		harness.service,
		"single-attempt-run",
		"single-attempt-probe-nonce",
	)
	requestID := matrixRequestID(91)
	first := createDynamicAttemptRequest(t, harness.service, authority, requestID)
	if first.Code != http.StatusCreated {
		t.Fatalf("first create status=%d body=%s", first.Code, first.Body.String())
	}
	replay := createDynamicAttemptRequest(t, harness.service, authority, requestID)
	if replay.Code != http.StatusOK {
		t.Fatalf("exact create replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	var firstLease, replayLease CreateAttemptResponse
	if json.Unmarshal(first.Body.Bytes(), &firstLease) != nil ||
		json.Unmarshal(replay.Body.Bytes(), &replayLease) != nil ||
		!reflect.DeepEqual(firstLease, replayLease) {
		t.Fatal("exact create replay changed its attempt authority")
	}

	second := createDynamicAttemptRequest(
		t,
		harness.service,
		authority,
		matrixRequestID(92),
	)
	if second.Code != http.StatusConflict || len(harness.factory.created) != 1 {
		t.Fatalf("control lease created multiple attempts: status=%d attempts=%d",
			second.Code, len(harness.factory.created))
	}
	deleted := serveControlLeaseRequest(
		t,
		harness.service,
		http.MethodDelete,
		attemptsPath+"/"+firstLease.AttemptAuthority.AttemptID,
		nil,
		authority.lease,
	)
	if deleted.Code != http.StatusOK {
		t.Fatalf("attempt cleanup status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	afterCleanup := createDynamicAttemptRequest(
		t,
		harness.service,
		authority,
		matrixRequestID(93),
	)
	if afterCleanup.Code != http.StatusConflict || len(harness.factory.created) != 1 {
		t.Fatal("control lease regained attempt authority after cleanup")
	}
	receipt, err := harness.service.ReleaseControlCredential(authority.lease.LeaseID)
	if err != nil || receipt.Terminal != "revoked" {
		t.Fatalf("graceful terminal release receipt=%#v err=%v", receipt, err)
	}
	if err := harness.service.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDynamicAttemptTombstoneCannotExpireBeforeItsControlAuthority(t *testing.T) {
	clock := &fakeLeaseClock{now: time.Now().UTC()}
	factory := &fakeAttemptFactory{}
	config := completeServiceConfig(factory, &fakeProber{}, clock)
	config.TombstoneRetention = time.Millisecond
	service, err := NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	authority := acquireTestDynamicControlAuthority(
		t, service, "short-retention-run", "short-retention-probe-nonce",
	)
	requestID := matrixRequestID(96)
	createdResponse := createDynamicAttemptRequest(t, service, authority, requestID)
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("dynamic create status=%d body=%s", createdResponse.Code, createdResponse.Body.String())
	}
	var created CreateAttemptResponse
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	deleted := serveControlLeaseRequest(
		t, service, http.MethodDelete, attemptsPath+"/"+created.AttemptAuthority.AttemptID, nil, authority.lease,
	)
	if deleted.Code != http.StatusOK {
		t.Fatalf("dynamic cleanup status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	controlExpiresAt, err := parseCanonicalTimestamp(authority.lease.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	tombstone := service.tombstones[created.AttemptAuthority.AttemptID]
	service.mu.Unlock()
	if tombstone.requestID != requestID || tombstone.expiresAt.Before(controlExpiresAt) {
		t.Fatalf("attempt tombstone expires before its authority: tombstone=%s authority=%s",
			tombstone.expiresAt, controlExpiresAt)
	}

	clock.mu.Lock()
	clock.now = clock.now.Add(time.Second)
	clock.mu.Unlock()
	replayed := createDynamicAttemptRequest(t, service, authority, requestID)
	if replayed.Code != http.StatusConflict || len(factory.created) != 1 {
		t.Fatalf("short retention allocated a second physical attempt: status=%d attempts=%d",
			replayed.Code, len(factory.created))
	}
	service.controlCredentials.mu.Lock()
	claim := service.controlCredentials.claimsByLeaseID[authority.lease.LeaseID]
	service.controlCredentials.mu.Unlock()
	if claim == nil || claim.attemptRequestID != requestID {
		t.Fatal("one-shot attempt claim did not survive its live control authority")
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredControlClaimErasesBearerBeforeForcedAttemptContainment(t *testing.T) {
	clock := &fakeLeaseClock{now: time.Now().UTC()}
	factory := &fakeAttemptFactory{}
	config := completeServiceConfig(factory, &fakeProber{}, clock)
	config.TombstoneRetention = time.Millisecond
	service, err := NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	authority := acquireTestDynamicControlAuthorityWithLease(
		t, service, "expired-claim-run", "expired-claim-probe-nonce", 10_000,
	)
	created := createDynamicAttemptRequest(t, service, authority, matrixRequestID(97))
	if created.Code != http.StatusCreated {
		t.Fatalf("dynamic create status=%d body=%s", created.Code, created.Body.String())
	}
	service.controlCredentials.mu.Lock()
	ownedCredential := service.controlCredentials.claimsByLeaseID[authority.lease.LeaseID].credential
	service.controlCredentials.mu.Unlock()

	clock.mu.Lock()
	clock.now = clock.now.Add(11 * time.Second)
	clock.mu.Unlock()
	if service.controlCredentials.Authenticate(
		authority.lease.LeaseID,
		string(authority.lease.Credential),
	) {
		t.Fatal("expired credential remained authorized")
	}
	for _, value := range ownedCredential {
		if value != 0 {
			t.Fatal("expired credential owner was not erased")
		}
	}
	service.controlCredentials.mu.Lock()
	_, active := service.controlCredentials.claimsByLeaseID[authority.lease.LeaseID]
	retirement := service.controlCredentials.retirementsByLeaseID[authority.lease.LeaseID]
	service.controlCredentials.mu.Unlock()
	if active || retirement.completed {
		t.Fatal("expired bearer was not converted into a non-terminal burned claim")
	}

	receipt, err := service.RevokeControlCredentialAndWait(authority.lease.LeaseID)
	if err != nil || receipt.Terminal != "revoked" || len(factory.created) != 1 ||
		factory.created[0].closeCountValue() != 1 {
		t.Fatalf("expired control revocation did not contain its attempt: receipt=%#v err=%v", receipt, err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestForcedCredentialRevocationCancelsAndWaitsForAttemptStartup(t *testing.T) {
	clock := &fakeLeaseClock{now: time.Now().UTC()}
	factory := &gatedAttemptFactory{
		started:      make(chan string, 1),
		release:      make(chan struct{}),
		honorContext: true,
	}
	service, err := NewService(completeServiceConfig(factory, &fakeProber{}, clock))
	if err != nil {
		t.Fatal(err)
	}
	authority := acquireTestDynamicControlAuthority(
		t,
		service,
		"startup-revocation-run",
		"startup-revocation-nonce",
	)
	createDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		createDone <- createDynamicAttemptRequest(
			t,
			service,
			authority,
			matrixRequestID(94),
		)
	}()
	<-factory.started

	type revokeResult struct {
		receipt ControlCredentialReceipt
		err     error
	}
	revokeDone := make(chan revokeResult, 1)
	go func() {
		receipt, revokeErr := service.RevokeControlCredentialAndWait(authority.lease.LeaseID)
		revokeDone <- revokeResult{receipt: receipt, err: revokeErr}
	}()
	createResponse := <-createDone
	if createResponse.Code != http.StatusInternalServerError &&
		createResponse.Code != http.StatusConflict {
		t.Fatalf("revoked startup response=%d body=%s", createResponse.Code, createResponse.Body.String())
	}
	revoked := <-revokeDone
	if revoked.err != nil || revoked.receipt.Terminal != "revoked" {
		t.Fatalf("startup revocation receipt=%#v err=%v", revoked.receipt, revoked.err)
	}
	attempts := factory.createdAttempts()
	if len(attempts) != 1 || attempts[0].closeCountValue() != 1 {
		t.Fatalf("startup revocation retained Pion state: attempts=%d", len(attempts))
	}
	service.mu.Lock()
	owned := service.controlLeaseOwnsAttemptLocked(authority.lease.LeaseID)
	service.mu.Unlock()
	if owned {
		t.Fatal("startup revocation returned before registry ownership retired")
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestForcedCredentialRevocationWithCloseFailureNeverReturnsReceipt(t *testing.T) {
	harness := newServiceHarness(t)
	authority := acquireTestDynamicControlAuthority(
		t,
		harness.service,
		"close-failure-run",
		"close-failure-probe-nonce",
	)
	created := createDynamicAttemptRequest(
		t,
		harness.service,
		authority,
		matrixRequestID(95),
	)
	if created.Code != http.StatusCreated {
		t.Fatalf("dynamic create status=%d body=%s", created.Code, created.Body.String())
	}
	harness.factory.created[0].closeErr = errTestAttemptContainment

	receipt, err := harness.service.RevokeControlCredentialAndWait(authority.lease.LeaseID)
	if err == nil || receipt != (ControlCredentialReceipt{}) {
		t.Fatalf("containment failure published a retirement receipt: receipt=%#v err=%v", receipt, err)
	}
	replayed, replayErr := harness.service.RevokeControlCredentialAndWait(authority.lease.LeaseID)
	if replayErr == nil || replayed != (ControlCredentialReceipt{}) ||
		harness.factory.created[0].closeCountValue() != 1 {
		t.Fatalf("failed revocation replay receipt=%#v err=%v closes=%d",
			replayed, replayErr, harness.factory.created[0].closeCountValue())
	}
	if closeErr := harness.service.Close(); closeErr == nil {
		t.Fatal("service close forgot the control-owned containment failure")
	}
}

var errTestAttemptContainment = &testContainmentError{}

type testContainmentError struct{}

func (*testContainmentError) Error() string { return "test attempt containment failed" }
