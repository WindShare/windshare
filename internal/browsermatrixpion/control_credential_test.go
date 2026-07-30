package browsermatrixpion

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestControlCredentialLeasePublishesExactNonceBoundDigest(t *testing.T) {
	harness := newServiceHarness(t)
	request := newTestAuthorityProbeRequest(
		t, harness.service, "credential-digest-run", "credential-probe-nonce", 30_000,
	)
	lease := acquireTestControlLease(t, harness.service, &request)
	encodedLease, err := json.Marshal(lease)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encodedLease, lease.Credential) ||
		!bytes.Contains(encodedLease, []byte("\"probeNonce\":\""+request.Nonce+"\"")) ||
		!bytes.Contains(encodedLease, []byte("\"requestId\":\"acquire-"+request.Nonce+"\"")) {
		t.Fatal("control lease serialization leaked its credential or omitted transaction identity")
	}
	if lease.RunID != request.ControlAuthority.SampleAuthority.RunID ||
		lease.RequestID != "acquire-"+request.Nonce ||
		lease.ProfileID != harness.service.profileID ||
		lease.ProbeNonce != request.Nonce ||
		lease.AuthorityInstanceID != harness.service.fixture.AuthorityInstanceID ||
		lease.MaxAttempts != 1 {
		t.Fatal("control credential lease metadata is not transaction-bound")
	}

	response := serveControlLeaseRequest(
		t,
		harness.service,
		http.MethodPost,
		authorityProbePath,
		request,
		lease,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("authority probe status=%d", response.Code)
	}
	var signed AuthorityProbeResponse
	if err := json.Unmarshal(response.Body.Bytes(), &signed); err != nil {
		t.Fatal(err)
	}
	document, err := CanonicalLiveAttestationDocument(signed.Attestation)
	if err != nil {
		t.Fatal(err)
	}
	if lease.AttestationSHA256 != signed.AttestationSHA256 ||
		lease.AttestationSHA256 != sha256Hex(document) {
		t.Fatal("control lease did not predict the exact signed attestation digest")
	}
	if _, err := VerifyAuthorityProbeResponse(
		signed,
		testAttestationPrivateKey().Public().(ed25519.PublicKey),
		harness.clock.Now().Add(time.Second),
		request.ControlAuthority.SampleAuthority.RunID,
		request.Nonce,
	); err != nil {
		t.Fatal(err)
	}

	receipt, err := harness.service.ReleaseControlCredential(lease.LeaseID)
	if err != nil || receipt.LeaseID != lease.LeaseID || receipt.Terminal != "revoked" {
		t.Fatalf("control credential release did not prove revocation: %#v err=%v", receipt, err)
	}
	erasedCredential := append([]byte(nil), lease.Credential...)
	eraseCredentialBytes(lease.Credential)
	rejected := serveRequest(
		t,
		harness.service,
		http.MethodGet,
		attemptsPath+"/aaaaaaaaaaaaaaaa",
		nil,
		string(erasedCredential),
	)
	eraseCredentialBytes(erasedCredential)
	if rejected.Code != http.StatusUnauthorized {
		t.Fatalf("released credential remained authorized: %d", rejected.Code)
	}
}

func TestControlCredentialAcquireReplaysOneActivePhysicalClaim(t *testing.T) {
	harness := newServiceHarness(t)
	request := ControlCredentialAcquireRequest{
		RequestID:            "credential-acquire-replay-request",
		RunID:                "credential-acquire-replay-run",
		ProfileID:            harness.service.profileID,
		ProbeNonce:           "credential-acquire-replay-nonce",
		RequestedLeaseMillis: 30_000,
	}
	first, err := harness.service.AcquireControlCredential(request)
	if err != nil {
		t.Fatal(err)
	}
	defer eraseCredentialBytes(first.Credential)
	replayed, err := harness.service.AcquireControlCredential(request)
	if err != nil {
		t.Fatal(err)
	}
	firstMetadata, firstMetadataErr := json.Marshal(first)
	replayedMetadata, replayedMetadataErr := json.Marshal(replayed)
	if firstMetadataErr != nil || replayedMetadataErr != nil ||
		!bytes.Equal(firstMetadata, replayedMetadata) ||
		!bytes.Equal(first.Credential, replayed.Credential) {
		t.Fatalf("active acquire replay changed its physical claim: first=%s replay=%s", first, replayed)
	}
	eraseCredentialBytes(replayed.Credential)
	if bytes.Equal(first.Credential, replayed.Credential) ||
		!harness.service.controlCredentials.Authenticate(first.LeaseID, string(first.Credential)) {
		t.Fatal("acquire replay did not return a credential copy independent from authority ownership")
	}
	harness.service.controlCredentials.mu.Lock()
	activeClaims := len(harness.service.controlCredentials.claimsByLeaseID)
	replayClaims := len(harness.service.controlCredentials.acquireReplaysByRequestID)
	harness.service.controlCredentials.mu.Unlock()
	if activeClaims != 1 || replayClaims != 1 {
		t.Fatalf("acquire replay allocated duplicate authority: active=%d replays=%d", activeClaims, replayClaims)
	}

	changed := request
	changed.RequestedLeaseMillis++
	if lease, changedErr := harness.service.AcquireControlCredential(changed); changedErr == nil {
		eraseCredentialBytes(lease.Credential)
		t.Fatal("acquire request ID was rebound to a different scope")
	}
	if _, err := harness.service.ReleaseControlCredential(first.LeaseID); err != nil {
		t.Fatal(err)
	}
	if lease, retiredErr := harness.service.AcquireControlCredential(request); retiredErr == nil {
		eraseCredentialBytes(lease.Credential)
		t.Fatal("retired acquire request reallocated an erased credential")
	}
}

func TestControlCredentialProbeNonceMismatchBurnsClaim(t *testing.T) {
	harness := newServiceHarness(t)
	request := newTestAuthorityProbeRequest(
		t, harness.service, "credential-mismatch-run", "credential-expected-nonce", 30_000,
	)
	lease := acquireTestControlLease(t, harness.service, &request)
	mismatched := request
	mismatched.Nonce = "credential-hostile-nonce"
	first := serveControlLeaseRequest(
		t,
		harness.service,
		http.MethodPost,
		authorityProbePath,
		mismatched,
		lease,
	)
	if first.Code != http.StatusConflict {
		t.Fatalf("nonce mismatch status=%d", first.Code)
	}
	reused := serveControlLeaseRequest(
		t,
		harness.service,
		http.MethodPost,
		authorityProbePath,
		request,
		lease,
	)
	eraseCredentialBytes(lease.Credential)
	if reused.Code != http.StatusConflict {
		t.Fatalf("mismatched one-shot claim was reusable: %d", reused.Code)
	}
}

func TestControlCredentialProbeRejectsClaimDigestMismatch(t *testing.T) {
	harness := newServiceHarness(t)
	request := newTestAuthorityProbeRequest(
		t, harness.service, "credential-tamper-run", "credential-tamper-nonce", 30_000,
	)
	lease := acquireTestControlLease(t, harness.service, &request)
	harness.service.controlCredentials.mu.Lock()
	claim := harness.service.controlCredentials.claimsByLeaseID[lease.LeaseID]
	claim.lease.AttestationSHA256 = strings.Repeat("f", 64)
	harness.service.controlCredentials.mu.Unlock()

	response := serveControlLeaseRequest(
		t,
		harness.service,
		http.MethodPost,
		authorityProbePath,
		request,
		lease,
	)
	eraseCredentialBytes(lease.Credential)
	if response.Code != http.StatusConflict {
		t.Fatalf("claim digest mismatch status=%d", response.Code)
	}
}

func TestControlCredentialRetirementWaitsForAuthorizedRequestsAndBurnsNonce(t *testing.T) {
	harness := newServiceHarness(t)
	request := ControlCredentialAcquireRequest{
		RequestID:            "credential-retirement-acquire",
		RunID:                "credential-retirement-run",
		ProfileID:            harness.service.profileID,
		ProbeNonce:           "credential-retirement-nonce",
		RequestedLeaseMillis: 30_000,
	}
	lease, err := harness.service.AcquireControlCredential(request)
	if err != nil {
		t.Fatal(err)
	}
	finishRequest, authorized := harness.service.controlCredentials.beginRequest(
		lease.LeaseID,
		string(lease.Credential),
	)
	if !authorized {
		t.Fatal("fresh control credential did not acquire request ownership")
	}
	if _, err := harness.service.ReleaseControlCredential(lease.LeaseID); err == nil {
		t.Fatal("graceful release ignored an in-flight authorized request")
	}

	type revokeResult struct {
		receipt ControlCredentialReceipt
		err     error
	}
	revoked := make(chan revokeResult, 1)
	go func() {
		receipt, revokeErr := harness.service.RevokeControlCredentialAndWait(lease.LeaseID)
		revoked <- revokeResult{receipt: receipt, err: revokeErr}
	}()
	deadline := time.Now().Add(time.Second)
	for {
		harness.service.controlCredentials.mu.Lock()
		claim := harness.service.controlCredentials.claimsByLeaseID[lease.LeaseID]
		revocationStarted := claim != nil && claim.revoked
		harness.service.controlCredentials.mu.Unlock()
		if revocationStarted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("forced revocation did not enter its wait state")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-revoked:
		t.Fatal("forced revocation returned before request ownership retired")
	default:
	}
	if harness.service.controlCredentials.Authenticate(lease.LeaseID, string(lease.Credential)) {
		t.Fatal("forced revocation admitted a new request while waiting")
	}
	finishRequest()
	result := <-revoked
	if result.err != nil || result.receipt.LeaseID != lease.LeaseID ||
		result.receipt.Terminal != "revoked" {
		t.Fatalf("forced revocation receipt=%#v err=%v", result.receipt, result.err)
	}
	eraseCredentialBytes(lease.Credential)

	if _, err := harness.service.AcquireControlCredential(request); err == nil {
		t.Fatal("retired one-shot probe nonce was reusable inside its original lease")
	}
	harness.clock.mu.Lock()
	harness.clock.now = harness.clock.now.Add(61 * time.Second)
	harness.clock.mu.Unlock()
	replacement, err := harness.service.AcquireControlCredential(request)
	if err != nil {
		t.Fatal(err)
	}
	eraseCredentialBytes(replacement.Credential)
}

func TestExpiringInFlightClaimPreservesForcedRetirementOwnership(t *testing.T) {
	harness := newServiceHarness(t)
	harness.service.controlCredentials.tombstoneRetention = time.Millisecond
	request := ControlCredentialAcquireRequest{
		RequestID:            "expiring-in-flight-acquire",
		RunID:                "expiring-in-flight-run",
		ProfileID:            harness.service.profileID,
		ProbeNonce:           "expiring-in-flight-probe-nonce",
		RequestedLeaseMillis: 2_000,
	}
	lease, err := harness.service.AcquireControlCredential(request)
	if err != nil {
		t.Fatal(err)
	}
	defer eraseCredentialBytes(lease.Credential)
	finishRequest, authorized := harness.service.controlCredentials.beginRequest(
		lease.LeaseID, string(lease.Credential),
	)
	if !authorized {
		t.Fatal("fresh control credential did not own its request")
	}
	if _, completed, err := harness.service.controlCredentials.beginRevocation(lease.LeaseID); err != nil || completed {
		t.Fatalf("forced retirement did not begin: completed=%t err=%v", completed, err)
	}

	harness.clock.mu.Lock()
	harness.clock.now = harness.clock.now.Add(3 * time.Second)
	harness.clock.mu.Unlock()
	finishRequest()
	if err := harness.service.controlCredentials.waitForRevocationRequests(lease.LeaseID); err != nil {
		t.Fatal(err)
	}
	// A completed HTTP owner is not the same as completed physical containment.
	// The revocation handoff therefore cannot age out before the service finishes it.
	harness.clock.mu.Lock()
	harness.clock.now = harness.clock.now.Add(time.Second)
	harness.clock.mu.Unlock()
	receipt, err := harness.service.controlCredentials.finishRevocation(lease.LeaseID)
	if err != nil || receipt.LeaseID != lease.LeaseID || receipt.Terminal != "revoked" {
		t.Fatalf("expired in-flight revocation lost ownership: receipt=%#v err=%v", receipt, err)
	}
}

func TestControlCredentialAuthorityRejectsDuplicateNonceAndWrongProfile(t *testing.T) {
	harness := newServiceHarness(t)
	request := ControlCredentialAcquireRequest{
		RequestID:            "credential-scope-acquire",
		RunID:                "credential-scope-run",
		ProfileID:            harness.service.profileID,
		ProbeNonce:           "credential-scope-nonce",
		RequestedLeaseMillis: 30_000,
	}
	lease, err := harness.service.AcquireControlCredential(request)
	if err != nil {
		t.Fatal(err)
	}
	defer eraseCredentialBytes(lease.Credential)
	request.RequestID = "credential-scope-duplicate-acquire"
	if _, err := harness.service.AcquireControlCredential(request); err == nil {
		t.Fatal("duplicate active probe nonce received a second credential")
	}
	request.RequestID = "credential-other-profile-acquire"
	request.ProbeNonce = "credential-other-nonce"
	request.ProfileID = "scheduled-coturn"
	if _, err := harness.service.AcquireControlCredential(request); err == nil {
		t.Fatal("credential authority accepted a different profile")
	}
}

func TestLostReleaseResponseReplaysThroughForcedRetirementAndPrunes(t *testing.T) {
	harness := newServiceHarness(t)
	authority := acquireTestDynamicControlAuthority(
		t, harness.service, "lost-release-run", "lost-release-probe-nonce",
	)

	released, err := harness.service.ReleaseControlCredential(authority.lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := harness.service.RevokeControlCredentialAndWait(authority.lease.LeaseID)
	if err != nil || replayed != released {
		t.Fatalf("lost release did not replay its exact terminal receipt: release=%#v replay=%#v err=%v",
			released, replayed, err)
	}
	harness.service.mu.Lock()
	retirement := harness.service.controlRevocations[authority.lease.LeaseID]
	retiring := harness.service.retiringControlLeases[authority.lease.LeaseID]
	harness.service.mu.Unlock()
	if retirement == nil || !retirement.completed || !retiring {
		t.Fatal("completed release did not retain its bounded replay authority")
	}

	harness.clock.mu.Lock()
	harness.clock.now = harness.clock.now.Add(2 * time.Minute)
	harness.clock.mu.Unlock()
	harness.service.mu.Lock()
	harness.service.pruneControlRetirementsLocked(harness.clock.Now().UTC())
	serviceRetirements := len(harness.service.controlRevocations)
	serviceMarkers := len(harness.service.retiringControlLeases)
	harness.service.mu.Unlock()
	harness.service.controlCredentials.mu.Lock()
	harness.service.controlCredentials.pruneLocked(harness.clock.Now().UTC())
	authorityRetirements := len(harness.service.controlCredentials.retirementsByLeaseID)
	harness.service.controlCredentials.mu.Unlock()
	if serviceRetirements != 0 || serviceMarkers != 0 || authorityRetirements != 0 {
		t.Fatalf("retirement replay maps did not prune: service=%d markers=%d authority=%d",
			serviceRetirements, serviceMarkers, authorityRetirements)
	}
}

func TestControlLeaseIDCannotBeReusedUntilRetirementRetentionExpires(t *testing.T) {
	clock := &fakeLeaseClock{now: time.Now().UTC()}
	config := completeServiceConfig(&fakeAttemptFactory{}, &fakeProber{}, clock)
	config.TombstoneRetention = 5 * time.Second
	reusedID := bytes.Repeat([]byte{0x7a}, controlLeaseIdentifierBytes)
	config.ControlLeaseIDSource = bytes.NewReader(bytes.Repeat(reusedID, 3))
	service, err := NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	request := ControlCredentialAcquireRequest{
		RequestID: "lease-id-collision-acquire-1",
		RunID:     "lease-id-collision-run", ProfileID: service.profileID,
		ProbeNonce: "lease-id-collision-nonce-1", RequestedLeaseMillis: 2_000,
	}
	first, err := service.AcquireControlCredential(request)
	if err != nil {
		t.Fatal(err)
	}
	defer eraseCredentialBytes(first.Credential)
	if _, err := service.ReleaseControlCredential(first.LeaseID); err != nil {
		t.Fatal(err)
	}
	clock.mu.Lock()
	clock.now = clock.now.Add(3 * time.Second)
	clock.mu.Unlock()
	request.ProbeNonce = "lease-id-collision-nonce-2"
	request.RequestID = "lease-id-collision-acquire-2"
	if collided, collisionErr := service.AcquireControlCredential(request); collisionErr == nil {
		eraseCredentialBytes(collided.Credential)
		t.Fatal("retired control lease ID was reused during retention")
	}

	clock.mu.Lock()
	clock.now = clock.now.Add(3 * time.Second)
	clock.mu.Unlock()
	request.ProbeNonce = "lease-id-collision-nonce-3"
	request.RequestID = "lease-id-collision-acquire-3"
	reused, err := service.AcquireControlCredential(request)
	if err != nil {
		t.Fatal(err)
	}
	defer eraseCredentialBytes(reused.Credential)
	if reused.LeaseID != first.LeaseID {
		t.Fatalf("post-retention source did not reuse the expected lease ID: %q != %q",
			reused.LeaseID, first.LeaseID)
	}
	service.mu.Lock()
	retirements, markers := len(service.controlRevocations), len(service.retiringControlLeases)
	service.mu.Unlock()
	if retirements != 0 || markers != 0 {
		t.Fatalf("lease ID reuse retained stale service ownership: retirements=%d markers=%d",
			retirements, markers)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestControlBearerNeverBecomesALookupDigestOrDiagnosticPayload(t *testing.T) {
	clock := &fakeLeaseClock{now: time.Now().UTC()}
	var traces []TraceEvent
	config := completeServiceConfig(&fakeAttemptFactory{}, &fakeProber{}, clock)
	config.Trace = func(event TraceEvent) { traces = append(traces, event) }
	service, err := NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	request := newTestAuthorityProbeRequest(
		t, service, "credential-freeze-run", "credential-freeze-probe-nonce", 30_000,
	)
	lease := acquireTestControlLease(t, service, &request)
	defer eraseCredentialBytes(lease.Credential)
	rawCredential := append([]byte(nil), lease.Credential...)
	nestedBase64 := base64.RawURLEncoding.EncodeToString(rawCredential)
	credentialDigest := fmt.Sprintf("%x", sha256.Sum256(rawCredential))
	service.controlCredentials.mu.Lock()
	ownedCredential := service.controlCredentials.claimsByLeaseID[lease.LeaseID].credential
	service.controlCredentials.mu.Unlock()

	missingLeaseID := serveRequest(
		t, service, http.MethodPost, authorityProbePath, request, string(lease.Credential),
	)
	if missingLeaseID.Code != http.StatusUnauthorized {
		t.Fatalf("dynamic bearer without its lease ID status=%d", missingLeaseID.Code)
	}
	probe := serveControlLeaseRequest(
		t, service, http.MethodPost, authorityProbePath, request, lease,
	)
	if probe.Code != http.StatusOK {
		t.Fatalf("lease-bound authority probe status=%d body=%s", probe.Code, probe.Body.String())
	}
	leaseJSON, err := json.Marshal(lease)
	if err != nil {
		t.Fatal(err)
	}
	traceJSON, err := json.Marshal(traces)
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile("control_credential.go")
	if err != nil {
		t.Fatal(err)
	}
	surfaces := [][]byte{
		leaseJSON,
		traceJSON,
		missingLeaseID.Body.Bytes(),
		probe.Body.Bytes(),
		[]byte(lease.String()),
		[]byte(service.String()),
		source,
	}
	for _, sentinel := range []string{string(rawCredential), nestedBase64, credentialDigest} {
		for _, surface := range surfaces {
			if bytes.Contains(surface, []byte(sentinel)) {
				t.Fatalf("credential-derived sentinel entered an observable surface")
			}
		}
	}
	for _, prohibitedSourcePattern := range []string{
		"claimsByCredential", "credentialByLeaseID", "sha256.Sum256(credential",
	} {
		if bytes.Contains(source, []byte(prohibitedSourcePattern)) {
			t.Fatalf("credential-derived lookup authority returned: %s", prohibitedSourcePattern)
		}
	}
	if _, err := service.ReleaseControlCredential(lease.LeaseID); err != nil {
		t.Fatal(err)
	}
	for _, value := range ownedCredential {
		if value != 0 {
			t.Fatal("retirement did not erase the authority-owned bearer")
		}
	}
	eraseCredentialBytes(rawCredential)
}
