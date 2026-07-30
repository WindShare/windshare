package browsermatrixpion

import (
	"bytes"
	"crypto/ed25519"
	"strings"
	"testing"
	"time"
)

func TestSignedExternalFixtureAttestationRoundTrip(t *testing.T) {
	fixture := testExternalFixture("scheduled-public-stun")
	privateKey := testAttestationPrivateKey()
	issuedAt := time.Date(2029, 1, 2, 3, 4, 5, 600_000_000, time.UTC)
	attestation := testLiveAttestation(fixture, issuedAt)
	signed, err := SignLiveAttestation(attestation, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := VerifyAuthorityProbeResponse(
		signed, privateKey.Public().(ed25519.PublicKey), issuedAt.Add(time.Second),
		attestation.RunID, attestation.Nonce,
	)
	if err != nil {
		t.Fatal(err)
	}
	wantNetwork, err := NetworkBindingSHA256(fixture)
	if err != nil {
		t.Fatal(err)
	}
	wantRemote, err := RemotePeerBindingSHA256FromFixture(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if verified.AttestationSHA256 != signed.AttestationSHA256 ||
		verified.NetworkBindingSHA256 != wantNetwork || verified.RemotePeerBindingSHA256 != wantRemote ||
		len(signed.Signature) != 86 {
		t.Fatalf("verified authority bindings are incomplete: %#v", verified)
	}
}

func TestSignedAttestationRejectsHostileEnvelopeExpiryAndSignature(t *testing.T) {
	privateKey := testAttestationPrivateKey()
	issuedAt := time.Date(2029, 1, 2, 3, 4, 5, 0, time.UTC)
	attestation := testLiveAttestation(testExternalFixture("scheduled-public-stun"), issuedAt)
	signed, err := SignLiveAttestation(attestation, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	mutations := map[string]func(AuthorityProbeResponse) AuthorityProbeResponse{
		"protocol": func(value AuthorityProbeResponse) AuthorityProbeResponse {
			value.ProtocolVersion = "windshare.browser-network-matrix.remote-pion/v1"
			return value
		},
		"digest": func(value AuthorityProbeResponse) AuthorityProbeResponse {
			value.AttestationSHA256 = strings.Repeat("0", 64)
			return value
		},
		"signature": func(value AuthorityProbeResponse) AuthorityProbeResponse {
			value.Signature = strings.Repeat("A", 86)
			return value
		},
		"fixture": func(value AuthorityProbeResponse) AuthorityProbeResponse {
			value.Attestation.Fixture.Revision++
			return value
		},
		"run": func(value AuthorityProbeResponse) AuthorityProbeResponse {
			value.Attestation.RunID = "different-run-0001"
			return value
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			if _, err := VerifyAuthorityProbeResponse(
				mutate(signed), publicKey, issuedAt.Add(time.Second), attestation.RunID, attestation.Nonce,
			); err == nil {
				t.Fatal("hostile attestation accepted")
			}
		})
	}
	wrongPublic, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyAuthorityProbeResponse(
		signed, wrongPublic, issuedAt.Add(time.Second), attestation.RunID, attestation.Nonce,
	); err == nil {
		t.Fatal("attestation signed by an unpinned key was accepted")
	}
	if _, err := VerifyAuthorityProbeResponse(
		signed, publicKey, issuedAt.Add(2*time.Minute), attestation.RunID, attestation.Nonce,
	); err == nil {
		t.Fatal("expired attestation accepted")
	}
	if _, err := VerifyAuthorityProbeResponse(
		signed, publicKey, issuedAt.Add(-maximumClockSkew-time.Millisecond), attestation.RunID, attestation.Nonce,
	); err == nil {
		t.Fatal("attestation issued beyond the clock-skew authority was accepted")
	}
}

func TestExternalFixtureCanonicalizationCoversEverySemanticProfile(t *testing.T) {
	for _, profileID := range []string{
		"scheduled-public-stun", "scheduled-restricted-udp", "scheduled-coturn", "manual-real-nat",
	} {
		t.Run(profileID, func(t *testing.T) {
			fixture := testExternalFixture(profileID)
			document, err := CanonicalExternalFixtureDocument(fixture)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := ParseCanonicalExternalFixture(document)
			if err != nil {
				t.Fatal(err)
			}
			if parsed.ProfileID != profileID {
				t.Fatalf("parsed profile=%q", parsed.ProfileID)
			}
			if profileID == "scheduled-coturn" &&
				(bytes.Contains(document, []byte(`"turnCredential":`)) ||
					bytes.Contains(document, []byte("test-turn-secret"))) {
				t.Fatal("signed Coturn declaration serialized a credential secret")
			}
			unknown := append([]byte(nil), document...)
			unknown = bytes.Replace(unknown, []byte(`"revision":1`), []byte(`"revision":1,"unknown":true`), 1)
			if _, err := ParseCanonicalExternalFixture(unknown); err == nil {
				t.Fatal("unknown declaration field accepted")
			}
		})
	}
}

func TestNetworkAndRemotePeerBindingsAreIndependentCanonicalDocuments(t *testing.T) {
	fixture := testExternalFixture("scheduled-public-stun")
	network, err := NetworkBindingSHA256(fixture)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := RemotePeerBindingSHA256FromFixture(fixture)
	if err != nil {
		t.Fatal(err)
	}
	controllerChanged := fixture
	controllerChanged.ControllerPublicIP = "9.9.9.9"
	controllerNetwork, _ := NetworkBindingSHA256(controllerChanged)
	controllerRemote, _ := RemotePeerBindingSHA256FromFixture(controllerChanged)
	if controllerNetwork == network || controllerRemote != remote {
		t.Fatal("controller identity was not isolated from the remote-peer binding document")
	}
	remoteChanged := fixture
	remoteChanged.RemotePeerUDPPortMax++
	changedNetwork, _ := NetworkBindingSHA256(remoteChanged)
	changedRemote, _ := RemotePeerBindingSHA256FromFixture(remoteChanged)
	if changedNetwork == network || changedRemote == remote {
		t.Fatal("remote endpoint mutation did not change both independently derived bindings")
	}
}

func TestCanonicalJSONMatchesJavaScriptStringEscaping(t *testing.T) {
	fixture := testExternalFixture("scheduled-coturn")
	fixture.NetworkSemantics.TURNUsername = "matrix<>&\u2028\u2029\\u2028"
	document, err := CanonicalExternalFixtureDocument(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(document, []byte(`\u003c`)) || bytes.Contains(document, []byte(`\u003e`)) ||
		bytes.Contains(document, []byte(`\u0026`)) || bytes.Contains(document, []byte(`\u2029`)) ||
		!bytes.Contains(document, []byte("<>&\u2028\u2029")) ||
		!bytes.Contains(document, []byte(`\\u2028`)) {
		t.Fatalf("canonical JSON does not match JSON.stringify escaping: %s", document)
	}
	if _, err := ParseCanonicalExternalFixture(document); err != nil {
		t.Fatal(err)
	}
}

func TestFixtureRejectsNoncanonicalOriginsIceURIsAndTimestamps(t *testing.T) {
	for name, mutate := range map[string]func(*ExternalFixture){
		"default TLS port": func(value *ExternalFixture) {
			value.ControllerOrigin = "https://matrix.example:443/"
		},
		"ambiguous origin port": func(value *ExternalFixture) {
			value.ControllerOrigin = "https://matrix.example:08443/"
		},
		"numeric origin": func(value *ExternalFixture) {
			value.ControllerOrigin = "https://999.999.999.999:8443/"
		},
		"documentation address": func(value *ExternalFixture) {
			value.ControllerPublicIP = "203.0.113.1"
		},
		"uppercase STUN scheme": func(value *ExternalFixture) {
			value.NetworkSemantics.STUNEndpoint = "STUN:stun.example:3478"
		},
		"uppercase STUN host": func(value *ExternalFixture) {
			value.NetworkSemantics.STUNEndpoint = "stun:STUN.example:3478"
		},
		"invalid numeric ICE host": func(value *ExternalFixture) {
			value.NetworkSemantics.STUNEndpoint = "stun:999.999.999.999:3478"
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := testExternalFixture("scheduled-public-stun")
			mutate(&fixture)
			if err := ValidateExternalFixture(fixture); err == nil {
				t.Fatal("noncanonical external fixture accepted")
			}
		})
	}
	fixture := testExternalFixture("scheduled-coturn")
	fixture.NetworkSemantics.TURNURLs = []string{"turns:turn.example:5349?transport=udp"}
	if err := ValidateExternalFixture(fixture); err == nil {
		t.Fatal("TURN-over-TLS URI with UDP transport accepted")
	}
	if _, err := parseCanonicalTimestamp("2029-01-02T03:04:05Z"); err == nil {
		t.Fatal("non-millisecond timestamp accepted")
	}
}

func testAttestationPrivateKey() ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x42}, ed25519.SeedSize))
}

func testLiveAttestation(fixture ExternalFixture, issuedAt time.Time) LiveExternalFixtureAttestation {
	return LiveExternalFixtureAttestation{
		SchemaVersion: LiveAttestationSchemaVersion,
		RunID:         "matrix-run-00000001", Nonce: "nonce-00000000001", LeaseID: "lease-00000000001",
		LeaseMillis: 60_000,
		IssuedAt:    issuedAt.UTC().Format(canonicalTimestampLayout),
		ExpiresAt:   issuedAt.UTC().Add(time.Minute).Format(canonicalTimestampLayout),
		Fixture:     fixture,
	}
}

func testExternalFixture(profileID string) ExternalFixture {
	semantics := NetworkSemantics{PolicyID: "matrix-policy", PolicyVersion: 1}
	switch profileID {
	case "scheduled-public-stun":
		semantics.Kind = NetworkSemanticsPublicSTUN
		semantics.STUNEndpoint = "stun:stun.example:3478"
	case "scheduled-restricted-udp":
		semantics.Kind = NetworkSemanticsRestrictedUDP
		semantics.OutboundUDP = "denied"
		semantics.InboundUDP = "denied"
		semantics.RelayAccess = "denied"
	case "scheduled-coturn":
		semantics.Kind = NetworkSemanticsCoturnRelay
		semantics.TURNServiceOwnerID = "turn-owner"
		semantics.TURNURLs = []string{
			"turn:turn.example:3478?transport=udp",
			"turns:turn.example:5349?transport=tcp",
		}
		semantics.TURNUsername = "matrix-user"
		semantics.TURNCredentialID = "turn-credential-2029"
		semantics.TURNCredentialExpiresAt = "2030-01-01T00:00:00.000Z"
	case "manual-real-nat":
		semantics.Kind = NetworkSemanticsManualRealNAT
		semantics.SenderHostID = "sender-host"
		semantics.SenderNetworkBoundaryID = "sender-boundary"
		semantics.STUNEndpoint = "stun:stun.example:3478"
	default:
		panic("unknown fixture test profile")
	}
	return ExternalFixture{
		SchemaVersion: ExternalFixtureSchemaVersion,
		DeploymentID:  "fixture-deployment", Revision: 1, ProfileID: profileID,
		AuthorityInstanceID: "fixture-authority", ImplementationSHA256: strings.Repeat("a", 64),
		RemoteServiceInstanceID: "remote-a", OperatorID: "fixture-operator",
		FixtureHostID: "remote-host", FixtureNetworkBoundaryID: "remote-boundary",
		ControllerOrigin: "https://matrix.example:8443/", ControllerPublicIP: "8.8.8.8",
		TLSCertificateSHA256: strings.Repeat("b", 64), RemotePeerPublicIP: "1.1.1.1",
		RemotePeerUDPPortMin: 41000, RemotePeerUDPPortMax: 41010,
		NetworkSemantics: semantics,
	}
}
