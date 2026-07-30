package browsermatrixpion

import (
	"crypto/ed25519"
	"time"

	"github.com/windshare/windshare/internal/browsermatrixfixture"
)

const (
	ExternalFixtureSchemaVersion   = browsermatrixfixture.ExternalFixtureSchemaVersion
	LiveAttestationSchemaVersion   = browsermatrixfixture.LiveAttestationSchemaVersion
	RemotePeerBindingSchemaVersion = browsermatrixfixture.RemotePeerBindingSchemaVersion
	AttestationSignatureAlgorithm  = browsermatrixfixture.AttestationSignatureAlgorithm
	NetworkSemanticsPublicSTUN     = browsermatrixfixture.NetworkSemanticsPublicSTUN
	NetworkSemanticsRestrictedUDP  = browsermatrixfixture.NetworkSemanticsRestrictedUDP
	NetworkSemanticsCoturnRelay    = browsermatrixfixture.NetworkSemanticsCoturnRelay
	NetworkSemanticsManualRealNAT  = browsermatrixfixture.NetworkSemanticsManualRealNAT
	canonicalTimestampLayout       = browsermatrixfixture.CanonicalTimestampLayout
	maximumClockSkew               = browsermatrixfixture.MaximumClockSkew
)

type ExternalFixture = browsermatrixfixture.ExternalFixture
type NetworkSemantics = browsermatrixfixture.NetworkSemantics
type LiveExternalFixtureAttestation = browsermatrixfixture.LiveExternalFixtureAttestation
type VerifiedAttestation = browsermatrixfixture.VerifiedAttestation
type AuthorityProbeResponse = browsermatrixfixture.AuthorityProbeResponse

func ValidateExternalFixture(fixture ExternalFixture) error {
	return browsermatrixfixture.ValidateExternalFixture(fixture)
}

func ParseCanonicalExternalFixture(document []byte) (ExternalFixture, error) {
	return browsermatrixfixture.ParseCanonicalExternalFixture(document)
}

func CanonicalExternalFixtureDocument(fixture ExternalFixture) ([]byte, error) {
	return browsermatrixfixture.CanonicalExternalFixtureDocument(fixture)
}

func NetworkBindingSHA256(fixture ExternalFixture) (string, error) {
	return browsermatrixfixture.NetworkBindingSHA256(fixture)
}

func RemotePeerBindingSHA256FromFixture(fixture ExternalFixture) (string, error) {
	return browsermatrixfixture.RemotePeerBindingSHA256FromFixture(fixture)
}

func CanonicalLiveAttestationDocument(attestation LiveExternalFixtureAttestation) ([]byte, error) {
	return browsermatrixfixture.CanonicalLiveAttestationDocument(attestation)
}

func SignLiveAttestation(
	attestation LiveExternalFixtureAttestation,
	privateKey ed25519.PrivateKey,
) (AuthorityProbeResponse, error) {
	return browsermatrixfixture.SignLiveAttestation(attestation, privateKey)
}

func VerifyAuthorityProbeResponse(
	response AuthorityProbeResponse,
	publicKey ed25519.PublicKey,
	now time.Time,
	expectedRunID string,
	expectedNonce string,
) (VerifiedAttestation, error) {
	return browsermatrixfixture.VerifyAuthorityProbeResponse(
		response, publicKey, now, expectedRunID, expectedNonce,
	)
}

func parseCanonicalTimestamp(value string) (time.Time, error) {
	return browsermatrixfixture.ParseCanonicalTimestamp(value)
}

func canonicalJSONLine(value any) ([]byte, error) {
	return browsermatrixfixture.CanonicalJSONLine(value)
}

func validICECredential(value string) bool {
	return browsermatrixfixture.ValidICECredential(value)
}

func validSTUNURI(value string) bool {
	return browsermatrixfixture.ValidSTUNURI(value)
}

func sha256Hex(document []byte) string {
	return browsermatrixfixture.SHA256Hex(document)
}
