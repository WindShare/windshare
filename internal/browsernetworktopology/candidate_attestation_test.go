package browsernetworktopology

import (
	"errors"
	"strings"
	"testing"
)

func TestCandidateEvaluationIsBrowserNeutralAndDeterministic(t *testing.T) {
	contract, _, _ := loadFixtureContract(t)
	profile, _, known := contract.Profile(string(ProfileScheduledPublicSTUN))
	if !known {
		t.Fatal("public STUN profile missing")
	}
	matching := matchingCandidatePath(profile.ProfileKind)
	outcome, rationales, err := EvaluateCandidatePath(
		profile.CandidatePolicy, profile.ConnectivityExpectation, matching,
	)
	if err != nil || outcome != CandidatePolicyMatched || len(rationales) != 0 {
		t.Fatalf("matching path = %q %v, err=%v", outcome, rationales, err)
	}

	local := CandidateRelay
	remote := CandidateRelay
	protocol := ProtocolTCP
	mismatch := matchingCandidatePath(profile.ProfileKind)
	mismatch.LocalCandidateType = &local
	mismatch.RemoteCandidateType = &remote
	mismatch.Protocol = &protocol
	outcome, rationales, err = EvaluateCandidatePath(
		profile.CandidatePolicy, profile.ConnectivityExpectation, mismatch,
	)
	expected := []CandidateRationaleCode{
		RationaleLocalCandidateTypeForbidden,
		RationaleLocalCandidateTypeRequiredMissing,
		RationaleRemoteCandidateTypeForbidden,
		RationaleProtocolForbidden,
		RationaleProtocolRequiredMissing,
	}
	if err != nil || outcome != CandidatePolicyMismatched || !exactRationaleCodes(rationales, expected) {
		t.Fatalf("mismatch = %q %v, err=%v", outcome, rationales, err)
	}

	outcome, rationales, err = EvaluateCandidatePath(
		profile.CandidatePolicy, profile.ConnectivityExpectation,
		CandidatePath{SelectedPair: SelectedPairAbsent},
	)
	if err != nil || outcome != CandidatePolicyMismatched ||
		!exactRationaleCodes(rationales, []CandidateRationaleCode{RationaleSelectedPairRequired}) {
		t.Fatalf("absent pair = %q %v, err=%v", outcome, rationales, err)
	}
}

func TestCandidateEvaluationSupportsProhibitedPairsAndRejectsMalformedPaths(t *testing.T) {
	contract, _, _ := loadFixtureContract(t)
	profile, _, known := contract.Profile(string(ProfileScheduledRestrictedUDP))
	if !known {
		t.Fatal("restricted UDP profile missing")
	}
	outcome, rationales, err := EvaluateCandidatePath(
		profile.CandidatePolicy,
		profile.ConnectivityExpectation,
		CandidatePath{SelectedPair: SelectedPairAbsent},
	)
	if err != nil || outcome != CandidatePolicyMatched || len(rationales) != 0 {
		t.Fatalf("prohibited absent pair = %q %v, err=%v", outcome, rationales, err)
	}
	local := CandidateHost
	remote := CandidateHost
	protocol := ProtocolUDP
	localAddress, localPort := "8.8.4.4", uint16(50_000)
	remoteAddress, remotePort := "1.1.1.1", uint16(40_000)
	outcome, rationales, err = EvaluateCandidatePath(
		profile.CandidatePolicy,
		profile.ConnectivityExpectation,
		CandidatePath{
			SelectedPair: SelectedPairPresent, LocalCandidateType: &local,
			LocalAddress: &localAddress, LocalPort: &localPort,
			RemoteCandidateType: &remote, RemoteAddress: &remoteAddress,
			RemotePort: &remotePort, Protocol: &protocol,
		},
	)
	if err != nil || outcome != CandidatePolicyMismatched ||
		!exactRationaleCodes(rationales, []CandidateRationaleCode{RationaleSelectedPairProhibited}) {
		t.Fatalf("prohibited present pair = %q %v, err=%v", outcome, rationales, err)
	}
	if _, _, err := EvaluateCandidatePath(
		profile.CandidatePolicy,
		profile.ConnectivityExpectation,
		CandidatePath{SelectedPair: SelectedPairAbsent, LocalCandidateType: &local},
	); !errors.Is(err, ErrInvalidCandidatePath) {
		t.Fatalf("malformed absent path error = %v", err)
	}
}

func TestRuntimeSatisfiedRequiresKindSpecificProof(t *testing.T) {
	contract, _, _ := loadFixtureContract(t)
	for _, spec := range frozenProfileSpecs {
		t.Run(spec.profileID, func(t *testing.T) {
			attestation := satisfiedAttestation(t, contract, "run-proof", spec.profileID)
			encoded, err := attestation.CanonicalJSON(contract)
			if err != nil {
				t.Fatalf("CanonicalJSON: %v", err)
			}
			parsed, err := ParseRuntimeAttestation(encoded, contract)
			if err != nil || parsed.ProfileID != spec.profileID {
				t.Fatalf("ParseRuntimeAttestation: parsed=%+v err=%v", parsed, err)
			}
		})
	}
}

func TestRuntimeAttestationRejectsFakeSatisfiedAndAuthorityMismatches(t *testing.T) {
	contract, _, _ := loadFixtureContract(t)
	base := satisfiedAttestation(t, contract, "run-hostile", string(ProfileScheduledPublicSTUN))
	tests := []struct {
		name   string
		mutate func(*RuntimeAttestation)
	}{
		{name: "missing proof", mutate: func(value *RuntimeAttestation) { value.Proof = nil }},
		{name: "success carries failure", mutate: func(value *RuntimeAttestation) {
			value.Failure = &RuntimeFailure{FailureKind: PrerequisiteFailed, FailureCode: FailureRuntimeCheckFailed}
		}},
		{name: "wrong authority kind", mutate: func(value *RuntimeAttestation) {
			value.AuthorityKind = AuthorityKind("fabricated-authority")
		}},
		{name: "wrong proof kind", mutate: func(value *RuntimeAttestation) {
			value.Proof.ProofKind = RuntimeProofKind("fabricated-proof")
		}},
		{name: "bad manifest digest", mutate: func(value *RuntimeAttestation) { value.ManifestSHA256 = strings.Repeat("f", 64) }},
		{name: "bad profile digest", mutate: func(value *RuntimeAttestation) { value.ProfileSHA256 = strings.Repeat("f", 64) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := base
			proofCopy := *base.Proof
			value.Proof = &proofCopy
			test.mutate(&value)
			if err := value.Validate(contract); !errors.Is(err, ErrInvalidRuntimeAttestation) {
				t.Fatalf("Validate error = %v", err)
			}
		})
	}
}

func TestRuntimeUnsatisfiedOutcomesAreTypedAndCannotCarryProof(t *testing.T) {
	contract, _, _ := loadFixtureContract(t)
	for _, outcome := range []PrerequisiteOutcome{
		PrerequisiteUnavailable, PrerequisiteInvalid, PrerequisiteFailed,
	} {
		attestation := unsatisfiedAttestation(
			t, contract, "run-unsatisfied", string(ProfileScheduledCoturn), outcome,
		)
		encoded, err := attestation.CanonicalJSON(contract)
		if err != nil {
			t.Fatalf("%s CanonicalJSON: %v", outcome, err)
		}
		if _, err := ParseRuntimeAttestation(encoded, contract); err != nil {
			t.Fatalf("%s ParseRuntimeAttestation: %v", outcome, err)
		}
		attestation.Proof = &RuntimeProof{ProofKind: RuntimeProofKind("fabricated-proof")}
		if err := attestation.Validate(contract); !errors.Is(err, ErrInvalidRuntimeAttestation) {
			t.Fatalf("%s proof-carrying error = %v", outcome, err)
		}
	}

	wrongCode := unsatisfiedAttestation(
		t, contract, "run-wrong-code", string(ProfileScheduledCoturn), PrerequisiteUnavailable,
	)
	wrongCode.Failure.FailureCode = FailureProofInvalid
	if err := wrongCode.Validate(contract); !errors.Is(err, ErrInvalidRuntimeAttestation) {
		t.Fatalf("wrong typed code error = %v", err)
	}
	wrongKind := unsatisfiedAttestation(
		t, contract, "run-wrong-kind", string(ProfileScheduledCoturn), PrerequisiteUnavailable,
	)
	wrongKind.Failure.FailureKind = PrerequisiteFailed
	if err := wrongKind.Validate(contract); !errors.Is(err, ErrInvalidRuntimeAttestation) {
		t.Fatalf("wrong failure kind error = %v", err)
	}

	bootstrapFailure := unsatisfiedAttestation(
		t, contract, "run-bootstrap-failure", string(ProfileScheduledCoturn), PrerequisiteFailed,
	)
	bootstrapFailure.Failure.FailureCode = FailurePrerequisiteBootstrap
	if err := bootstrapFailure.Validate(contract); err != nil {
		t.Fatalf("runtime bootstrap failure: %v", err)
	}
	for _, code := range []RuntimeFailureCode{
		FailureAuthorityAttestationExpired,
		FailureAuthorityKeyRotationRequired,
		FailureAuthorityNotProvisioned,
		FailureAuthorityUnreachable,
	} {
		unavailable := unsatisfiedAttestation(
			t, contract, "run-unavailable-code", string(ProfileScheduledCoturn), PrerequisiteUnavailable,
		)
		unavailable.Failure.FailureCode = code
		if err := unavailable.Validate(contract); err != nil {
			t.Fatalf("unavailable failure code %q: %v", code, err)
		}
	}

	withdrawnBootstrap := unsatisfiedAttestation(
		t, contract, "run-withdrawn-bootstrap", string(ProfileScheduledCoturn), PrerequisiteFailed,
	)
	withdrawnBootstrap.Failure.FailureCode = RuntimeFailureCode("runner-setup-failed")
	if err := withdrawnBootstrap.Validate(contract); !errors.Is(err, ErrInvalidRuntimeAttestation) {
		t.Fatalf("withdrawn bootstrap failure code error = %v", err)
	}
}

func TestExternalFixtureAttestationRequiresPinnedLocalTrust(t *testing.T) {
	contract, _, _ := loadFixtureContract(t)
	attestation := satisfiedAttestation(t, contract, "run-real-nat", string(ProfileManualRealNAT))
	tests := []struct {
		name   string
		mutate func(*ExternalFixtureTrustProof)
	}{
		{name: "non-HTTPS controller origin", mutate: func(proof *ExternalFixtureTrustProof) {
			proof.ControllerOrigin = "http://fixture.example.test/"
		}},
		{name: "noncanonical controller origin", mutate: func(proof *ExternalFixtureTrustProof) {
			proof.ControllerOrigin = "https://fixture.example.test:443/"
		}},
		{name: "IPv6 controller origin", mutate: func(proof *ExternalFixtureTrustProof) {
			proof.ControllerOrigin = "https://[2001:db8::1]/"
		}},
		{name: "invalid TLS certificate digest", mutate: func(proof *ExternalFixtureTrustProof) {
			proof.TLSCertificateSHA256 = strings.Repeat("a", 63)
		}},
		{name: "invalid TLS authority digest", mutate: func(proof *ExternalFixtureTrustProof) {
			proof.TLSCertificateAuthoritySHA256 = strings.Repeat("B", 64)
		}},
		{name: "unpinned attestation key", mutate: func(proof *ExternalFixtureTrustProof) {
			proof.AttestationPublicKeySPKI = proof.AttestationPublicKeySPKI[:len(proof.AttestationPublicKeySPKI)-1] + "A"
		}},
		{name: "mismatched key digest", mutate: func(proof *ExternalFixtureTrustProof) {
			proof.AttestationPublicKeySHA256 = strings.Repeat("0", 64)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := satisfiedAttestation(t, contract, "run-real-nat", string(ProfileManualRealNAT))
			test.mutate(&value.Proof.ExternalFixtureTrust)
			if err := value.Validate(contract); !errors.Is(err, ErrInvalidRuntimeAttestation) {
				t.Fatalf("external fixture trust proof error = %v", err)
			}
		})
	}

	encoded, err := attestation.CanonicalJSON(contract)
	if err != nil {
		t.Fatalf("canonical external fixture attestation: %v", err)
	}
	canonicalProof, err := marshalCanonicalDocument(attestation.Proof.ExternalFixtureTrust)
	if err != nil {
		t.Fatal(err)
	}
	expectedProof := `"externalFixtureTrust":` + strings.TrimSuffix(string(canonicalProof), "\n")
	if !strings.Contains(string(encoded), expectedProof) {
		t.Fatal("external fixture proof does not preserve its exact five-field canonical trust object")
	}
	for _, withdrawn := range []string{
		`"externalFixture":`, "external-fixture-binding", `"probeId":`, `"signedAttestation":`,
		`"attestationSha256":`, `"configurationSha256":`, `"networkBindingSha256":`,
		`"remotePeerBindingSha256":`, `"controllerPublicIp":`, `"leaseExpiresAt":`,
	} {
		if strings.Contains(string(encoded), withdrawn) {
			t.Fatalf("canonical external fixture trust proof contains withdrawn v1 term %q: %s", withdrawn, encoded)
		}
	}
	oldUnion := []byte(strings.Replace(string(encoded), `"externalFixtureTrust":`, `"externalFixture":`, 1))
	if _, err := ParseRuntimeAttestation(oldUnion, contract); !errors.Is(err, ErrInvalidRuntimeAttestation) {
		t.Fatalf("withdrawn v1 runtime proof union error = %v", err)
	}
}

func TestExternalFixtureTrustUsesCanonicalHTTPSOrigins(t *testing.T) {
	for _, origin := range []string{
		"https://fixture.example.test/",
		"https://127.0.0.1:0/",
		"https://fixture.example.test:65535/",
	} {
		if !canonicalHTTPSOrigin(origin) {
			t.Fatalf("canonical HTTPS origin %q was rejected", origin)
		}
	}
	for _, origin := range []string{
		"http://fixture.example.test/",
		"https://user@fixture.example.test/",
		"https://fixture.example.test/path",
		"https://fixture.example.test/?query=1",
		"https://fixture.example.test/#fragment",
		"https://fixture.example.test:443/",
		"https://fixture.example.test:0443/",
		"https://[2001:db8::1]/",
		"https://Fixture.example.test/",
		"https://127.000.000.001/",
	} {
		if canonicalHTTPSOrigin(origin) {
			t.Fatalf("noncanonical HTTPS origin %q was accepted", origin)
		}
	}
}

func TestRuntimeAttestationParserRejectsUnknownAndNoncanonicalJSON(t *testing.T) {
	contract, _, _ := loadFixtureContract(t)
	attestation := satisfiedAttestation(t, contract, "run-canonical", string(ProfileScheduledPublicSTUN))
	encoded, err := attestation.CanonicalJSON(contract)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if _, err := ParseRuntimeAttestation(addRootMember(encoded, `"unknown":true`), contract); !errors.Is(err, ErrInvalidRuntimeAttestation) {
		t.Fatalf("unknown field error = %v", err)
	}
	nestedUnknown := []byte(strings.Replace(
		string(encoded),
		`"attestationPublicKeySha256":"`+testFixtureAttestationPublicKeySHA256+`"}`,
		`"attestationPublicKeySha256":"`+testFixtureAttestationPublicKeySHA256+`","platformClaims":[]}`,
		1,
	))
	if _, err := ParseRuntimeAttestation(nestedUnknown, contract); !errors.Is(err, ErrInvalidRuntimeAttestation) {
		t.Fatalf("nested unknown field error = %v", err)
	}
	v1Schema := []byte(strings.Replace(
		string(encoded),
		"windshare.browser-network-matrix.runtime-attestation/v2",
		"windshare.browser-network-matrix.runtime-attestation/v1",
		1,
	))
	if _, err := ParseRuntimeAttestation(v1Schema, contract); !errors.Is(err, ErrInvalidRuntimeAttestation) {
		t.Fatalf("withdrawn v1 schema error = %v", err)
	}
	if _, err := ParseRuntimeAttestation(encoded[:len(encoded)-1], contract); !errors.Is(err, ErrNonCanonicalJSON) {
		t.Fatalf("noncanonical error = %v", err)
	}
}

func TestRuntimeAttestationDigestCommitsToLocalTrust(t *testing.T) {
	contract, _, _ := loadFixtureContract(t)
	attestation := satisfiedAttestation(t, contract, "run-trust-digest", string(ProfileScheduledPublicSTUN))
	encoded, err := attestation.CanonicalJSON(contract)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	digest, err := attestation.SHA256(contract)
	if err != nil || digest != sha256Text(encoded) {
		t.Fatalf("SHA256 = %q, err=%v", digest, err)
	}

	mutated := attestation
	proof := *attestation.Proof
	mutated.Proof = &proof
	mutated.Proof.ExternalFixtureTrust.TLSCertificateSHA256 = strings.Repeat("c", 64)
	mutatedDigest, err := mutated.SHA256(contract)
	if err != nil {
		t.Fatalf("mutated SHA256: %v", err)
	}
	if mutatedDigest == digest {
		t.Fatal("runtime attestation digest did not commit to the local TLS trust declaration")
	}
}

func TestExternalFixtureProofMatchesSharedDeterministicVector(t *testing.T) {
	proof := signedFixtureProof(t, "shared-observed-sample-run", string(ProfileScheduledPublicSTUN))
	want := struct {
		attestation   string
		signature     string
		configuration string
		network       string
		remote        string
	}{
		attestation:   "f4b56fb2e08f51cf093c2c9434efad56dbc1b0947fcac052ff341dad98e340eb",
		signature:     "BKA8IGeUz3jN89uID_pQ__nKlyy-TA1A30EEhc850BrOkJJDze6t7Z1x0f6xIaHARnYXom2yWhkHDprUgNBQBg",
		configuration: "a47097e146d4f7f129430374a37ad8763cdc109c4a020669a4d991ff47fad696",
		network:       "e41c6ad7acaceeb8ee1840404099045d6e14fea55d220aaa32227bf686c8ef9f",
		remote:        "faffe144e5f48beee5904adf6b76e09364819b2f933b4dc5e16e230211e93a76",
	}
	if proof.AttestationPublicKeySPKI != testFixtureAttestationPublicKeySPKI ||
		proof.AttestationSHA256 != want.attestation ||
		proof.SignedAttestation.Signature != want.signature ||
		proof.ConfigurationSHA256 != want.configuration ||
		proof.NetworkBindingSHA256 != want.network ||
		proof.RemotePeerBindingSHA256 != want.remote {
		t.Fatalf("shared signed fixture proof vector changed: %+v", proof)
	}
}
