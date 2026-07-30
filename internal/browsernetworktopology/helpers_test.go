package browsernetworktopology

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/internal/browsermatrixfixture"
	"github.com/windshare/windshare/internal/browsermatrixpion"
)

const (
	fixtureManifestSHA256                          = "4e57f971941aef9667f42531fdb4f903d89fbded9753afe660273efe0f6f4379"
	testFixtureAuthoritySeedDomain                 = "windshare/browser-network-matrix/test-fixture-attestation-authority/v1\n"
	testFixtureAttestationPublicKeySPKI            = "MCowBQYDK2VwAyEAzzdkusInBjsJpvUWGibdzr50te_7a2iquAxNqY_jJiE"
	testFixtureAttestationPublicKeySHA256          = "dfb0a30ce0e35eae2e02434da28c14a7977eb4b966f82efee9cfd6e27c401ad8"
	testFixtureControllerOrigin                    = "https://fixture.example.test/"
	testFixtureTLSCertificateSHA256                = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testFixtureTLSCertificateAuthoritySHA256       = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testFixtureIssuedAt                            = "2026-07-29T00:00:00.000Z"
	testFixtureExpiresAt                           = "2026-07-29T00:05:00.000Z"
	testFixtureLeaseMillis                   int64 = 300_000
)

var fixtureProfileSHA256 = map[string]string{
	string(ProfileScheduledPublicSTUN):    "c4a10d8d5712307e29cde26ec26dadcec2ff89da293a4aa467ef656f2cb2b7e5",
	string(ProfileScheduledRestrictedUDP): "01f59210b0e92ee8b327714afe3daad86ee1ae167bbb792ade1f7b593b744e31",
	string(ProfileScheduledCoturn):        "1777486737a8e7e4f4286d788689ea6b9d50c2a60b0a54021815a43a7df96a90",
	string(ProfileManualRealNAT):          "2689011b60e2b16549725c13188abeadef106590f47829309c8c2099a9cc432f",
}

func loadFixtureContract(t *testing.T) (Contract, []byte, []ProfileDocument) {
	t.Helper()
	fixtureRoot := filepath.Join("..", "..", "testdata", "browser-network-matrix")
	manifestJSON := mustReadFile(t, filepath.Join(fixtureRoot, "manifest.v1.json"))
	documents := make([]ProfileDocument, 0, len(frozenProfileSpecs))
	for _, spec := range frozenProfileSpecs {
		documents = append(documents, ProfileDocument{
			Path: spec.profilePath,
			JSON: mustReadFile(t, filepath.Join(fixtureRoot, filepath.FromSlash(spec.profilePath))),
		})
	}
	contract, err := ParseContract(manifestJSON, documents)
	if err != nil {
		t.Fatalf("ParseContract fixtures: %v", err)
	}
	return contract, manifestJSON, documents
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return encoded
}

func satisfiedAttestation(t *testing.T, contract Contract, runID, profileID string) RuntimeAttestation {
	t.Helper()
	profile, profileDigest, known := contract.Profile(profileID)
	if !known {
		t.Fatalf("unknown profile %q", profileID)
	}
	proof := &RuntimeProof{
		ProofKind:            ProofExternalFixtureTrust,
		ExternalFixtureTrust: testExternalFixtureTrustProof(),
	}
	return RuntimeAttestation{
		SchemaVersion:       RuntimeAttestationSchemaVersion,
		RunID:               runID,
		ManifestSHA256:      contract.ManifestSHA256(),
		ProfileID:           profileID,
		ProfileSHA256:       profileDigest,
		AuthorityID:         profile.Authority.AuthorityID,
		AuthorityKind:       profile.Authority.AuthorityKind,
		PrerequisiteOutcome: PrerequisiteSatisfied,
		Proof:               proof,
		Failure:             nil,
	}
}

type testExternalFixtureLiveProof struct {
	ProbeID                  string
	AttestationPublicKeySPKI string
	SignedAttestation        browsermatrixfixture.AuthorityProbeResponse
	AttestationSHA256        string
	AuthorityInstanceID      string
	RemoteServiceInstanceID  string
	ConfigurationSHA256      string
	NetworkBindingSHA256     string
	RemotePeerBindingSHA256  string
	ControllerPublicIP       string
	LeaseExpiresAt           string
}

func testExternalFixtureTrustProof() ExternalFixtureTrustProof {
	return ExternalFixtureTrustProof{
		ControllerOrigin:              testFixtureControllerOrigin,
		TLSCertificateSHA256:          testFixtureTLSCertificateSHA256,
		TLSCertificateAuthoritySHA256: testFixtureTLSCertificateAuthoritySHA256,
		AttestationPublicKeySPKI:      testFixtureAttestationPublicKeySPKI,
		AttestationPublicKeySHA256:    testFixtureAttestationPublicKeySHA256,
	}
}

func signedFixtureProof(t *testing.T, runID, profileID string) testExternalFixtureLiveProof {
	t.Helper()
	fixture := testExternalFixture(profileID)
	attestation := browsermatrixfixture.LiveExternalFixtureAttestation{
		SchemaVersion: browsermatrixfixture.LiveAttestationSchemaVersion,
		RunID:         runID,
		Nonce:         "nonce-" + profileID + "-fixture",
		LeaseID:       "lease-" + profileID + "-fixture",
		LeaseMillis:   testFixtureLeaseMillis,
		IssuedAt:      testFixtureIssuedAt,
		ExpiresAt:     testFixtureExpiresAt,
		Fixture:       fixture,
	}
	signed, err := browsermatrixfixture.SignLiveAttestation(
		attestation,
		testFixtureAttestationPrivateKey(t),
	)
	if err != nil {
		t.Fatalf("sign external fixture attestation: %v", err)
	}
	networkBinding, err := browsermatrixfixture.NetworkBindingSHA256(fixture)
	if err != nil {
		t.Fatalf("network binding: %v", err)
	}
	remotePeerBinding, err := browsermatrixfixture.RemotePeerBindingSHA256FromFixture(fixture)
	if err != nil {
		t.Fatalf("remote peer binding: %v", err)
	}
	configuration := testSignedExternalFixtureConfigurationSHA256(t, fixture, signed.AttestationSHA256)
	return testExternalFixtureLiveProof{
		ProbeID:                  "probe-" + signed.AttestationSHA256,
		AttestationPublicKeySPKI: testFixtureAttestationPublicKeySPKI,
		SignedAttestation:        signed, AttestationSHA256: signed.AttestationSHA256,
		AuthorityInstanceID:     fixture.AuthorityInstanceID,
		RemoteServiceInstanceID: fixture.RemoteServiceInstanceID,
		ConfigurationSHA256:     configuration, NetworkBindingSHA256: networkBinding,
		RemotePeerBindingSHA256: remotePeerBinding,
		ControllerPublicIP:      fixture.ControllerPublicIP, LeaseExpiresAt: attestation.ExpiresAt,
	}
}

func testSignedExternalFixtureConfigurationSHA256(
	t *testing.T,
	fixture browsermatrixfixture.ExternalFixture,
	attestationSHA256 string,
) string {
	t.Helper()
	const configurationSchema = "windshare.browser-network-matrix.external-fixture-configuration/v1"
	transportPolicy := "all"
	iceServerURLs := make([][]string, 0)
	switch fixture.NetworkSemantics.Kind {
	case browsermatrixfixture.NetworkSemanticsPublicSTUN,
		browsermatrixfixture.NetworkSemanticsManualRealNAT:
		iceServerURLs = append(iceServerURLs, []string{fixture.NetworkSemantics.STUNEndpoint})
	case browsermatrixfixture.NetworkSemanticsRestrictedUDP:
	case browsermatrixfixture.NetworkSemanticsCoturnRelay:
		transportPolicy = "relay"
		iceServerURLs = append(iceServerURLs, cloneStrings(fixture.NetworkSemantics.TURNURLs))
	default:
		t.Fatal("external fixture network semantics are invalid")
	}
	document, err := marshalCanonicalDocument(struct {
		SchemaVersion      string     `json:"schemaVersion"`
		ProfileID          string     `json:"profileId"`
		AttestationSHA256  string     `json:"attestationSha256"`
		ICETransportPolicy string     `json:"iceTransportPolicy"`
		ICEServerURLs      [][]string `json:"iceServerUrls"`
	}{
		SchemaVersion:      configurationSchema,
		ProfileID:          fixture.ProfileID,
		AttestationSHA256:  attestationSHA256,
		ICETransportPolicy: transportPolicy,
		ICEServerURLs:      iceServerURLs,
	})
	if err != nil {
		t.Fatalf("external fixture configuration: %v", err)
	}
	return sha256Text(document)
}

func testFixtureAttestationPrivateKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	seed := sha256.Sum256([]byte(testFixtureAuthoritySeedDomain))
	privateKey := ed25519.NewKeyFromSeed(seed[:])
	publicDER, err := x509.MarshalPKIXPublicKey(privateKey.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	if base64.RawURLEncoding.EncodeToString(publicDER) != testFixtureAttestationPublicKeySPKI ||
		sha256Text(publicDER) != testFixtureAttestationPublicKeySHA256 {
		t.Fatal("deterministic external fixture test authority changed")
	}
	return privateKey
}

func testExternalFixture(profileID string) browsermatrixfixture.ExternalFixture {
	identity := strings.TrimPrefix(strings.TrimPrefix(profileID, "scheduled-"), "manual-")
	semantics := browsermatrixfixture.NetworkSemantics{PolicyVersion: 1}
	switch profileID {
	case string(ProfileScheduledPublicSTUN):
		semantics.Kind = browsermatrixfixture.NetworkSemanticsPublicSTUN
		semantics.PolicyID = "public-stun-policy"
		semantics.STUNEndpoint = "stun:stun.cloudflare.com:3478"
	case string(ProfileScheduledRestrictedUDP):
		semantics.Kind = browsermatrixfixture.NetworkSemanticsRestrictedUDP
		semantics.PolicyID = "restricted-udp-policy"
		semantics.OutboundUDP = "denied"
		semantics.InboundUDP = "denied"
		semantics.RelayAccess = "denied"
	case string(ProfileScheduledCoturn):
		semantics.Kind = browsermatrixfixture.NetworkSemanticsCoturnRelay
		semantics.PolicyID = "coturn-relay-policy"
		semantics.TURNServiceOwnerID = "coturn-service-owner"
		semantics.TURNURLs = []string{"turn:turn.browser-matrix.test:3478?transport=udp"}
		semantics.TURNUsername = "test-turn-user"
		semantics.TURNCredentialID = "test-turn-credential"
		semantics.TURNCredentialExpiresAt = testFixtureExpiresAt
	case string(ProfileManualRealNAT):
		semantics.Kind = browsermatrixfixture.NetworkSemanticsManualRealNAT
		semantics.PolicyID = "operator-real-nat-policy"
		semantics.SenderHostID = "manual-sender-host"
		semantics.SenderNetworkBoundaryID = "manual-sender-network"
		semantics.STUNEndpoint = "stun:stun.cloudflare.com:3478"
	default:
		panic("unknown external fixture test profile")
	}
	return browsermatrixfixture.ExternalFixture{
		SchemaVersion: browsermatrixfixture.ExternalFixtureSchemaVersion,
		DeploymentID:  identity + "-deployment", Revision: 1, ProfileID: profileID,
		AuthorityInstanceID:     identity + "-authority",
		ImplementationSHA256:    sha256Text([]byte("implementation:" + profileID)),
		RemoteServiceInstanceID: identity + "-remote-pion", OperatorID: "windshare-test-operator",
		FixtureHostID: identity + "-fixture-host", FixtureNetworkBoundaryID: identity + "-fixture-network",
		ControllerOrigin: "https://browser-matrix.test/", ControllerPublicIP: "8.8.8.8",
		TLSCertificateSHA256: sha256Text([]byte("browser-matrix-test-certificate")),
		RemotePeerPublicIP:   "1.1.1.1", RemotePeerUDPPortMin: 40_000, RemotePeerUDPPortMax: 40_099,
		NetworkSemantics: semantics,
	}
}

func unsatisfiedAttestation(
	t *testing.T,
	contract Contract,
	runID, profileID string,
	outcome PrerequisiteOutcome,
) RuntimeAttestation {
	t.Helper()
	profile, profileDigest, known := contract.Profile(profileID)
	if !known {
		t.Fatalf("unknown profile %q", profileID)
	}
	code := FailureAuthorityNotProvisioned
	switch outcome {
	case PrerequisiteInvalid:
		code = FailureProofInvalid
	case PrerequisiteFailed:
		code = FailureRuntimeCheckFailed
	}
	return RuntimeAttestation{
		SchemaVersion:       RuntimeAttestationSchemaVersion,
		RunID:               runID,
		ManifestSHA256:      contract.ManifestSHA256(),
		ProfileID:           profileID,
		ProfileSHA256:       profileDigest,
		AuthorityID:         profile.Authority.AuthorityID,
		AuthorityKind:       profile.Authority.AuthorityKind,
		PrerequisiteOutcome: outcome,
		Proof:               nil,
		Failure:             &RuntimeFailure{FailureKind: outcome, FailureCode: code},
	}
}

func observedSample(
	t *testing.T,
	contract Contract,
	attestation RuntimeAttestation,
	identity SampleIdentity,
) SampleResult {
	t.Helper()
	profile, profileDigest, known := contract.Profile(identity.ProfileID)
	if !known {
		t.Fatalf("unknown profile %q", identity.ProfileID)
	}
	processInstanceID := fmt.Sprintf(
		"process-%s-%s-%d",
		identity.ProfileID,
		identity.Browser,
		identity.SampleOrdinal,
	)
	attemptID := fmt.Sprintf(
		"attempt-%s-%s-%d",
		identity.ProfileID,
		identity.Browser,
		identity.SampleOrdinal,
	)
	attemptEvidence := matchingAttemptEvidence(
		t, profile.ProfileKind, attemptID, attestation, identity, processInstanceID,
	)
	outcome, rationales, err := EvaluateCandidatePath(
		profile.CandidatePolicy,
		profile.ConnectivityExpectation,
		attemptEvidence.BrowserSelectedPair,
	)
	if err != nil {
		t.Fatalf("EvaluateCandidatePath: %v", err)
	}
	attestationDigest, err := attestation.SHA256(contract)
	if err != nil {
		t.Fatalf("attestation SHA256: %v", err)
	}
	return SampleResult{
		SchemaVersion:          SampleResultSchemaVersion,
		RunID:                  attestation.RunID,
		ManifestSHA256:         contract.ManifestSHA256(),
		Identity:               identity,
		ProfileSHA256:          profileDigest,
		AttestationSHA256:      attestationDigest,
		SampleOutcome:          SampleObserved,
		ProcessInstanceID:      &processInstanceID,
		AttemptEvidence:        &attemptEvidence,
		CandidatePolicyOutcome: outcome,
		RationaleCodes:         rationales,
		Failure:                nil,
	}
}

func matchingAttemptEvidence(
	t *testing.T,
	kind ProfileKind,
	attemptID string,
	attestation RuntimeAttestation,
	identity SampleIdentity,
	processInstanceID string,
) AttemptEvidence {
	t.Helper()
	if attestation.Proof == nil {
		t.Fatal("matching attempt requires a satisfied external fixture proof")
	}
	trust := attestation.Proof.ExternalFixtureTrust
	liveProof := signedFixtureProof(t, attestation.RunID, attestation.ProfileID)
	if liveProof.AttestationPublicKeySPKI != trust.AttestationPublicKeySPKI {
		t.Fatal("sample fixture key differs from its runtime trust anchor")
	}
	fixture := liveProof.SignedAttestation.Attestation.Fixture
	browserPair := matchingCandidatePath(kind)
	challenge := strings.Replace(attemptID, "attempt-", "challenge-", 1)
	sampleAuthority, err := browsermatrixpion.NewSampleAuthority(
		attestation.RunID,
		attestation.ProfileID,
		string(identity.Browser),
		identity.SampleOrdinal,
		processInstanceID,
	)
	if err != nil {
		t.Fatalf("sample authority: %v", err)
	}
	attemptAuthority := browsermatrixpion.AttemptAuthority{
		SchemaVersion: browsermatrixpion.AttemptAuthoritySchemaVersion,
		RequestAuthority: browsermatrixpion.AttemptRequestAuthority{
			SchemaVersion: browsermatrixpion.AttemptRequestAuthoritySchemaVersion,
			ControlAuthority: browsermatrixpion.ControlAuthority{
				SchemaVersion:   browsermatrixpion.ControlAuthoritySchemaVersion,
				SampleAuthority: sampleAuthority,
				ControlLeaseID:  strings.Replace(attemptID, "attempt-", "control-", 1),
			},
			RequestID: strings.Replace(attemptID, "attempt-", "request-", 1),
			FixtureBinding: browsermatrixpion.AttemptFixtureBinding{
				AttestationSHA256:       liveProof.AttestationSHA256,
				AuthorityInstanceID:     liveProof.AuthorityInstanceID,
				RemoteServiceInstanceID: liveProof.RemoteServiceInstanceID,
				NetworkBindingSHA256:    liveProof.NetworkBindingSHA256,
				RemotePeerBindingSHA256: liveProof.RemotePeerBindingSHA256,
			},
		},
		AttemptID: attemptID,
		Challenge: challenge,
	}
	bindingSHA256 := testChallengeBindingSHA256(t, attemptAuthority)
	pionPair := PionSelectedPair{SelectedPair: SelectedPairAbsent}
	var terminalPair *browsermatrixpion.SelectedPairEvidence
	var challengeProof *ChallengeProof
	state := "failed"
	failureCode := "ice-failed"
	var terminalFailure *string = &failureCode
	if kind != ProfileScheduledRestrictedUDP {
		pionLocal := *browserPair.RemoteCandidateType
		pionRemote := *browserPair.LocalCandidateType
		protocol := *browserPair.Protocol
		localFamily := AddressFamilyIPv4
		remoteFamily := AddressFamilyIPv4
		localAddress, localPort := fixture.RemotePeerPublicIP, fixture.RemotePeerUDPPortMin
		remoteAddress, remotePort := *browserPair.LocalAddress, *browserPair.LocalPort
		pionPair = PionSelectedPair{
			SelectedPair:       SelectedPairPresent,
			LocalCandidateType: &pionLocal, LocalAddressFamily: &localFamily,
			RemoteCandidateType: &pionRemote, RemoteAddressFamily: &remoteFamily,
			Protocol: &protocol, LocalAddress: &localAddress, LocalPort: &localPort,
			RemoteAddress: &remoteAddress, RemotePort: &remotePort,
		}
		terminalPair = &browsermatrixpion.SelectedPairEvidence{
			Local: browsermatrixpion.CandidateEvidence{
				CandidateType: string(pionLocal), Protocol: string(protocol),
				Address: localAddress, Port: localPort, AddressFamily: string(localFamily),
			},
			Remote: browsermatrixpion.CandidateEvidence{
				CandidateType: string(pionRemote), Protocol: string(protocol),
				Address: remoteAddress, Port: remotePort, AddressFamily: string(remoteFamily),
			},
		}
		challengeProof = &ChallengeProof{
			BindingSHA256: bindingSHA256, Challenge: challenge,
			PionChallengeObserved: true, BrowserEchoObserved: true,
		}
		state = "established"
		terminalFailure = nil
	}
	receipt, err := browsermatrixpion.SignAttemptTerminalReceipt(
		browsermatrixpion.AttemptTerminalReceipt{
			ProtocolVersion:       browsermatrixpion.ProtocolVersion,
			AttemptAuthority:      attemptAuthority,
			TerminalAt:            "2026-07-29T00:00:02.000Z",
			AttemptLeaseIssuedAt:  "2026-07-29T00:00:01.000Z",
			AttemptLeaseExpiresAt: "2026-07-29T00:01:01.000Z",
			AttemptLeaseMillis:    60_000, State: state, SelectedPair: terminalPair,
			ChallengeBindingSHA256: bindingSHA256,
			FailureCode:            terminalFailure,
		},
		testFixtureAttestationPrivateKey(t),
	)
	if err != nil {
		t.Fatalf("sign matching attempt terminal receipt: %v", err)
	}
	return AttemptEvidence{
		AttemptAuthority: attemptAuthority,
		PionAuthority:    PionAuthorityExternalRemote,
		ExternalFixture: ExternalFixtureAttemptBinding{
			RunID: attestation.RunID, AuthorityInstanceID: liveProof.AuthorityInstanceID,
			RemoteServiceInstanceID:  liveProof.RemoteServiceInstanceID,
			AttestationSHA256:        liveProof.AttestationSHA256,
			AttestationPublicKeySPKI: liveProof.AttestationPublicKeySPKI,
			SignedAttestation:        liveProof.SignedAttestation,
			NetworkBindingSHA256:     liveProof.NetworkBindingSHA256,
			RemotePeerBindingSHA256:  liveProof.RemotePeerBindingSHA256,
			ControllerPublicIP:       liveProof.ControllerPublicIP,
			AttestationExpiresAt:     liveProof.LeaseExpiresAt,
			RemotePeerPublicIP:       fixture.RemotePeerPublicIP,
			RemotePeerUDPPortMin:     fixture.RemotePeerUDPPortMin,
			RemotePeerUDPPortMax:     fixture.RemotePeerUDPPortMax,
		},
		BrowserSelectedPair: browserPair,
		PionSelectedPair:    pionPair,
		Challenge:           challengeProof,
		TerminalReceipt:     receipt,
	}
}

func testChallengeBindingSHA256(
	t *testing.T,
	attemptAuthority browsermatrixpion.AttemptAuthority,
) string {
	t.Helper()
	document, err := browsermatrixpion.CanonicalAttemptAuthorityDocument(
		attemptAuthority,
	)
	if err != nil {
		t.Fatalf("canonical challenge binding: %v", err)
	}
	return sha256Text(document)
}

func matchingCandidatePath(kind ProfileKind) CandidatePath {
	if kind == ProfileScheduledRestrictedUDP {
		return CandidatePath{SelectedPair: SelectedPairAbsent}
	}
	local := CandidateSRFLX
	if kind == ProfileScheduledCoturn {
		local = CandidateRelay
	}
	remote := CandidateHost
	protocol := ProtocolUDP
	localAddress, localPort := "8.8.4.4", uint16(50_000)
	remoteAddress, remotePort := "1.1.1.1", uint16(40_000)
	return CandidatePath{
		SelectedPair:        SelectedPairPresent,
		LocalCandidateType:  &local,
		LocalAddress:        &localAddress,
		LocalPort:           &localPort,
		RemoteCandidateType: &remote,
		RemoteAddress:       &remoteAddress,
		RemotePort:          &remotePort,
		Protocol:            &protocol,
	}
}

func buildRun(
	t *testing.T,
	contract Contract,
	mode ExecutionMode,
	runID string,
	outcomes []PrerequisiteOutcome,
	orchestration OrchestrationOutcome,
) RunResult {
	t.Helper()
	profileIDs := contract.profileIDs(mode)
	if len(outcomes) != len(profileIDs) {
		t.Fatalf("outcome count %d, want %d", len(outcomes), len(profileIDs))
	}
	expected, err := contract.ExpectedIdentities(mode)
	if err != nil {
		t.Fatalf("ExpectedIdentities: %v", err)
	}
	attestations := make([]RuntimeAttestation, len(profileIDs))
	for index, profileID := range profileIDs {
		if outcomes[index] == PrerequisiteSatisfied {
			attestations[index] = satisfiedAttestation(t, contract, runID, profileID)
		} else {
			attestations[index] = unsatisfiedAttestation(t, contract, runID, profileID, outcomes[index])
		}
	}
	samples := make([]SampleResult, 0, len(expected))
	for _, identity := range expected {
		profileIndex := findStringIndex(profileIDs, identity.ProfileID)
		if outcomes[profileIndex] == PrerequisiteSatisfied {
			samples = append(samples, observedSample(t, contract, attestations[profileIndex], identity))
		}
	}
	result := RunResult{
		SchemaVersion:        RunResultSchemaVersion,
		RunID:                runID,
		ManifestSHA256:       contract.ManifestSHA256(),
		ExecutionMode:        mode,
		OrchestrationOutcome: orchestration,
		ExpectedIdentities:   expected,
		RuntimeAttestations:  attestations,
		Samples:              samples,
	}
	if orchestration == OrchestrationFailed {
		result.OrchestrationFailure = &OrchestrationFailure{FailureCode: FailureCollector}
	}
	refreshRunSummary(t, &result)
	return result
}

func refreshRunSummary(t *testing.T, result *RunResult) {
	t.Helper()
	sampleCounts := make([]int, len(result.RuntimeAttestations))
	for _, sample := range result.Samples {
		profileIndex := findAttestationIndex(result.RuntimeAttestations, sample.Identity.ProfileID)
		if profileIndex < 0 {
			t.Fatalf("sample profile %q has no attestation", sample.Identity.ProfileID)
		}
		sampleCounts[profileIndex]++
	}
	profileResults, runOutcome, err := deriveRunSummary(*result, sampleCounts)
	if err != nil {
		t.Fatalf("derive run summary: %v", err)
	}
	result.ProfileResults = profileResults
	result.RunOutcome = runOutcome
}
