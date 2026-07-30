package browsernetworktopology

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

var ErrInvalidRuntimeAttestation = errors.New("invalid browser network runtime attestation")

type PrerequisiteOutcome string

const (
	PrerequisiteSatisfied   PrerequisiteOutcome = "satisfied"
	PrerequisiteUnavailable PrerequisiteOutcome = "unavailable"
	PrerequisiteInvalid     PrerequisiteOutcome = "invalid"
	PrerequisiteFailed      PrerequisiteOutcome = "failed"
)

type RuntimeProofKind string

const (
	ProofExternalFixtureTrust RuntimeProofKind = "external-fixture-trust"
)

type ExternalFixtureTrustProof struct {
	ControllerOrigin              string `json:"controllerOrigin"`
	TLSCertificateSHA256          string `json:"tlsCertificateSha256"`
	TLSCertificateAuthoritySHA256 string `json:"tlsCertificateAuthoritySha256"`
	AttestationPublicKeySPKI      string `json:"attestationPublicKeySpki"`
	AttestationPublicKeySHA256    string `json:"attestationPublicKeySha256"`
}

type RuntimeProof struct {
	ProofKind            RuntimeProofKind          `json:"proofKind"`
	ExternalFixtureTrust ExternalFixtureTrustProof `json:"externalFixtureTrust"`
}

type RuntimeFailureCode string

const (
	FailureAuthorityAttestationExpired  RuntimeFailureCode = "authority-attestation-expired"
	FailureAuthorityKeyRotationRequired RuntimeFailureCode = "authority-key-rotation-required"
	FailureAuthorityNotProvisioned      RuntimeFailureCode = "authority-not-provisioned"
	FailureAuthorityUnreachable         RuntimeFailureCode = "authority-unreachable"
	FailureAuthorityBindingMismatch     RuntimeFailureCode = "authority-binding-mismatch"
	FailureProofInvalid                 RuntimeFailureCode = "proof-invalid"
	FailureAuthorityProbeFailed         RuntimeFailureCode = "authority-probe-failed"
	FailureRuntimeCheckFailed           RuntimeFailureCode = "runtime-check-failed"
	FailurePrerequisiteBootstrap        RuntimeFailureCode = "runtime-bootstrap-failed"
)

type RuntimeFailure struct {
	FailureKind PrerequisiteOutcome `json:"failureKind"`
	FailureCode RuntimeFailureCode  `json:"failureCode"`
}

type RuntimeAttestation struct {
	SchemaVersion       string              `json:"schemaVersion"`
	RunID               string              `json:"runId"`
	ManifestSHA256      string              `json:"manifestSha256"`
	ProfileID           string              `json:"profileId"`
	ProfileSHA256       string              `json:"profileSha256"`
	AuthorityID         string              `json:"authorityId"`
	AuthorityKind       AuthorityKind       `json:"authorityKind"`
	PrerequisiteOutcome PrerequisiteOutcome `json:"prerequisiteOutcome"`
	Proof               *RuntimeProof       `json:"proof"`
	Failure             *RuntimeFailure     `json:"failure"`
}

func ParseRuntimeAttestation(encoded []byte, contract Contract) (RuntimeAttestation, error) {
	var attestation RuntimeAttestation
	if err := decodeCanonicalDocument(
		encoded, "browser network runtime attestation", &attestation, ErrInvalidRuntimeAttestation,
	); err != nil {
		return RuntimeAttestation{}, err
	}
	if err := attestation.Validate(contract); err != nil {
		return RuntimeAttestation{}, err
	}
	return attestation, nil
}

func (attestation RuntimeAttestation) Validate(contract Contract) error {
	profile, profileDigest, known := contract.Profile(attestation.ProfileID)
	if !known || attestation.SchemaVersion != RuntimeAttestationSchemaVersion ||
		!validIdentifier(attestation.RunID) || attestation.ManifestSHA256 != contract.ManifestSHA256() ||
		attestation.ProfileSHA256 != profileDigest || attestation.AuthorityID != profile.Authority.AuthorityID ||
		attestation.AuthorityKind != profile.Authority.AuthorityKind {
		return fmt.Errorf("%w: schema, run, manifest, profile, or authority binding differs", ErrInvalidRuntimeAttestation)
	}

	if attestation.PrerequisiteOutcome == PrerequisiteSatisfied {
		if attestation.Proof == nil || attestation.Failure != nil {
			return fmt.Errorf("%w: satisfied prerequisite needs proof and cannot carry failure", ErrInvalidRuntimeAttestation)
		}
		if err := attestation.Proof.Validate(profile.Authority); err != nil {
			return errors.Join(ErrInvalidRuntimeAttestation, err)
		}
		return nil
	}

	if attestation.Proof != nil || attestation.Failure == nil {
		return fmt.Errorf("%w: unsatisfied prerequisite cannot carry success proof and needs typed failure", ErrInvalidRuntimeAttestation)
	}
	if err := attestation.Failure.Validate(attestation.PrerequisiteOutcome); err != nil {
		return errors.Join(ErrInvalidRuntimeAttestation, err)
	}
	return nil
}

func (proof RuntimeProof) Validate(authority AuthorityReference) error {
	if authority.AuthorityKind != AuthorityExternalFixture {
		return fmt.Errorf("runtime proof authority kind is unknown")
	}
	if proof.ProofKind != ProofExternalFixtureTrust {
		return fmt.Errorf("external fixture proof does not match its authority binding")
	}
	if err := proof.ExternalFixtureTrust.validate(authority); err != nil {
		return err
	}
	return nil
}

func (proof ExternalFixtureTrustProof) validate(authority AuthorityReference) error {
	if !canonicalHTTPSOrigin(proof.ControllerOrigin) ||
		!isCanonicalSHA256(proof.TLSCertificateSHA256) ||
		!isCanonicalSHA256(proof.TLSCertificateAuthoritySHA256) ||
		proof.AttestationPublicKeySHA256 != authority.AttestationPublicKeySHA256 {
		return fmt.Errorf("external fixture local trust declaration is invalid")
	}
	if _, err := parseAttestationPublicKeySPKI(
		proof.AttestationPublicKeySPKI,
		proof.AttestationPublicKeySHA256,
	); err != nil {
		return fmt.Errorf("external fixture local trust anchor is invalid")
	}
	return nil
}

func canonicalHTTPSOrigin(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Host == "" ||
		parsed.Hostname() == "" || parsed.Path != "/" || parsed.RawQuery != "" || parsed.ForceQuery ||
		parsed.Fragment != "" || parsed.RawFragment != "" || parsed.String() != value {
		return false
	}
	host := parsed.Hostname()
	if strings.Contains(host, ":") || strings.ToLower(host) != host {
		return false
	}
	for _, character := range host {
		if character > 127 {
			return false
		}
	}
	if strings.Trim(host, "0123456789.") == "" {
		address, parseErr := netip.ParseAddr(host)
		if parseErr != nil || !address.Is4() || address.String() != host {
			return false
		}
	}
	port := parsed.Port()
	if port == "443" {
		return false
	}
	if port != "" {
		parsedPort, parseErr := strconv.Atoi(port)
		if parseErr != nil || parsedPort < 0 || parsedPort > 65_535 || strconv.Itoa(parsedPort) != port {
			return false
		}
	}
	expected := "https://" + host
	if port != "" {
		expected += ":" + port
	}
	return value == expected+"/"
}

func parseAttestationPublicKeySPKI(encoded, expectedSHA256 string) (ed25519.PublicKey, error) {
	if len(encoded) < 32 || len(encoded) > 256 || !isCanonicalSHA256(expectedSHA256) {
		return nil, errors.New("external fixture attestation trust anchor is invalid")
	}
	der, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(der) != encoded {
		return nil, errors.New("external fixture attestation SPKI is not canonical base64url")
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, errors.New("external fixture attestation SPKI is invalid")
	}
	publicKey, ok := parsed.(ed25519.PublicKey)
	if !ok || len(publicKey) != ed25519.PublicKeySize {
		return nil, errors.New("external fixture attestation key is not Ed25519")
	}
	canonical, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil || !bytes.Equal(canonical, der) || sha256Text(canonical) != expectedSHA256 {
		return nil, errors.New("external fixture attestation key differs from its pinned digest")
	}
	return append(ed25519.PublicKey(nil), publicKey...), nil
}

func (failure RuntimeFailure) Validate(outcome PrerequisiteOutcome) error {
	if failure.FailureKind != outcome {
		return fmt.Errorf("runtime failure kind differs from prerequisite outcome")
	}
	valid := false
	switch outcome {
	case PrerequisiteUnavailable:
		valid = failure.FailureCode == FailureAuthorityAttestationExpired ||
			failure.FailureCode == FailureAuthorityKeyRotationRequired ||
			failure.FailureCode == FailureAuthorityNotProvisioned ||
			failure.FailureCode == FailureAuthorityUnreachable
	case PrerequisiteInvalid:
		valid = failure.FailureCode == FailureAuthorityBindingMismatch ||
			failure.FailureCode == FailureProofInvalid
	case PrerequisiteFailed:
		valid = failure.FailureCode == FailureAuthorityProbeFailed ||
			failure.FailureCode == FailureRuntimeCheckFailed ||
			failure.FailureCode == FailurePrerequisiteBootstrap
	}
	if !valid {
		return fmt.Errorf("runtime failure code does not belong to outcome %q", outcome)
	}
	return nil
}

func (attestation RuntimeAttestation) CanonicalJSON(contract Contract) ([]byte, error) {
	if err := attestation.Validate(contract); err != nil {
		return nil, err
	}
	return marshalCanonicalDocument(attestation)
}

func (attestation RuntimeAttestation) SHA256(contract Contract) (string, error) {
	encoded, err := attestation.CanonicalJSON(contract)
	if err != nil {
		return "", err
	}
	return sha256Text(encoded), nil
}
