package browsermatrixpion

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestServiceSignsStableTerminalReceiptWithinExactAttemptAuthority(t *testing.T) {
	harness := newServiceHarness(t)
	defer func() {
		if err := harness.service.Close(); err != nil {
			t.Error(err)
		}
	}()
	created := createAttempt(t, harness, matrixRequestID(80))
	issuedAt, err := parseCanonicalTimestamp(created.LeaseIssuedAt)
	if err != nil {
		t.Fatal(err)
	}
	expiresAt, err := parseCanonicalTimestamp(created.LeaseExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if created.LeaseMillis != 5_000 ||
		expiresAt.Sub(issuedAt) != time.Duration(created.LeaseMillis)*time.Millisecond ||
		created.LeaseIssuedAt != harness.clock.Now().UTC().Truncate(time.Millisecond).Format(canonicalTimestampLayout) {
		t.Fatalf("create-attempt lease tuple is not exact: %#v", created)
	}

	pair := testSelectedPairEvidence()
	attempt := harness.factory.created[0]
	attempt.mu.Lock()
	attempt.result.State = attemptStateEstablished
	attempt.result.SelectedPair = &pair
	attempt.result.FailureCode = nil
	attempt.mu.Unlock()

	first := serveRequest(
		t, harness.service, "GET", attemptsPath+"/"+created.AttemptAuthority.AttemptID, nil, testCredential,
	)
	if first.Code != 200 {
		t.Fatalf("terminal result status=%d body=%s", first.Code, first.Body.String())
	}
	var result AttemptResult
	if err := json.Unmarshal(first.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.TerminalReceipt == nil || result.State != attemptStateEstablished ||
		result.SelectedPair == nil || !reflect.DeepEqual(*result.SelectedPair, pair) {
		t.Fatalf("established result lacks exact terminal evidence: %s", first.Body.String())
	}

	authorityResponse := authorityResponseForBinding(
		t,
		harness.service,
		attemptBindingFromAuthority(created.AttemptAuthority),
	)
	verifiedAuthority, err := VerifyAuthorityProbeResponse(
		authorityResponse,
		testAttestationPrivateKey().Public().(ed25519.PublicKey),
		harness.clock.Now().UTC(),
		authorityResponse.Attestation.RunID,
		authorityResponse.Attestation.Nonce,
	)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := VerifyAttemptTerminalReceipt(
		*result.TerminalReceipt,
		testAttestationPrivateKey().Public().(ed25519.PublicKey),
		verifiedAuthority,
		created,
	)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.State != result.State ||
		!reflect.DeepEqual(receipt.SelectedPair, result.SelectedPair) ||
		receipt.ChallengeBindingSHA256 != result.ChallengeBindingSHA256 ||
		receipt.TerminalAt != harness.clock.Now().UTC().Truncate(time.Millisecond).Format(canonicalTimestampLayout) {
		t.Fatal("signed receipt differs from the terminal result it authenticates")
	}
	assertTerminalReceiptCanonicalFieldOrder(t, receipt)

	second := serveRequest(
		t, harness.service, "GET", attemptsPath+"/"+created.AttemptAuthority.AttemptID, nil, testCredential,
	)
	var repeated AttemptResult
	if second.Code != 200 || json.Unmarshal(second.Body.Bytes(), &repeated) != nil ||
		!reflect.DeepEqual(repeated.TerminalReceipt, result.TerminalReceipt) {
		t.Fatalf("terminal receipt was not stable across reads: %s", second.Body.String())
	}
}

func TestReapedAttemptTombstoneReplaysLostSignedTerminalResult(t *testing.T) {
	clock := &fakeLeaseClock{now: time.Now().UTC()}
	factory := &fakeAttemptFactory{}
	config := completeServiceConfig(factory, &fakeProber{}, clock)
	config.TombstoneRetention = time.Millisecond
	service, err := NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	harness := serviceHarness{service: service, factory: factory, clock: clock}
	created := createAttempt(t, harness, matrixRequestID(84))
	pair := testSelectedPairEvidence()
	factory.created[0].mu.Lock()
	factory.created[0].result.State = attemptStateEstablished
	factory.created[0].result.SelectedPair = &pair
	factory.created[0].mu.Unlock()

	first := serveRequest(
		t, service, "GET", attemptsPath+"/"+created.AttemptAuthority.AttemptID, nil, testCredential,
	)
	var issued AttemptResult
	if first.Code != 200 || json.Unmarshal(first.Body.Bytes(), &issued) != nil ||
		issued.TerminalReceipt == nil {
		t.Fatalf("terminal result was not signed: status=%d body=%s", first.Code, first.Body.String())
	}
	deleted := serveRequest(
		t, service, "DELETE", attemptsPath+"/"+created.AttemptAuthority.AttemptID, nil, testCredential,
	)
	if deleted.Code != 200 || factory.created[0].closeCountValue() != 1 {
		t.Fatalf("terminal attempt was not reaped: status=%d closes=%d",
			deleted.Code, factory.created[0].closeCountValue())
	}
	clock.mu.Lock()
	clock.now = clock.now.Add(time.Second)
	clock.mu.Unlock()

	replayed := serveRequest(
		t, service, "GET", attemptsPath+"/"+created.AttemptAuthority.AttemptID, nil, testCredential,
	)
	var replayResult AttemptResult
	if replayed.Code != 200 || json.Unmarshal(replayed.Body.Bytes(), &replayResult) != nil ||
		!reflect.DeepEqual(replayResult, issued) || replayed.Body.String() != first.Body.String() {
		t.Fatalf("reaped tombstone did not replay the exact signed terminal result: %s", replayed.Body.String())
	}

	clock.mu.Lock()
	clock.now = clock.now.Add(time.Minute)
	clock.mu.Unlock()
	pruned := serveRequest(
		t, service, "GET", attemptsPath+"/"+created.AttemptAuthority.AttemptID, nil, testCredential,
	)
	if pruned.Code != 404 {
		t.Fatalf("terminal result outlived authority-derived retention: status=%d", pruned.Code)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestServiceSignsFailedAttemptTerminalReceipt(t *testing.T) {
	harness := newServiceHarness(t)
	defer func() {
		if err := harness.service.Close(); err != nil {
			t.Error(err)
		}
	}()
	created := createAttempt(t, harness, matrixRequestID(81))
	failureCode := "ice-failed"
	attempt := harness.factory.created[0]
	attempt.mu.Lock()
	attempt.result.State = attemptStateFailed
	attempt.result.SelectedPair = nil
	attempt.result.FailureCode = &failureCode
	attempt.mu.Unlock()

	response := serveRequest(
		t, harness.service, "GET", attemptsPath+"/"+created.AttemptAuthority.AttemptID, nil, testCredential,
	)
	var result AttemptResult
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil ||
		result.TerminalReceipt == nil || result.SelectedPair != nil ||
		result.FailureCode == nil || *result.FailureCode != failureCode ||
		result.TerminalReceipt.Receipt.FailureCode == nil ||
		*result.TerminalReceipt.Receipt.FailureCode != failureCode {
		t.Fatalf("failed attempt lacks a signed terminal receipt: %s", response.Body.String())
	}
}

func TestServiceFailsClosedWhenTerminalResultCannotBeSigned(t *testing.T) {
	harness := newServiceHarness(t)
	defer func() {
		if err := harness.service.Close(); err != nil {
			t.Error(err)
		}
	}()
	created := createAttempt(t, harness, matrixRequestID(83))
	attempt := harness.factory.created[0]
	attempt.mu.Lock()
	attempt.result.State = attemptStateEstablished
	attempt.result.SelectedPair = nil
	attempt.result.FailureCode = nil
	attempt.mu.Unlock()

	response := serveRequest(
		t, harness.service, "GET", attemptsPath+"/"+created.AttemptAuthority.AttemptID, nil, testCredential,
	)
	if response.Code != 500 ||
		strings.Contains(response.Body.String(), "terminalReceipt") ||
		attempt.closeCountValue() != 1 {
		t.Fatalf("unsigned terminal result escaped containment: status=%d body=%s closes=%d",
			response.Code, response.Body.String(), attempt.closeCountValue())
	}
	if repeated := serveRequest(
		t, harness.service, "GET", attemptsPath+"/"+created.AttemptAuthority.AttemptID, nil, testCredential,
	); repeated.Code != 404 {
		t.Fatalf("invalid terminal attempt remained active: %d", repeated.Code)
	}

	entry := &leasedAttempt{
		authorityIssuedAt:  harness.clock.Now().Add(-time.Minute),
		authorityExpiresAt: harness.clock.Now(),
	}
	if _, err := harness.service.signAttemptTerminalResult(
		entry,
		AttemptResult{State: attemptStateFailed},
		entry.authorityExpiresAt,
	); err == nil {
		t.Fatal("terminal signer accepted authority expiry equality")
	}
}

func TestAttemptTerminalReceiptRejectsTamperingAndSignedExpiredTerminalTime(t *testing.T) {
	harness := newServiceHarness(t)
	defer func() {
		if err := harness.service.Close(); err != nil {
			t.Error(err)
		}
	}()
	created := createAttempt(t, harness, matrixRequestID(82))
	failureCode := "ice-failed"
	attempt := harness.factory.created[0]
	attempt.mu.Lock()
	attempt.result.State = attemptStateFailed
	attempt.result.FailureCode = &failureCode
	attempt.mu.Unlock()
	response := serveRequest(
		t, harness.service, "GET", attemptsPath+"/"+created.AttemptAuthority.AttemptID, nil, testCredential,
	)
	var result AttemptResult
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil ||
		result.TerminalReceipt == nil {
		t.Fatalf("terminal result status=%d body=%s", response.Code, response.Body.String())
	}
	authorityResponse := authorityResponseForBinding(
		t,
		harness.service,
		attemptBindingFromAuthority(created.AttemptAuthority),
	)
	verifiedAuthority, err := VerifyAuthorityProbeResponse(
		authorityResponse,
		testAttestationPrivateKey().Public().(ed25519.PublicKey),
		harness.clock.Now().UTC(),
		authorityResponse.Attestation.RunID,
		authorityResponse.Attestation.Nonce,
	)
	if err != nil {
		t.Fatal(err)
	}
	publicKey := testAttestationPrivateKey().Public().(ed25519.PublicKey)

	tampered := cloneSignedAttemptTerminalReceipt(*result.TerminalReceipt)
	terminalAt, _ := parseCanonicalTimestamp(tampered.Receipt.TerminalAt)
	tampered.Receipt.TerminalAt = terminalAt.Add(time.Millisecond).Format(canonicalTimestampLayout)
	if _, err := VerifyAttemptTerminalReceipt(tampered, publicKey, verifiedAuthority, created); err == nil {
		t.Fatal("tampered terminal receipt retained authority")
	}

	expired := cloneSignedAttemptTerminalReceipt(*result.TerminalReceipt)
	attemptExpiresAt, _ := parseCanonicalTimestamp(expired.Receipt.AttemptLeaseExpiresAt)
	expired.Receipt.TerminalAt = attemptExpiresAt.Add(time.Millisecond).Format(canonicalTimestampLayout)
	document, err := canonicalJSONLine(expired.Receipt)
	if err != nil {
		t.Fatal(err)
	}
	expired.ReceiptSHA256 = sha256Hex(document)
	expired.Signature = base64.RawURLEncoding.EncodeToString(
		ed25519.Sign(testAttestationPrivateKey(), document),
	)
	if _, err := VerifyAttemptTerminalReceipt(expired, publicKey, verifiedAuthority, created); err == nil {
		t.Fatal("valid signature authorized a terminal event after the attempt lease")
	}
}

func authorityResponseForBinding(
	t *testing.T,
	service *Service,
	binding AttemptBinding,
) AuthorityProbeResponse {
	t.Helper()
	service.mu.Lock()
	defer service.mu.Unlock()
	lease, exists := service.authorityLeases[binding.FixtureBinding.AttestationSHA256]
	if !exists || lease.binding != binding {
		t.Fatal("test authority binding is not active")
	}
	return cloneAuthorityProbeResponse(lease.response)
}

func testSelectedPairEvidence() SelectedPairEvidence {
	return SelectedPairEvidence{
		Local: CandidateEvidence{
			CandidateType: "host",
			Protocol:      "udp",
			Address:       "192.0.2.10",
			Port:          41_000,
			AddressFamily: "ipv4",
		},
		Remote: CandidateEvidence{
			CandidateType: "srflx",
			Protocol:      "udp",
			Address:       "198.51.100.20",
			Port:          42_000,
			AddressFamily: "ipv4",
		},
	}
}

func assertTerminalReceiptCanonicalFieldOrder(t *testing.T, receipt AttemptTerminalReceipt) {
	t.Helper()
	document, err := CanonicalAttemptTerminalReceiptDocument(receipt)
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{
		"protocolVersion", "attemptAuthority", "terminalAt",
		"attemptLeaseIssuedAt", "attemptLeaseExpiresAt",
		"attemptLeaseMillis", "state", "selectedPair",
		"challengeBindingSha256", "failureCode",
	}
	previous := -1
	for _, key := range keys {
		position := strings.Index(string(document), "\""+key+"\":")
		if position <= previous {
			t.Fatalf("terminal receipt field %q is out of canonical order: %s", key, document)
		}
		previous = position
	}
}
