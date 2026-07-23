package v2

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"io"
	"testing"
	"time"
)

func TestChallengeLedgerRejectsEntropyBudgetAndBindingFailures(t *testing.T) {
	if _, err := NewChallengeLedger(ChallengeLedgerConfig{}); !errors.Is(err, ErrChallengeBudget) {
		t.Fatalf("zero ledger config error = %v", err)
	}
	if _, err := NewChallengeLedger(ChallengeLedgerConfig{Capacity: 1}); !errors.Is(err, ErrChallengeBudget) {
		t.Fatalf("nil random source error = %v", err)
	}
	now := time.Unix(100, 0)
	zeroLedger, _ := NewChallengeLedger(ChallengeLedgerConfig{
		Capacity: 1, Random: bytes.NewReader(make([]byte, challengeIssueRetries*(ChallengeIDBytes+ChallengeNonceBytes))),
		Now: func() time.Time { return now },
	})
	var binding ChallengeBinding
	binding[0] = 1
	if _, err := zeroLedger.Issue(ChallengeRegister, binding); !errors.Is(err, ErrChallengeBudget) {
		t.Fatalf("zero entropy error = %v", err)
	}
	shortLedger, _ := NewChallengeLedger(ChallengeLedgerConfig{
		Capacity: 1, Random: bytes.NewReader([]byte{1}), Now: func() time.Time { return now },
	})
	if _, err := shortLedger.Issue(ChallengeRegister, binding); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short entropy error = %v", err)
	}
	if _, err := shortLedger.Issue(ChallengePurpose(99), binding); !errors.Is(err, ErrPurpose) {
		t.Fatalf("unknown purpose error = %v", err)
	}
	if _, err := shortLedger.Issue(ChallengeRegister, ChallengeBinding{}); !errors.Is(err, ErrPurpose) {
		t.Fatalf("zero binding error = %v", err)
	}

	random := append(testSequence(1, ChallengeIDBytes), testSequence(21, ChallengeNonceBytes)...)
	random = append(random, testSequence(51, ChallengeIDBytes)...)
	random = append(random, testSequence(71, ChallengeNonceBytes)...)
	ledger, _ := NewChallengeLedger(ChallengeLedgerConfig{
		Capacity: 1, Random: bytes.NewReader(random), Now: func() time.Time { return now },
	})
	first, err := ledger.Issue(ChallengeRegister, binding)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(ChallengeTTL)
	second, err := ledger.Issue(ChallengeRegister, binding)
	if err != nil || second.ID == first.ID {
		t.Fatalf("expired cleanup issue = %+v, %v", second, err)
	}
	wrongBinding := binding
	wrongBinding[0] ^= 1
	if _, err := ledger.Take(second.ID, ChallengeRegister, wrongBinding); !errors.Is(err, ErrProof) {
		t.Fatalf("binding substitution error = %v", err)
	}
	if _, err := (*ChallengeLedger)(nil).Take(second.ID, ChallengeRegister, binding); !errors.Is(err, ErrChallengeConsumed) {
		t.Fatalf("nil ledger take error = %v", err)
	}
}

func TestFixedFramesRejectZeroSemanticFieldsBeforeEncoding(t *testing.T) {
	init, challenge, relay, privateKey := frozenRegistration(t)
	proof, _ := NewRegisterProof(init, challenge, relay, privateKey)
	var stop StopInit
	stop.ShareID, stop.ShareInstance, stop.PKHash, stop.RelayIdentity = init.ShareID, init.ShareInstance, init.PKHash, relay
	copy(stop.StopID[:], testSequence(0x61, StopIDBytes))
	stopChallenge := challenge
	stopChallenge.Purpose = ChallengeStop
	stopProof, _ := NewStopProof(stop, stopChallenge, privateKey)
	tests := []struct {
		name string
		run  func() error
	}{
		{"register-mode", func() error { value := init; value.Mode = 9; _, err := value.MarshalBinary(); return err }},
		{"register-share", func() error {
			value := init
			value.ShareInstance = ShareInstance{}
			_, err := value.MarshalBinary()
			return err
		}},
		{"challenge-purpose", func() error { value := challenge; value.Purpose = 9; _, err := value.MarshalBinary(); return err }},
		{"challenge-id", func() error {
			value := challenge
			value.ID = ChallengeID{}
			_, err := value.MarshalBinary()
			return err
		}},
		{"register-proof", func() error {
			value := proof
			value.Signature = [SignatureBytes]byte{}
			_, err := value.MarshalBinary()
			return err
		}},
		{"registered", func() error { _, err := (Registered{}).MarshalBinary(); return err }},
		{"resume", func() error { _, err := (ResumeCredential{}).MarshalBinary(); return err }},
		{"stop-init", func() error { value := stop; value.StopID = StopID{}; _, err := value.MarshalBinary(); return err }},
		{"stop-proof", func() error {
			value := stopProof
			value.Signature = [SignatureBytes]byte{}
			_, err := value.MarshalBinary()
			return err
		}},
		{"stopped", func() error { _, err := (Stopped{}).MarshalBinary(); return err }},
		{"join", func() error { _, err := (Join{}).MarshalBinary(); return err }},
		{"delivery", func() error { _, err := (DescriptorDelivery{Object: []byte{1}}).MarshalBinary(); return err }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); err == nil {
				t.Fatal("invalid frame encoded")
			}
		})
	}
}

func TestProofAuthoritiesCannotCrossRegistrationOrStopBoundaries(t *testing.T) {
	init, challenge, relay, privateKey := frozenRegistration(t)
	if _, err := NewRegisterProof(init, challenge, relay, privateKey[:63]); !errors.Is(err, ErrProof) {
		t.Fatalf("short register key error = %v", err)
	}
	otherKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x91}, ed25519.SeedSize))
	if _, err := NewRegisterProof(init, challenge, relay, otherKey); !errors.Is(err, ErrProof) {
		t.Fatalf("wrong register key error = %v", err)
	}
	proof, _ := NewRegisterProof(init, challenge, relay, privateKey)
	badMode := proof
	badMode.Mode = RegistrationResume
	if _, err := authenticateRegisterProof(init, challenge, relay, badMode, time.Unix(1_700_000_000, 0)); !errors.Is(err, ErrProof) {
		t.Fatalf("proof mode substitution error = %v", err)
	}
	badSignature := proof
	badSignature.Signature[0] ^= 1
	if _, err := authenticateRegisterProof(init, challenge, relay, badSignature, time.Unix(1_700_000_000, 0)); !errors.Is(err, ErrProof) {
		t.Fatalf("register signature substitution error = %v", err)
	}
	authority, _ := authenticateRegisterProof(init, challenge, relay, proof, time.Unix(1_700_000_000, 0))
	if (SenderAuthority{}).Authorizes(init) {
		t.Fatal("zero sender authority authorized registration")
	}
	otherInit := init
	otherInit.DescriptorDigest[0] ^= 1
	if authority.Authorizes(otherInit) {
		t.Fatal("sender authority crossed registration digest")
	}

	uploadBytes := testB64(t, "V1MyVQIAAAAAAADjAgAAAAAAAI/Q0dLT1NXW19jZ2ttZiFtVkBUbMjw7HI/q5B9qTw6bdNmlWAQtMgPxJGkRSmmZjl6KhYM8wRjPSNppp6y8l+oskLhTNjknPbPR41l3KLLCcl8a3QGnBZltpRP2erySHhoULzJzlDcEAkmQJyrASmgNQdVQSipSRia3l7yRnZAeFhColC/WYAO47TBO1eZro4Q2/eBvLZYFXoul3IZ12dbXljAhPMxure5bwD4bJzI+qSRmw1G6aZems+gCERr/qm4+5Vtdpve2Fmf2tbEqKTtppABJrqXNgEaKnQ4=")
	upload, _ := ParseDescriptorUpload(uploadBytes)
	if _, err := VerifyDescriptorUpload(init, SenderAuthority{}, upload); !errors.Is(err, ErrProof) {
		t.Fatalf("zero descriptor authority error = %v", err)
	}
	malformed := bytes.Clone(upload.Object)
	malformed[1] = 1
	if _, err := VerifyDescriptorUpload(init, authority, DescriptorUpload{Object: malformed}); !errors.Is(err, ErrProof) {
		t.Fatalf("descriptor header substitution error = %v", err)
	}
	verified, _ := VerifyDescriptorUpload(init, authority, upload)
	if object, ok := (VerifiedDescriptor{}).ObjectFor(init); ok || object != nil {
		t.Fatal("zero verified descriptor released bytes")
	}
	if object, ok := verified.ObjectFor(otherInit); ok || object != nil {
		t.Fatal("verified descriptor crossed registration")
	}

	var stop StopInit
	stop.ShareID, stop.ShareInstance, stop.PKHash, stop.RelayIdentity = init.ShareID, init.ShareInstance, init.PKHash, relay
	copy(stop.StopID[:], testSequence(0x61, StopIDBytes))
	stopChallenge := challenge
	stopChallenge.Purpose = ChallengeStop
	if _, err := NewStopProof(stop, stopChallenge, privateKey[:63]); !errors.Is(err, ErrProof) {
		t.Fatalf("short stop key error = %v", err)
	}
	if _, err := NewStopProof(stop, stopChallenge, otherKey); !errors.Is(err, ErrProof) {
		t.Fatalf("wrong stop key error = %v", err)
	}
	stopProof, _ := NewStopProof(stop, stopChallenge, privateKey)
	stopProof.Signature[0] ^= 1
	if _, err := authenticateStopProof(stop, stopChallenge, stopProof, time.Unix(1_700_000_000, 0)); !errors.Is(err, ErrProof) {
		t.Fatalf("stop signature substitution error = %v", err)
	}
	if (StopAuthority{}).Authorizes(stop) {
		t.Fatal("zero stop authority authorized STOP")
	}
}

func TestErrorAndPayloadParsersRejectUnknownCodesAndZeroRoutes(t *testing.T) {
	errorBytes, _ := (ErrorFrame{Code: ErrorAdmission, RetryAfter: time.Second}).MarshalBinary()
	errorBytes[7] = 99
	if _, err := ParseError(errorBytes); !errors.Is(err, ErrMalformed) {
		t.Fatalf("unknown relay error = %v", err)
	}
	if _, err := (OpaqueRoute{Ciphertext: []byte{1}}).MarshalBinary(); !errors.Is(err, ErrMalformed) {
		t.Fatalf("zero opaque route error = %v", err)
	}
	var session RelaySessionID
	session[0] = 1
	if _, err := (OpaqueRoute{RelaySessionID: session}).MarshalBinary(); !errors.Is(err, ErrMalformed) {
		t.Fatalf("empty opaque ciphertext error = %v", err)
	}
	if _, err := (OpaqueRoute{RelaySessionID: session, Ciphertext: make([]byte, MaxOpaqueCiphertextBytes+1)}).MarshalBinary(); !errors.Is(err, ErrMalformed) {
		t.Fatalf("oversized opaque ciphertext error = %v", err)
	}
	if _, err := senderKeyHash(make([]byte, SenderPublicKeyBytes-1)); !errors.Is(err, ErrIdentity) {
		t.Fatalf("short sender key error = %v", err)
	}
}
