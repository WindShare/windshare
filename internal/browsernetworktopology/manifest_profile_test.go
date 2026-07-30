package browsernetworktopology

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestCanonicalFixturesBindExactMatrixUniverse(t *testing.T) {
	contract, manifestJSON, documents := loadFixtureContract(t)
	if got := sha256Text(manifestJSON); got != fixtureManifestSHA256 || contract.ManifestSHA256() != got {
		t.Fatalf("manifest SHA256 = %q, contract = %q", got, contract.ManifestSHA256())
	}
	manifest, err := ParseManifest(manifestJSON)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	canonicalManifest, err := manifest.CanonicalJSON()
	if err != nil || !bytes.Equal(canonicalManifest, manifestJSON) {
		t.Fatalf("manifest canonical bytes differ: err=%v", err)
	}
	for index, document := range documents {
		profile, parseErr := ParseProfile(document.JSON)
		if parseErr != nil {
			t.Fatalf("ParseProfile %s: %v", document.Path, parseErr)
		}
		digest, digestErr := profile.SHA256()
		if digestErr != nil || digest != fixtureProfileSHA256[profile.ProfileID] {
			t.Fatalf("profile %s digest = %q, err=%v", profile.ProfileID, digest, digestErr)
		}
		if digest != manifest.Profiles[index].ProfileSHA256 {
			t.Fatalf("profile %s is not digest-bound by manifest", profile.ProfileID)
		}
	}

	scheduled, err := contract.ExpectedIdentities(ModeScheduled)
	if err != nil || len(scheduled) != ScheduledIdentityCount {
		t.Fatalf("scheduled identities = %d, err=%v", len(scheduled), err)
	}
	manual, err := contract.ExpectedIdentities(ModeManual)
	if err != nil || len(manual) != ManualIdentityCount {
		t.Fatalf("manual identities = %d, err=%v", len(manual), err)
	}
	all, err := contract.AllExpectedIdentities()
	if err != nil || len(all) != TotalIdentityCount {
		t.Fatalf("all identities = %d, err=%v", len(all), err)
	}
	if scheduled[0] != (SampleIdentity{ProfileID: string(ProfileScheduledPublicSTUN), Browser: BrowserChromium, SampleOrdinal: 1}) ||
		scheduled[len(scheduled)-1] != (SampleIdentity{ProfileID: string(ProfileScheduledCoturn), Browser: BrowserWebKit, SampleOrdinal: 5}) ||
		manual[0] != (SampleIdentity{ProfileID: string(ProfileManualRealNAT), Browser: BrowserChromium, SampleOrdinal: 1}) ||
		manual[len(manual)-1].Browser != BrowserWebKit || manual[len(manual)-1].SampleOrdinal != 5 {
		t.Fatal("identity universe is not in topology x browser x ordinal canonical order")
	}
}

func TestManifestAndProfileParsersRejectUnknownAndNoncanonicalJSON(t *testing.T) {
	_, manifestJSON, documents := loadFixtureContract(t)
	tests := []struct {
		name     string
		encoded  []byte
		parse    func([]byte) error
		sentinel error
	}{
		{
			name:     "manifest unknown field",
			encoded:  addRootMember(manifestJSON, `"unknown":true`),
			parse:    func(encoded []byte) error { _, err := ParseManifest(encoded); return err },
			sentinel: ErrInvalidManifest,
		},
		{
			name:     "manifest missing canonical LF",
			encoded:  manifestJSON[:len(manifestJSON)-1],
			parse:    func(encoded []byte) error { _, err := ParseManifest(encoded); return err },
			sentinel: ErrNonCanonicalJSON,
		},
		{
			name:     "profile unknown field",
			encoded:  addRootMember(documents[0].JSON, `"unknown":true`),
			parse:    func(encoded []byte) error { _, err := ParseProfile(encoded); return err },
			sentinel: ErrInvalidProfile,
		},
		{
			name:     "profile duplicate field",
			encoded:  addRootMember(documents[0].JSON, `"schemaVersion":"`+ProfileSchemaVersion+`"`),
			parse:    func(encoded []byte) error { _, err := ParseProfile(encoded); return err },
			sentinel: ErrNonCanonicalJSON,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.parse(test.encoded); !errors.Is(err, test.sentinel) {
				t.Fatalf("error = %v, want %v", err, test.sentinel)
			}
		})
	}
}

func TestContractRejectsDigestPathAndAuthorityMismatches(t *testing.T) {
	_, manifestJSON, documents := loadFixtureContract(t)
	manifest, err := ParseManifest(manifestJSON)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}

	wrongDigest := manifest
	wrongDigest.Profiles = append([]ProfileReference(nil), manifest.Profiles...)
	wrongDigest.Profiles[0].ProfileSHA256 = strings.Repeat("f", 64)
	wrongDigestJSON, err := wrongDigest.CanonicalJSON()
	if err != nil {
		t.Fatalf("wrong digest manifest encode: %v", err)
	}
	if _, err := ParseContract(wrongDigestJSON, documents); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("wrong digest error = %v", err)
	}

	duplicateDigest := manifest
	duplicateDigest.Profiles = append([]ProfileReference(nil), manifest.Profiles...)
	duplicateDigest.Profiles[1].ProfileSHA256 = duplicateDigest.Profiles[0].ProfileSHA256
	if err := duplicateDigest.Validate(); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("duplicate profile digest error = %v", err)
	}

	malformedAnchor := manifest
	malformedAnchor.Authorities = append([]AuthorityDefinition(nil), manifest.Authorities...)
	malformedAnchor.Authorities[0].AttestationPublicKeySHA256 = "f"
	if err := malformedAnchor.Validate(); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("malformed attestation trust anchor error = %v", err)
	}

	driftedAnchor := manifest
	driftedAnchor.Authorities = append([]AuthorityDefinition(nil), manifest.Authorities...)
	driftedAnchor.Authorities[0].AttestationPublicKeySHA256 = strings.Repeat("f", 64)
	driftedAnchorJSON, err := driftedAnchor.CanonicalJSON()
	if err != nil {
		t.Fatalf("drifted trust anchor manifest encode: %v", err)
	}
	if _, err := ParseContract(driftedAnchorJSON, documents); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("profile/manifest trust anchor mismatch error = %v", err)
	}

	badPathDocuments := append([]ProfileDocument(nil), documents...)
	badPathDocuments[0].Path = `profiles\scheduled-public-stun.v1.json`
	if _, err := ParseContract(manifestJSON, badPathDocuments); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("non-POSIX path error = %v", err)
	}
	if _, err := ParseContract(manifestJSON, documents[:len(documents)-1]); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("missing profile error = %v", err)
	}
	duplicateDocuments := append([]ProfileDocument(nil), documents...)
	duplicateDocuments[1] = documents[0]
	if _, err := ParseContract(manifestJSON, duplicateDocuments); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("duplicate profile error = %v", err)
	}

	profile, err := ParseProfile(documents[0].JSON)
	if err != nil {
		t.Fatalf("ParseProfile: %v", err)
	}
	profile.Authority.AuthorityKind = AuthorityKind("fabricated-authority")
	wrongAuthorityJSON, err := marshalCanonicalDocument(profile)
	if err != nil {
		t.Fatalf("marshal wrong authority profile: %v", err)
	}
	if _, err := ParseProfile(wrongAuthorityJSON); !errors.Is(err, ErrInvalidProfile) {
		t.Fatalf("wrong authority kind error = %v", err)
	}
}

func TestCandidatePolicyRejectsContradictionsAndNoncanonicalSets(t *testing.T) {
	_, _, documents := loadFixtureContract(t)
	base, err := ParseProfile(documents[0].JSON)
	if err != nil {
		t.Fatalf("ParseProfile: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*Profile)
	}{
		{
			name: "multiple required local values",
			mutate: func(profile *Profile) {
				profile.CandidatePolicy.LocalCandidateTypes.Required = []CandidateType{CandidateHost, CandidateSRFLX}
			},
		},
		{
			name: "required value outside allowed",
			mutate: func(profile *Profile) {
				profile.CandidatePolicy.LocalCandidateTypes.Required = []CandidateType{CandidateRelay}
			},
		},
		{
			name: "allowed and forbidden overlap",
			mutate: func(profile *Profile) {
				profile.CandidatePolicy.Protocols.Forbidden = []TransportProtocol{ProtocolUDP}
			},
		},
		{
			name: "candidate vocabulary value unclassified",
			mutate: func(profile *Profile) {
				profile.CandidatePolicy.LocalCandidateTypes.Forbidden = []CandidateType{}
			},
		},
		{
			name: "protocol vocabulary value unclassified",
			mutate: func(profile *Profile) {
				profile.CandidatePolicy.Protocols.Forbidden = []TransportProtocol{}
			},
		},
		{
			name: "duplicate candidate type",
			mutate: func(profile *Profile) {
				profile.CandidatePolicy.RemoteCandidateTypes.Allowed = []CandidateType{CandidateHost, CandidateHost}
			},
		},
		{
			name: "unordered protocol set",
			mutate: func(profile *Profile) {
				profile.CandidatePolicy.Protocols.Allowed = []TransportProtocol{ProtocolTCP, ProtocolUDP}
			},
		},
		{
			name: "selected pair contradicts profile connectivity",
			mutate: func(profile *Profile) {
				profile.CandidatePolicy.SelectedPair = SelectedPairProhibited
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			profile := base
			profile.CandidatePolicy.LocalCandidateTypes.Allowed = append([]CandidateType(nil), base.CandidatePolicy.LocalCandidateTypes.Allowed...)
			profile.CandidatePolicy.LocalCandidateTypes.Required = append([]CandidateType(nil), base.CandidatePolicy.LocalCandidateTypes.Required...)
			profile.CandidatePolicy.LocalCandidateTypes.Forbidden = append([]CandidateType(nil), base.CandidatePolicy.LocalCandidateTypes.Forbidden...)
			profile.CandidatePolicy.RemoteCandidateTypes.Allowed = append([]CandidateType(nil), base.CandidatePolicy.RemoteCandidateTypes.Allowed...)
			profile.CandidatePolicy.RemoteCandidateTypes.Required = append([]CandidateType(nil), base.CandidatePolicy.RemoteCandidateTypes.Required...)
			profile.CandidatePolicy.RemoteCandidateTypes.Forbidden = append([]CandidateType(nil), base.CandidatePolicy.RemoteCandidateTypes.Forbidden...)
			profile.CandidatePolicy.Protocols.Allowed = append([]TransportProtocol(nil), base.CandidatePolicy.Protocols.Allowed...)
			profile.CandidatePolicy.Protocols.Required = append([]TransportProtocol(nil), base.CandidatePolicy.Protocols.Required...)
			profile.CandidatePolicy.Protocols.Forbidden = append([]TransportProtocol(nil), base.CandidatePolicy.Protocols.Forbidden...)
			test.mutate(&profile)
			if err := profile.Validate(); !errors.Is(err, ErrInvalidCandidatePolicy) {
				t.Fatalf("Validate error = %v", err)
			}
		})
	}
	prohibitedWithConstraints := base.CandidatePolicy
	prohibitedWithConstraints.SelectedPair = SelectedPairProhibited
	if err := prohibitedWithConstraints.Validate(ConnectivityBlocked); !errors.Is(err, ErrInvalidCandidatePolicy) {
		t.Fatalf("prohibited pair with constraints error = %v", err)
	}
}

func TestProfileKindFixesConnectivitySemantics(t *testing.T) {
	_, _, documents := loadFixtureContract(t)
	profile, err := ParseProfile(documents[0].JSON)
	if err != nil {
		t.Fatalf("ParseProfile: %v", err)
	}
	profile.ConnectivityExpectation = ConnectivityBlocked
	profile.CandidatePolicy = CandidatePolicy{
		SelectedPair:         SelectedPairProhibited,
		LocalCandidateTypes:  CandidateTypeConstraint{Allowed: []CandidateType{}, Required: []CandidateType{}, Forbidden: []CandidateType{}},
		RemoteCandidateTypes: CandidateTypeConstraint{Allowed: []CandidateType{}, Required: []CandidateType{}, Forbidden: []CandidateType{}},
		Protocols:            ProtocolConstraint{Allowed: []TransportProtocol{}, Required: []TransportProtocol{}, Forbidden: []TransportProtocol{}},
	}
	if err := profile.Validate(); !errors.Is(err, ErrInvalidProfile) {
		t.Fatalf("rewritten profile connectivity error = %v", err)
	}
}

func TestCanonicalContractRejectsWithdrawnSchedulerSchema(t *testing.T) {
	_, manifestJSON, documents := loadFixtureContract(t)
	canonicalDocuments := append([][]byte{manifestJSON}, profileBytes(documents)...)
	for _, encoded := range canonicalDocuments {
		for _, withdrawn := range [][]byte{
			[]byte(`"executionTrigger"`),
			[]byte(`"requiredRunnerLabels"`),
			[]byte(`manual-self-hosted-real-nat`),
			[]byte(`self-hosted-runner`),
		} {
			if bytes.Contains(encoded, withdrawn) {
				t.Fatalf("canonical contract contains withdrawn scheduler term %q", withdrawn)
			}
		}
	}

	oldModeManifest := bytes.Replace(manifestJSON, []byte(`"executionMode"`), []byte(`"executionTrigger"`), 1)
	if _, err := ParseManifest(oldModeManifest); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("withdrawn manifest execution field error = %v", err)
	}
	oldModeProfile := bytes.Replace(documents[0].JSON, []byte(`"executionMode"`), []byte(`"executionTrigger"`), 1)
	if _, err := ParseProfile(oldModeProfile); !errors.Is(err, ErrInvalidProfile) {
		t.Fatalf("withdrawn profile execution field error = %v", err)
	}
	oldLabels := bytes.Replace(
		manifestJSON,
		[]byte(`"availabilityExpectation":"not-assumed"`),
		[]byte(`"availabilityExpectation":"not-assumed","requiredRunnerLabels":[]`),
		1,
	)
	if _, err := ParseManifest(oldLabels); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("withdrawn manifest label field error = %v", err)
	}
}

func TestContractProfileReturnsDeepCopy(t *testing.T) {
	contract, _, _ := loadFixtureContract(t)
	publicProfileID := string(ProfileScheduledPublicSTUN)
	first, digest, ok := contract.Profile(publicProfileID)
	if !ok {
		t.Fatal("public STUN profile missing")
	}
	first.CandidatePolicy.LocalCandidateTypes.Allowed[0] = CandidateRelay

	second, secondDigest, ok := contract.Profile(publicProfileID)
	if !ok || secondDigest != digest {
		t.Fatalf("second profile lookup = ok:%v digest:%q", ok, secondDigest)
	}
	if second.CandidatePolicy.LocalCandidateTypes.Allowed[0] != CandidateHost {
		t.Fatal("candidate-policy mutation escaped into the digest-bound contract")
	}

	for _, spec := range frozenProfileSpecs {
		profile, _, known := contract.Profile(spec.profileID)
		if !known {
			t.Fatalf("profile %q missing", spec.profileID)
		}
		if err := profile.Validate(); err != nil {
			t.Fatalf("cloned profile %q is no longer valid: %v", spec.profileID, err)
		}
	}
}

func profileBytes(documents []ProfileDocument) [][]byte {
	result := make([][]byte, len(documents))
	for index := range documents {
		result[index] = documents[index].JSON
	}
	return result
}

func addRootMember(encoded []byte, member string) []byte {
	trimmed := bytes.TrimSuffix(encoded, []byte("\n"))
	result := append([]byte(nil), trimmed[:len(trimmed)-1]...)
	result = append(result, ',')
	result = append(result, member...)
	return append(result, '}', '\n')
}
