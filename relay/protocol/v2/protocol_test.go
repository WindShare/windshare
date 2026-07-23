package v2

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"testing"
	"time"
)

func testB64(t *testing.T, value string) []byte {
	t.Helper()
	result, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func testSequence(first byte, length int) []byte {
	result := make([]byte, length)
	for index := range result {
		result[index] = first + byte(index)
	}
	return result
}

func frozenRegistration(t *testing.T) (RegisterInit, Challenge, RelayIdentity, ed25519.PrivateKey) {
	t.Helper()
	var init RegisterInit
	init.Mode = RegistrationFresh
	copy(init.ShareID[:], testB64(t, "tVW+68OSeLBZTpU+"))
	copy(init.ShareInstance[:], testB64(t, "QEFCQ0RFRkdISUpLTE1OTw=="))
	copy(init.PKHash[:], testB64(t, "JEKgoDij26RUJt97fcJPxA=="))
	copy(init.DescriptorDigest[:], testB64(t, "On7QQd47GWSkm2d7BhQJ5XmC1Y4Xxtu4k+cCPMNKEZY="))
	copy(init.ResumeTokenHash[:], testB64(t, "y8MvQbuxtwSyAAZ4WakNTEN24ONLRmnUEgYX5VXgHiA="))
	var challenge Challenge
	challenge.Purpose = ChallengeRegister
	copy(challenge.ID[:], testB64(t, "AQIDBAUGBwgJCgsMDQ4PEA=="))
	copy(challenge.Nonce[:], testB64(t, "ISIjJCUmJygpKissLS4vMDEyMzQ1Njc4OTo7PD0+P0A="))
	challenge.ExpiresAtUnixSeconds = 1_700_000_030
	var relayIdentity RelayIdentity
	copy(relayIdentity[:], testB64(t, "lqw7a1WCyYfbtMfhCa0djaH+/mO59x3qwmV4Mgrmsh0="))
	privateKey := ed25519.NewKeyFromSeed(testSequence(0x20, ed25519.SeedSize))
	return init, challenge, relayIdentity, privateKey
}

func TestFreshRegistrationAndDescriptorDirectionsMatchFrozenVectors(t *testing.T) {
	init, challenge, relayIdentity, privateKey := frozenRegistration(t)
	initBytes, err := init.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if want := testB64(t, "V1MyUgIAAAC1Vb7rw5J4sFlOlT5AQUJDREVGR0hJSktMTU5PJEKgoDij26RUJt97fcJPxDp+0EHeOxlkpJtnewYUCeV5gtWOF8bbuJPnAjzDShGWy8MvQbuxtwSyAAZ4WakNTEN24ONLRmnUEgYX5VXgHiA="); !bytes.Equal(initBytes, want) {
		t.Fatal("REGISTER_INIT diverged from frozen vector")
	}
	challengeBytes, _ := challenge.MarshalBinary()
	if want := testB64(t, "V1MyUQIAAAABAgMEBQYHCAkKCwwNDg8QISIjJCUmJygpKissLS4vMDEyMzQ1Njc4OTo7PD0+P0AAAAAAZVPxHg=="); !bytes.Equal(challengeBytes, want) {
		t.Fatal("fresh challenge diverged from frozen vector")
	}
	preimage, err := RegistrationPreimage(init, challenge, relayIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if want := testB64(t, "d2luZHNoYXJlL3YyIHJlbGF5LXJlZ2lzdGVyALVVvuvDkniwWU6VPkBBQkNERUZHSElKS0xNTk8kQqCgOKPbpFQm33t9wk/EOn7QQd47GWSkm2d7BhQJ5XmC1Y4Xxtu4k+cCPMNKEZbLwy9Bu7G3BLIABnhZqQ1MQ3bg40tGadQSBhflVeAeIJasO2tVgsmH27TH4QmtHY2h/v5jufcd6sJleDIK5rIdAQIDBAUGBwgJCgsMDQ4PECEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj9AAAAAAGVT8R4="); !bytes.Equal(preimage, want) {
		t.Fatal("registration preimage diverged from frozen vector")
	}
	proof, err := NewRegisterProof(init, challenge, relayIdentity, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	proofBytes, _ := proof.MarshalBinary()
	if want := testB64(t, "V1MyUAIAAAAprLrhQbzK8LIuGpTTTQvHNh5SbQv+EsiXlLyTIpZt15V0JzXTUa/PIcjBj7iDpbBz0BLmKfRh1tda+jcvQLpTZsOJM/DUj88nARIOvEQD4mLjoo2ARiKCJ42qVjLXEgs="); !bytes.Equal(proofBytes, want) {
		t.Fatal("REGISTER_PROOF diverged from frozen vector")
	}
	authority, err := authenticateRegisterProof(init, challenge, relayIdentity, proof, time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}

	uploadBytes := testB64(t, "V1MyVQIAAAAAAADjAgAAAAAAAI/Q0dLT1NXW19jZ2ttZiFtVkBUbMjw7HI/q5B9qTw6bdNmlWAQtMgPxJGkRSmmZjl6KhYM8wRjPSNppp6y8l+oskLhTNjknPbPR41l3KLLCcl8a3QGnBZltpRP2erySHhoULzJzlDcEAkmQJyrASmgNQdVQSipSRia3l7yRnZAeFhColC/WYAO47TBO1eZro4Q2/eBvLZYFXoul3IZ12dbXljAhPMxure5bwD4bJzI+qSRmw1G6aZems+gCERr/qm4+5Vtdpve2Fmf2tbEqKTtppABJrqXNgEaKnQ4=")
	upload, err := ParseDescriptorUpload(uploadBytes)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := VerifyDescriptorUpload(init, authority, upload)
	if err != nil {
		t.Fatal(err)
	}
	if object, ok := verified.ObjectFor(init); !ok || !bytes.Equal(object, upload.Object) {
		t.Fatal("verified descriptor did not preserve its registration binding")
	}
	if encoded, err := upload.MarshalBinary(); err != nil || !bytes.Equal(encoded, uploadBytes) {
		t.Fatalf("WS2U byte replay changed: %v", err)
	}
	deliveryBytes := testB64(t, "V1MyRAIAAADh4uPk5ebn6AAAAOMCAAAAAAAAj9DR0tPU1dbX2Nna21mIW1WQFRsyPDscj+rkH2pPDpt02aVYBC0yA/EkaRFKaZmOXoqFgzzBGM9I2mmnrLyX6iyQuFM2OSc9s9HjWXcossJyXxrdAacFmW2lE/Z6vJIeGhQvMnOUNwQCSZAnKsBKaA1B1VBKKlJGJreXvJGdkB4WEKiUL9ZgA7jtME7V5mujhDb94G8tlgVei6XchnXZ1teWMCE8zG6t7lvAPhsnMj6pJGbDUbppl6az6AIRGv+qbj7lW12m97YWZ/a1sSopO2mkAEmupc2ARoqdDg==")
	delivery, err := ParseDescriptorDelivery(deliveryBytes)
	if err != nil || !bytes.Equal(delivery.Object, upload.Object) {
		t.Fatalf("WS2D parse: %v", err)
	}
	if encoded, err := delivery.MarshalBinary(); err != nil || !bytes.Equal(encoded, deliveryBytes) {
		t.Fatalf("WS2D byte replay changed: %v", err)
	}
	if _, err := ParseDescriptorDelivery(uploadBytes); !errors.Is(err, ErrMalformed) {
		t.Fatalf("WS2U accepted as WS2D: %v", err)
	}
	if _, err := ParseDescriptorUpload(deliveryBytes); !errors.Is(err, ErrMalformed) {
		t.Fatalf("WS2D accepted as WS2U: %v", err)
	}
}

func TestRegisterAndStopProofsRejectEveryIdentitySubstitution(t *testing.T) {
	init, challenge, relayIdentity, privateKey := frozenRegistration(t)
	proof, _ := NewRegisterProof(init, challenge, relayIdentity, privateKey)
	tests := []func(RegisterInit, Challenge, RelayIdentity, RegisterProof) error{
		func(value RegisterInit, c Challenge, relay RelayIdentity, p RegisterProof) error {
			value.ShareInstance[0] ^= 1
			_, err := authenticateRegisterProof(value, c, relay, p, time.Unix(1_700_000_000, 0))
			return err
		},
		func(value RegisterInit, c Challenge, relay RelayIdentity, p RegisterProof) error {
			value.DescriptorDigest[0] ^= 1
			_, err := authenticateRegisterProof(value, c, relay, p, time.Unix(1_700_000_000, 0))
			return err
		},
		func(value RegisterInit, c Challenge, relay RelayIdentity, p RegisterProof) error {
			c.Nonce[0] ^= 1
			_, err := authenticateRegisterProof(value, c, relay, p, time.Unix(1_700_000_000, 0))
			return err
		},
		func(value RegisterInit, c Challenge, relay RelayIdentity, p RegisterProof) error {
			relay[0] ^= 1
			_, err := authenticateRegisterProof(value, c, relay, p, time.Unix(1_700_000_000, 0))
			return err
		},
	}
	for index, test := range tests {
		if err := test(init, challenge, relayIdentity, proof); !errors.Is(err, ErrProof) {
			t.Fatalf("registration substitution %d error = %v", index, err)
		}
	}
	if _, err := authenticateRegisterProof(init, challenge, relayIdentity, proof, time.Unix(int64(challenge.ExpiresAtUnixSeconds), 0)); !errors.Is(err, ErrChallengeExpired) {
		t.Fatalf("expiry boundary error = %v", err)
	}

	var stop StopInit
	stop.ShareID, stop.ShareInstance, stop.PKHash, stop.RelayIdentity = init.ShareID, init.ShareInstance, init.PKHash, relayIdentity
	copy(stop.StopID[:], testSequence(0x62, StopIDBytes))
	stopChallenge := challenge
	stopChallenge.Purpose = ChallengeStop
	stopProof, err := NewStopProof(stop, stopChallenge, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	stopAuthority, err := authenticateStopProof(stop, stopChallenge, stopProof, time.Unix(1_700_000_000, 0))
	if err != nil || !stopAuthority.Authorizes(stop) {
		t.Fatal(err)
	}
	stop.StopID[0] ^= 1
	if _, err := authenticateStopProof(stop, stopChallenge, stopProof, time.Unix(1_700_000_000, 0)); !errors.Is(err, ErrProof) {
		t.Fatalf("stop ID substitution error = %v", err)
	}
}

func TestOpaqueRouteExposesOnlyRouteAndCiphertextLength(t *testing.T) {
	var session RelaySessionID
	copy(session[:], testSequence(1, RelaySessionIDBytes))
	// These bytes deliberately resemble a file identity and exact-size field.
	// The outer codec owns no fields capable of interpreting either value.
	opaque := append(testSequence(0x70, 16), 0, 0, 0, 0, 0, 32, 0, 23)
	frame := OpaqueRoute{RelaySessionID: session, Ciphertext: opaque}
	encoded, err := frame.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseOpaqueRoute(encoded)
	if err != nil || parsed.RelaySessionID != session || !bytes.Equal(parsed.Ciphertext, opaque) {
		t.Fatalf("opaque route = %+v, %v", parsed, err)
	}
	opaque[0] ^= 1
	if bytes.Equal(parsed.Ciphertext, opaque) {
		t.Fatal("opaque route retained caller-owned storage")
	}
	for _, mutate := range []func([]byte){
		func(value []byte) { value[5] = 1 },
		func(value []byte) { value[19]++ },
	} {
		hostile := bytes.Clone(encoded)
		mutate(hostile)
		if _, err := ParseOpaqueRoute(hostile); err == nil {
			t.Fatal("hostile outer route was accepted")
		}
	}
}

func TestSessionRetiredFrameIsExactAndAllocationFree(t *testing.T) {
	var session RelaySessionID
	copy(session[:], testSequence(41, RelaySessionIDBytes))
	encoded, err := (SessionRetired{RelaySessionID: session}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseSessionRetired(encoded)
	if err != nil || parsed.RelaySessionID != session || len(encoded) != SessionRetiredBytes {
		t.Fatalf("session retired = %+v, bytes=%d, error=%v", parsed, len(encoded), err)
	}
	if _, err := ParseOpaqueRoute(encoded); err == nil {
		t.Fatal("SESSION_RETIRED was accepted as an opaque route")
	}
	for _, hostile := range [][]byte{
		append(bytes.Clone(encoded), 0),
		encoded[:len(encoded)-1],
		appendReservedPrefix(nil, SessionRetiredMagic),
	} {
		if _, err := ParseSessionRetired(hostile); err == nil {
			t.Fatalf("hostile session retirement was accepted: %x", hostile)
		}
	}
	if _, err := (SessionRetired{}).MarshalBinary(); err == nil {
		t.Fatal("zero session retirement was encoded")
	}
}

func TestChallengeLedgerIsBoundedOneUseAndPurposeBound(t *testing.T) {
	now := time.Unix(1_000, 0)
	random := bytes.NewReader(append(testSequence(1, ChallengeIDBytes+ChallengeNonceBytes), testSequence(80, ChallengeIDBytes+ChallengeNonceBytes)...))
	ledger, err := NewChallengeLedger(ChallengeLedgerConfig{Capacity: 1, Random: random, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	init, _, relay, _ := frozenRegistration(t)
	binding, _ := RegistrationChallengeBinding(init, relay)
	challenge, err := ledger.Issue(ChallengeRegister, binding)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Issue(ChallengeRegister, binding); !errors.Is(err, ErrChallengeBudget) {
		t.Fatalf("budget error = %v", err)
	}
	if _, err := ledger.Take(challenge.ID, ChallengeResume, binding); !errors.Is(err, ErrProof) {
		t.Fatalf("purpose substitution error = %v", err)
	}
	if _, err := ledger.Take(challenge.ID, ChallengeRegister, binding); !errors.Is(err, ErrChallengeConsumed) {
		t.Fatalf("one-use error = %v", err)
	}
	second, err := ledger.Issue(ChallengeRegister, binding)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(ChallengeTTL)
	if _, err := ledger.Take(second.ID, ChallengeRegister, binding); !errors.Is(err, ErrChallengeExpired) {
		t.Fatalf("expiry error = %v", err)
	}
}

func TestChallengeLedgerIsTheOnlyAuthorityMint(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	init, _, relay, privateKey := frozenRegistration(t)
	binding, _ := RegistrationChallengeBinding(init, relay)
	random := append(testSequence(1, ChallengeIDBytes), testSequence(21, ChallengeNonceBytes)...)
	ledger, _ := NewChallengeLedger(ChallengeLedgerConfig{
		Capacity: 1, Random: bytes.NewReader(random), Now: func() time.Time { return now },
	})
	challenge, err := ledger.Issue(ChallengeRegister, binding)
	if err != nil {
		t.Fatal(err)
	}
	proof, _ := NewRegisterProof(init, challenge, relay, privateKey)
	authority, err := ledger.AuthenticateRegistration(challenge.ID, init, relay, proof)
	if err != nil || !authority.Authorizes(init) {
		t.Fatalf("registration authority = %+v, %v", authority, err)
	}
	if _, err := ledger.AuthenticateRegistration(challenge.ID, init, relay, proof); !errors.Is(err, ErrChallengeConsumed) {
		t.Fatalf("registration challenge reused: %v", err)
	}

	var stop StopInit
	stop.ShareID, stop.ShareInstance, stop.PKHash, stop.RelayIdentity = init.ShareID, init.ShareInstance, init.PKHash, relay
	copy(stop.StopID[:], testSequence(0x61, StopIDBytes))
	stopBinding, _ := StopChallengeBinding(stop)
	stopRandom := append(testSequence(51, ChallengeIDBytes), testSequence(71, ChallengeNonceBytes)...)
	stopLedger, _ := NewChallengeLedger(ChallengeLedgerConfig{
		Capacity: 1, Random: bytes.NewReader(stopRandom), Now: func() time.Time { return now },
	})
	stopChallenge, _ := stopLedger.Issue(ChallengeStop, stopBinding)
	stopProof, _ := NewStopProof(stop, stopChallenge, privateKey)
	stopAuthority, err := stopLedger.AuthenticateStop(stopChallenge.ID, stop, stopProof)
	if err != nil || !stopAuthority.Authorizes(stop) {
		t.Fatalf("stop authority = %+v, %v", stopAuthority, err)
	}
}

func TestVariableAndControlFramesRejectReservedTrailingAndLengthAliases(t *testing.T) {
	init, challenge, _, _ := frozenRegistration(t)
	cases := [][]byte{}
	initBytes, _ := init.MarshalBinary()
	challengeBytes, _ := challenge.MarshalBinary()
	cases = append(cases, initBytes, challengeBytes)
	for _, valid := range cases {
		hostile := bytes.Clone(valid)
		hostile[7] = 1
		if bytes.Equal(valid[:4], []byte("WS2R")) {
			if _, err := ParseRegisterInit(hostile); err == nil {
				t.Fatal("REGISTER_INIT reserved bit accepted")
			}
		} else if _, err := ParseChallenge(hostile); err == nil {
			t.Fatal("challenge reserved bit accepted")
		}
	}
	upload := DescriptorUpload{Object: bytes.Repeat([]byte{1}, 100)}
	encoded, _ := upload.MarshalBinary()
	if _, err := ParseDescriptorUpload(append(encoded, 0)); err == nil {
		t.Fatal("descriptor trailing byte accepted")
	}
	tooLarge := DescriptorUpload{Object: make([]byte, MaxDescriptorBytes+1)}
	if _, err := tooLarge.MarshalBinary(); err == nil {
		t.Fatal("oversized descriptor accepted")
	}
	errorFrame := ErrorFrame{Code: ErrorStopped}
	errorBytes, _ := errorFrame.MarshalBinary()
	if parsed, err := ParseError(errorBytes); err != nil || parsed.Code != ErrorStopped || parsed.RetryAfter != 0 {
		t.Fatalf("stopped error = %+v, %v", parsed, err)
	}
	if _, err := (ErrorFrame{Code: ErrorStopped, RetryAfter: time.Second}).MarshalBinary(); err == nil {
		t.Fatal("permanent stopped error carried retry-after")
	}
}

func TestDescriptorDigestSubstitutionFailsBeforePublication(t *testing.T) {
	init, challenge, relay, privateKey := frozenRegistration(t)
	proof, _ := NewRegisterProof(init, challenge, relay, privateKey)
	authority, _ := authenticateRegisterProof(init, challenge, relay, proof, time.Unix(1_700_000_000, 0))
	object := testB64(t, "AgAAAAAAAI/Q0dLT1NXW19jZ2ttZiFtVkBUbMjw7HI/q5B9qTw6bdNmlWAQtMgPxJGkRSmmZjl6KhYM8wRjPSNppp6y8l+oskLhTNjknPbPR41l3KLLCcl8a3QGnBZltpRP2erySHhoULzJzlDcEAkmQJyrASmgNQdVQSipSRia3l7yRnZAeFhColC/WYAO47TBO1eZro4Q2/eBvLZYFXoul3IZ12dbXljAhPMxure5bwD4bJzI+qSRmw1G6aZems+gCERr/qm4+5Vtdpve2Fmf2tbEqKTtppABJrqXNgEaKnQ4=")
	if digest := sha256.Sum256(object); digest != init.DescriptorDigest {
		t.Fatal("test fixture digest mismatch")
	}
	object[20] ^= 1
	if _, err := VerifyDescriptorUpload(init, authority, DescriptorUpload{Object: object}); !errors.Is(err, ErrProof) {
		t.Fatalf("descriptor substitution error = %v", err)
	}
}

func TestEveryFixedControlFrameRoundTripsAndOwnsItsMagic(t *testing.T) {
	init, challenge, relay, privateKey := frozenRegistration(t)
	proof, _ := NewRegisterProof(init, challenge, relay, privateKey)
	registered := Registered{
		ShareID: init.ShareID, ShareInstance: init.ShareInstance, DescriptorDigest: init.DescriptorDigest,
	}
	var credential ResumeCredential
	copy(credential.Token[:], testSequence(0xa1, ResumeTokenBytes))
	var stop StopInit
	stop.ShareID, stop.ShareInstance, stop.PKHash, stop.RelayIdentity = init.ShareID, init.ShareInstance, init.PKHash, relay
	copy(stop.StopID[:], testSequence(0x62, StopIDBytes))
	stopChallenge := challenge
	stopChallenge.Purpose = ChallengeStop
	stopProof, _ := NewStopProof(stop, stopChallenge, privateKey)
	stopped := Stopped{StopID: stop.StopID}
	join := Join{ShareID: init.ShareID}

	type frameCase struct {
		name   string
		encode func() ([]byte, error)
		parse  func([]byte) error
	}
	cases := []frameCase{
		{"register-init", init.MarshalBinary, func(raw []byte) error {
			got, err := ParseRegisterInit(raw)
			if err == nil && got != init {
				t.Fatal("REGISTER_INIT semantic round trip changed")
			}
			return err
		}},
		{"challenge", challenge.MarshalBinary, func(raw []byte) error {
			got, err := ParseChallenge(raw)
			if err == nil && got != challenge {
				t.Fatal("challenge semantic round trip changed")
			}
			return err
		}},
		{"register-proof", proof.MarshalBinary, func(raw []byte) error {
			got, err := ParseRegisterProof(raw)
			if err == nil && got != proof {
				t.Fatal("REGISTER_PROOF semantic round trip changed")
			}
			return err
		}},
		{"registered", registered.MarshalBinary, func(raw []byte) error {
			got, err := ParseRegistered(raw)
			if err == nil && got != registered {
				t.Fatal("REGISTERED semantic round trip changed")
			}
			return err
		}},
		{"resume-credential", credential.MarshalBinary, func(raw []byte) error {
			got, err := ParseResumeCredential(raw)
			if err == nil && got != credential {
				t.Fatal("RESUME_CREDENTIAL semantic round trip changed")
			}
			return err
		}},
		{"stop-init", stop.MarshalBinary, func(raw []byte) error {
			got, err := ParseStopInit(raw)
			if err == nil && got != stop {
				t.Fatal("STOP_INIT semantic round trip changed")
			}
			return err
		}},
		{"stop-proof", stopProof.MarshalBinary, func(raw []byte) error {
			got, err := ParseStopProof(raw)
			if err == nil && got != stopProof {
				t.Fatal("STOP_PROOF semantic round trip changed")
			}
			return err
		}},
		{"stopped", stopped.MarshalBinary, func(raw []byte) error {
			got, err := ParseStopped(raw)
			if err == nil && got != stopped {
				t.Fatal("STOPPED semantic round trip changed")
			}
			return err
		}},
		{"join", join.MarshalBinary, func(raw []byte) error {
			got, err := ParseJoin(raw)
			if err == nil && got != join {
				t.Fatal("JOIN semantic round trip changed")
			}
			return err
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			raw, err := test.encode()
			if err != nil {
				t.Fatal(err)
			}
			if err := test.parse(raw); err != nil {
				t.Fatal(err)
			}
			if err := test.parse(append(bytes.Clone(raw), 0)); err == nil {
				t.Fatal("trailing byte accepted")
			}
			hostile := bytes.Clone(raw)
			hostile[6] = 1
			if err := test.parse(hostile); err == nil {
				t.Fatal("reserved byte accepted")
			}
		})
	}
}

func TestFixedIdentityDecodersRejectWidthsAndCopyInput(t *testing.T) {
	tests := []struct {
		length int
		decode func([]byte) ([]byte, error)
	}{
		{ShareIDBytes, func(raw []byte) ([]byte, error) { value, err := ShareIDFromBytes(raw); return value[:], err }},
		{ShareInstanceBytes, func(raw []byte) ([]byte, error) { value, err := ShareInstanceFromBytes(raw); return value[:], err }},
		{PKHashBytes, func(raw []byte) ([]byte, error) { value, err := PKHashFromBytes(raw); return value[:], err }},
		{DigestBytes, func(raw []byte) ([]byte, error) { value, err := DigestFromBytes(raw); return value[:], err }},
		{RelayIdentityBytes, func(raw []byte) ([]byte, error) { value, err := RelayIdentityFromBytes(raw); return value[:], err }},
		{ChallengeIDBytes, func(raw []byte) ([]byte, error) { value, err := ChallengeIDFromBytes(raw); return value[:], err }},
		{RelaySessionIDBytes, func(raw []byte) ([]byte, error) { value, err := RelaySessionIDFromBytes(raw); return value[:], err }},
	}
	for index, test := range tests {
		raw := testSequence(byte(index+1), test.length)
		got, err := test.decode(raw)
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("decoder %d = %x, %v", index, got, err)
		}
		got[0] ^= 1
		if raw[0] == got[0] {
			t.Fatalf("decoder %d retained input storage", index)
		}
		if _, err := test.decode(raw[:len(raw)-1]); !errors.Is(err, ErrIdentity) {
			t.Fatalf("decoder %d width error = %v", index, err)
		}
	}
}

func TestResumePurposeAndStopBindingAreDistinct(t *testing.T) {
	init, challenge, relay, privateKey := frozenRegistration(t)
	init.Mode = RegistrationResume
	challenge.Purpose = ChallengeResume
	proof, err := NewRegisterProof(init, challenge, relay, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authenticateRegisterProof(init, challenge, relay, proof, time.Unix(1_700_000_000, 0)); err != nil {
		t.Fatal(err)
	}
	freshChallenge := challenge
	freshChallenge.Purpose = ChallengeRegister
	if _, err := authenticateRegisterProof(init, freshChallenge, relay, proof, time.Unix(1_700_000_000, 0)); !errors.Is(err, ErrPurpose) {
		t.Fatalf("cross-purpose proof error = %v", err)
	}
	var stop StopInit
	stop.ShareID, stop.ShareInstance, stop.PKHash, stop.RelayIdentity = init.ShareID, init.ShareInstance, init.PKHash, relay
	copy(stop.StopID[:], testSequence(0x71, StopIDBytes))
	binding, err := StopChallengeBinding(stop)
	if err != nil || binding == (ChallengeBinding{}) {
		t.Fatalf("stop binding = %x, %v", binding, err)
	}
	registrationBinding, _ := RegistrationChallengeBinding(init, relay)
	if binding == registrationBinding {
		t.Fatal("stop and registration challenge bindings collapsed")
	}
}
