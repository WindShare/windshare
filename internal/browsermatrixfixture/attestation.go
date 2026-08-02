package browsermatrixfixture

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	ProtocolVersion                = "windshare.browser-network-matrix.remote-pion/v3"
	ExternalFixtureSchemaVersion   = "windshare.browser-network-matrix.external-fixture-declaration/v2"
	LiveAttestationSchemaVersion   = "windshare.browser-network-matrix.external-fixture-attestation/v1"
	RemotePeerBindingSchemaVersion = "windshare.browser-network-matrix.external-remote-peer-binding/v1"
	AttestationSignatureAlgorithm  = "ed25519"
	NetworkSemanticsPublicSTUN     = "public-stun"
	NetworkSemanticsRestrictedUDP  = "restricted-udp"
	NetworkSemanticsCoturnRelay    = "coturn-relay"
	CanonicalTimestampLayout       = "2006-01-02T15:04:05.000Z"
	MaximumClockSkew               = 30 * time.Second
	MaximumAttestationLease        = 5 * time.Minute
	canonicalTimestampLayout       = CanonicalTimestampLayout
	maximumSafeJSONInteger         = uint64(9_007_199_254_740_991)
	maximumClockSkew               = MaximumClockSkew
	maximumLeaseLimit              = MaximumAttestationLease
)

var canonicalIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
var opaqueIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
var sha256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

var canonicalICEURIPattern = regexp.MustCompile(`^(stun|turn|turns):(\[[0-9a-f:.]+\]|[a-z0-9.-]+)(?::([0-9]{1,5}))?(?:\?transport=(udp|tcp))?$`)
var canonicalDNSLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

// ExternalFixture is the immutable operator declaration that gives runtime
// observations meaning. Keeping deployment identity, TLS, endpoint, and network
// policy in one signed object prevents independently plausible values
// from being spliced across deployments.
type ExternalFixture struct {
	SchemaVersion            string           `json:"schemaVersion"`
	DeploymentID             string           `json:"deploymentId"`
	Revision                 uint64           `json:"revision"`
	ProfileID                string           `json:"profileId"`
	AuthorityInstanceID      string           `json:"authorityInstanceId"`
	RemoteServiceInstanceID  string           `json:"remoteServiceInstanceId"`
	OperatorID               string           `json:"operatorId"`
	FixtureHostID            string           `json:"fixtureHostId"`
	FixtureNetworkBoundaryID string           `json:"fixtureNetworkBoundaryId"`
	ControllerOrigin         string           `json:"controllerOrigin"`
	ControllerPublicIP       string           `json:"controllerPublicIp"`
	TLSCertificateSHA256     string           `json:"tlsCertificateSha256"`
	RemotePeerPublicIP       string           `json:"remotePeerPublicIp"`
	RemotePeerUDPPortMin     uint16           `json:"remotePeerUdpPortMin"`
	RemotePeerUDPPortMax     uint16           `json:"remotePeerUdpPortMax"`
	NetworkSemantics         NetworkSemantics `json:"networkSemantics"`
}

// NetworkSemantics has one exact profile-selected shape. Optional JSON fields
// are a serialization detail only: validation rejects every field that does
// not belong to the selected discriminator.
type NetworkSemantics struct {
	Kind                    string   `json:"kind"`
	PolicyID                string   `json:"policyId"`
	PolicyVersion           uint64   `json:"policyVersion"`
	STUNEndpoint            string   `json:"stunEndpoint,omitempty"`
	OutboundUDP             string   `json:"outboundUdp,omitempty"`
	InboundUDP              string   `json:"unsolicitedInboundUdp,omitempty"`
	RelayAccess             string   `json:"relayAccess,omitempty"`
	TURNServiceOwnerID      string   `json:"turnServiceOwnerId,omitempty"`
	TURNURLs                []string `json:"turnUrls,omitempty"`
	TURNUsername            string   `json:"turnUsername,omitempty"`
	TURNCredentialID        string   `json:"turnCredentialId,omitempty"`
	TURNCredentialExpiresAt string   `json:"turnCredentialExpiresAt,omitempty"`
}

func (semantics NetworkSemantics) MarshalJSON() ([]byte, error) {
	switch semantics.Kind {
	case NetworkSemanticsPublicSTUN:
		return marshalCanonicalObject(struct {
			Kind          string `json:"kind"`
			PolicyID      string `json:"policyId"`
			PolicyVersion uint64 `json:"policyVersion"`
			STUNEndpoint  string `json:"stunEndpoint"`
		}{semantics.Kind, semantics.PolicyID, semantics.PolicyVersion, semantics.STUNEndpoint})
	case NetworkSemanticsRestrictedUDP:
		return marshalCanonicalObject(struct {
			Kind          string `json:"kind"`
			PolicyID      string `json:"policyId"`
			PolicyVersion uint64 `json:"policyVersion"`
			OutboundUDP   string `json:"outboundUdp"`
			InboundUDP    string `json:"unsolicitedInboundUdp"`
			RelayAccess   string `json:"relayAccess"`
		}{semantics.Kind, semantics.PolicyID, semantics.PolicyVersion,
			semantics.OutboundUDP, semantics.InboundUDP, semantics.RelayAccess})
	case NetworkSemanticsCoturnRelay:
		return marshalCanonicalObject(struct {
			Kind                    string   `json:"kind"`
			PolicyID                string   `json:"policyId"`
			PolicyVersion           uint64   `json:"policyVersion"`
			TURNServiceOwnerID      string   `json:"turnServiceOwnerId"`
			TURNURLs                []string `json:"turnUrls"`
			TURNUsername            string   `json:"turnUsername"`
			TURNCredentialID        string   `json:"turnCredentialId"`
			TURNCredentialExpiresAt string   `json:"turnCredentialExpiresAt"`
		}{semantics.Kind, semantics.PolicyID, semantics.PolicyVersion, semantics.TURNServiceOwnerID,
			semantics.TURNURLs, semantics.TURNUsername, semantics.TURNCredentialID,
			semantics.TURNCredentialExpiresAt})
	default:
		return nil, errors.New("external fixture network semantics discriminator is invalid")
	}
}

type LiveExternalFixtureAttestation struct {
	SchemaVersion string          `json:"schemaVersion"`
	RunID         string          `json:"runId"`
	Nonce         string          `json:"nonce"`
	LeaseID       string          `json:"leaseId"`
	LeaseMillis   int64           `json:"leaseMillis"`
	IssuedAt      string          `json:"issuedAt"`
	ExpiresAt     string          `json:"expiresAt"`
	Fixture       ExternalFixture `json:"fixture"`
}

type AuthorityProbeResponse struct {
	ProtocolVersion    string                         `json:"protocolVersion"`
	Attestation        LiveExternalFixtureAttestation `json:"attestation"`
	AttestationSHA256  string                         `json:"attestationSha256"`
	SignatureAlgorithm string                         `json:"signatureAlgorithm"`
	Signature          string                         `json:"signature"`
}

type VerifiedAttestation struct {
	Attestation             LiveExternalFixtureAttestation
	AttestationSHA256       string
	NetworkBindingSHA256    string
	RemotePeerBindingSHA256 string
	IssuedAt                time.Time
	ExpiresAt               time.Time
}

type remotePeerBinding struct {
	SchemaVersion            string `json:"schemaVersion"`
	DeploymentID             string `json:"deploymentId"`
	ProfileID                string `json:"profileId"`
	AuthorityInstanceID      string `json:"authorityInstanceId"`
	RemoteServiceInstanceID  string `json:"remoteServiceInstanceId"`
	FixtureHostID            string `json:"fixtureHostId"`
	FixtureNetworkBoundaryID string `json:"fixtureNetworkBoundaryId"`
	PublicIP                 string `json:"publicIp"`
	UDPPortMin               uint16 `json:"udpPortMin"`
	UDPPortMax               uint16 `json:"udpPortMax"`
}

func ValidateExternalFixture(fixture ExternalFixture) error {
	if fixture.SchemaVersion != ExternalFixtureSchemaVersion || fixture.Revision == 0 || fixture.Revision > maximumSafeJSONInteger ||
		!validProfileID(fixture.ProfileID) || !sha256Pattern.MatchString(fixture.TLSCertificateSHA256) {
		return errors.New("external fixture identity is invalid")
	}
	for label, value := range map[string]string{
		"deployment ID":               fixture.DeploymentID,
		"authority instance ID":       fixture.AuthorityInstanceID,
		"remote service instance ID":  fixture.RemoteServiceInstanceID,
		"operator ID":                 fixture.OperatorID,
		"fixture host ID":             fixture.FixtureHostID,
		"fixture network boundary ID": fixture.FixtureNetworkBoundaryID,
	} {
		if err := validateCanonicalID(value, label); err != nil {
			return errors.New("external fixture identity is invalid")
		}
	}
	if err := validateControllerOrigin(fixture.ControllerOrigin); err != nil ||
		validateCanonicalIPv4(fixture.ControllerPublicIP) != nil ||
		validateCanonicalIPv4(fixture.RemotePeerPublicIP) != nil ||
		fixture.RemotePeerUDPPortMin == 0 || fixture.RemotePeerUDPPortMax < fixture.RemotePeerUDPPortMin {
		return errors.New("external fixture endpoint authority is invalid")
	}
	parsedOrigin, _ := url.Parse(fixture.ControllerOrigin)
	if net.ParseIP(parsedOrigin.Hostname()) != nil && parsedOrigin.Hostname() != fixture.ControllerPublicIP {
		return errors.New("external fixture controller origin contradicts its public address")
	}
	if err := validateNetworkSemantics(fixture.ProfileID, fixture.NetworkSemantics); err != nil {
		return err
	}
	return nil
}

func ParseCanonicalExternalFixture(document []byte) (ExternalFixture, error) {
	var fixture ExternalFixture
	if err := decodeExactJSON(document, &fixture); err != nil {
		return ExternalFixture{}, errors.New("external fixture template is not the exact schema")
	}
	canonical, err := CanonicalExternalFixtureDocument(fixture)
	if err != nil || !bytes.Equal(document, canonical) {
		return ExternalFixture{}, errors.New("external fixture template is not canonical JSON")
	}
	return fixture, nil
}

func CanonicalExternalFixtureDocument(fixture ExternalFixture) ([]byte, error) {
	if err := ValidateExternalFixture(fixture); err != nil {
		return nil, err
	}
	return canonicalJSONLine(fixture)
}

func NetworkBindingSHA256(fixture ExternalFixture) (string, error) {
	document, err := CanonicalExternalFixtureDocument(fixture)
	if err != nil {
		return "", err
	}
	return sha256Hex(document), nil
}

func RemotePeerBindingSHA256FromFixture(fixture ExternalFixture) (string, error) {
	if err := ValidateExternalFixture(fixture); err != nil {
		return "", err
	}
	return remotePeerBindingSHA256(remotePeerBinding{
		SchemaVersion:            RemotePeerBindingSchemaVersion,
		DeploymentID:             fixture.DeploymentID,
		ProfileID:                fixture.ProfileID,
		AuthorityInstanceID:      fixture.AuthorityInstanceID,
		RemoteServiceInstanceID:  fixture.RemoteServiceInstanceID,
		FixtureHostID:            fixture.FixtureHostID,
		FixtureNetworkBoundaryID: fixture.FixtureNetworkBoundaryID,
		PublicIP:                 fixture.RemotePeerPublicIP,
		UDPPortMin:               fixture.RemotePeerUDPPortMin,
		UDPPortMax:               fixture.RemotePeerUDPPortMax,
	})
}

func CanonicalLiveAttestationDocument(attestation LiveExternalFixtureAttestation) ([]byte, error) {
	if _, _, err := validateLiveAttestation(attestation); err != nil {
		return nil, err
	}
	return canonicalJSONLine(attestation)
}

func SignLiveAttestation(attestation LiveExternalFixtureAttestation, privateKey ed25519.PrivateKey) (AuthorityProbeResponse, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return AuthorityProbeResponse{}, errors.New("external fixture attestation signer is invalid")
	}
	document, err := CanonicalLiveAttestationDocument(attestation)
	if err != nil {
		return AuthorityProbeResponse{}, err
	}
	digest := sha256Hex(document)
	signature := ed25519.Sign(privateKey, document)
	return AuthorityProbeResponse{
		ProtocolVersion:    ProtocolVersion,
		Attestation:        attestation,
		AttestationSHA256:  digest,
		SignatureAlgorithm: AttestationSignatureAlgorithm,
		Signature:          base64.RawURLEncoding.EncodeToString(signature),
	}, nil
}

func VerifyAuthorityProbeResponse(
	response AuthorityProbeResponse,
	publicKey ed25519.PublicKey,
	now time.Time,
	expectedRunID string,
	expectedNonce string,
) (VerifiedAttestation, error) {
	if response.ProtocolVersion != ProtocolVersion || response.SignatureAlgorithm != AttestationSignatureAlgorithm ||
		!sha256Pattern.MatchString(response.AttestationSHA256) || len(publicKey) != ed25519.PublicKeySize {
		return VerifiedAttestation{}, errors.New("external fixture attestation envelope is invalid")
	}
	issuedAt, expiresAt, err := validateLiveAttestation(response.Attestation)
	if err != nil || response.Attestation.RunID != expectedRunID || response.Attestation.Nonce != expectedNonce {
		return VerifiedAttestation{}, errors.New("external fixture attestation claims are invalid")
	}
	document, err := canonicalJSONLine(response.Attestation)
	if err != nil {
		return VerifiedAttestation{}, errors.New("external fixture attestation cannot be canonicalized")
	}
	digest := sha256Hex(document)
	if subtle.ConstantTimeCompare([]byte(digest), []byte(response.AttestationSHA256)) != 1 {
		return VerifiedAttestation{}, errors.New("external fixture attestation digest is invalid")
	}
	signature, err := base64.RawURLEncoding.DecodeString(response.Signature)
	if err != nil || len(response.Signature) != 86 || len(signature) != ed25519.SignatureSize ||
		base64.RawURLEncoding.EncodeToString(signature) != response.Signature ||
		!ed25519.Verify(publicKey, document, signature) {
		return VerifiedAttestation{}, errors.New("external fixture attestation signature is invalid")
	}
	now = now.UTC()
	if issuedAt.After(now.Add(maximumClockSkew)) || !now.Before(expiresAt) {
		return VerifiedAttestation{}, errors.New("external fixture attestation is outside its lease")
	}
	if response.Attestation.Fixture.NetworkSemantics.Kind == NetworkSemanticsCoturnRelay {
		credentialExpiry, _ := parseCanonicalTimestamp(
			response.Attestation.Fixture.NetworkSemantics.TURNCredentialExpiresAt,
		)
		if !credentialExpiry.Equal(expiresAt) {
			return VerifiedAttestation{}, errors.New("external fixture TURN credential lease differs from its attestation")
		}
	}
	networkBinding, err := NetworkBindingSHA256(response.Attestation.Fixture)
	if err != nil {
		return VerifiedAttestation{}, err
	}
	remotePeerBinding, err := RemotePeerBindingSHA256FromFixture(response.Attestation.Fixture)
	if err != nil {
		return VerifiedAttestation{}, err
	}
	return VerifiedAttestation{
		Attestation:             response.Attestation,
		AttestationSHA256:       digest,
		NetworkBindingSHA256:    networkBinding,
		RemotePeerBindingSHA256: remotePeerBinding,
		IssuedAt:                issuedAt,
		ExpiresAt:               expiresAt,
	}, nil
}

func validateLiveAttestation(attestation LiveExternalFixtureAttestation) (time.Time, time.Time, error) {
	if attestation.SchemaVersion != LiveAttestationSchemaVersion || validateCanonicalID(attestation.RunID, "run ID") != nil ||
		!validOpaqueID(attestation.Nonce) || !validOpaqueID(attestation.LeaseID) ||
		attestation.LeaseMillis < 1 || attestation.LeaseMillis > maximumLeaseLimit.Milliseconds() ||
		ValidateExternalFixture(attestation.Fixture) != nil {
		return time.Time{}, time.Time{}, errors.New("external fixture live attestation is invalid")
	}
	issuedAt, issuedErr := parseCanonicalTimestamp(attestation.IssuedAt)
	expiresAt, expiresErr := parseCanonicalTimestamp(attestation.ExpiresAt)
	if issuedErr != nil || expiresErr != nil || !expiresAt.After(issuedAt) ||
		expiresAt.Sub(issuedAt) != time.Duration(attestation.LeaseMillis)*time.Millisecond {
		return time.Time{}, time.Time{}, errors.New("external fixture attestation lease is invalid")
	}
	return issuedAt, expiresAt, nil
}

func validateNetworkSemantics(profileID string, semantics NetworkSemantics) error {
	if semantics.PolicyVersion == 0 || semantics.PolicyVersion > maximumSafeJSONInteger ||
		validateCanonicalID(semantics.PolicyID, "network policy ID") != nil {
		return errors.New("external fixture network policy is invalid")
	}
	expectStringsEmpty := func(values ...string) bool {
		for _, value := range values {
			if value != "" {
				return true
			}
		}
		return false
	}
	switch profileID {
	case "scheduled-public-stun":
		if semantics.Kind != NetworkSemanticsPublicSTUN || !validSTUNURI(semantics.STUNEndpoint) ||
			len(semantics.TURNURLs) != 0 || expectStringsEmpty(semantics.OutboundUDP, semantics.InboundUDP, semantics.RelayAccess,
			semantics.TURNServiceOwnerID, semantics.TURNUsername, semantics.TURNCredentialID,
			semantics.TURNCredentialExpiresAt) {
			return errors.New("public STUN fixture semantics are invalid")
		}
	case "scheduled-restricted-udp":
		if semantics.Kind != NetworkSemanticsRestrictedUDP || semantics.OutboundUDP != "denied" ||
			semantics.InboundUDP != "denied" || semantics.RelayAccess != "denied" ||
			len(semantics.TURNURLs) != 0 || expectStringsEmpty(semantics.STUNEndpoint, semantics.TURNServiceOwnerID, semantics.TURNUsername,
			semantics.TURNCredentialID, semantics.TURNCredentialExpiresAt) {
			return errors.New("restricted UDP fixture semantics are invalid")
		}
	case "scheduled-coturn":
		if semantics.Kind != NetworkSemanticsCoturnRelay ||
			validateCanonicalID(semantics.TURNServiceOwnerID, "TURN service owner ID") != nil ||
			!validTURNURLs(semantics.TURNURLs) || !validICECredential(semantics.TURNUsername) ||
			validateCanonicalID(semantics.TURNCredentialID, "TURN credential ID") != nil ||
			validateCredentialExpiry(semantics.TURNCredentialExpiresAt) != nil ||
			expectStringsEmpty(semantics.STUNEndpoint, semantics.OutboundUDP, semantics.InboundUDP,
				semantics.RelayAccess) {
			return errors.New("coturn fixture semantics are invalid")
		}
	default:
		return errors.New("external fixture profile is invalid")
	}
	return nil
}

func remotePeerBindingSHA256(binding remotePeerBinding) (string, error) {
	if binding.SchemaVersion != RemotePeerBindingSchemaVersion ||
		validateCanonicalID(binding.DeploymentID, "deployment ID") != nil || !validProfileID(binding.ProfileID) ||
		validateCanonicalID(binding.AuthorityInstanceID, "authority instance ID") != nil ||
		validateCanonicalID(binding.RemoteServiceInstanceID, "remote service instance ID") != nil ||
		validateCanonicalID(binding.FixtureHostID, "fixture host ID") != nil ||
		validateCanonicalID(binding.FixtureNetworkBoundaryID, "fixture network boundary ID") != nil ||
		validateCanonicalIPv4(binding.PublicIP) != nil || binding.UDPPortMin == 0 || binding.UDPPortMax < binding.UDPPortMin {
		return "", errors.New("remote Pion public endpoint declaration is invalid")
	}
	document, err := canonicalJSONLine(binding)
	if err != nil {
		return "", fmt.Errorf("encode remote Pion binding: %w", err)
	}
	return sha256Hex(document), nil
}

func validateControllerOrigin(origin string) error {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Hostname() == "" ||
		parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.String() != origin ||
		strings.ToLower(parsed.Hostname()) != parsed.Hostname() || origin != canonicalHTTPSOrigin(parsed) {
		return errors.New("controller origin is not a canonical HTTPS origin")
	}
	if port := parsed.Port(); port != "" {
		parsedPort, parseErr := strconv.Atoi(port)
		if parseErr != nil || parsedPort < 1 || parsedPort > 65_535 || strconv.Itoa(parsedPort) != port {
			return errors.New("controller origin port is not canonical")
		}
	}
	host := parsed.Hostname()
	for _, character := range host {
		if character > 127 {
			return errors.New("controller origin host must use canonical ASCII form")
		}
	}
	if strings.Trim(host, "0123456789.") == "" {
		address, parseErr := netip.ParseAddr(host)
		if parseErr != nil || !address.Is4() || address.String() != host {
			return errors.New("controller origin has an ambiguous numeric host")
		}
	}
	return nil
}

func validateCanonicalIPv4(value string) error {
	address, err := netip.ParseAddr(value)
	if err != nil || !address.Is4() || address.String() != value || !isAcceptedGlobalIPv4(address) {
		return errors.New("address is not a canonical global-unicast IPv4 address")
	}
	return nil
}

func validSTUNURI(value string) bool {
	return validICEURI(value, map[string]bool{"stun": true})
}

func validTURNURLs(values []string) bool {
	if len(values) == 0 || len(values) > 16 {
		return false
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
		if !validICEURI(value, map[string]bool{"turn": true, "turns": true}) {
			return false
		}
	}
	return true
}

func validICECredential(value string) bool {
	return value != "" && len([]byte(value)) <= 512 && utf8.ValidString(value) &&
		!strings.ContainsAny(value, "\x00\r\n")
}

func parseCanonicalTimestamp(value string) (time.Time, error) {
	parsed, err := time.Parse(canonicalTimestampLayout, value)
	if err != nil || parsed.Location() != time.UTC || parsed.UTC().Format(canonicalTimestampLayout) != value {
		return time.Time{}, errors.New("timestamp is not canonical UTC RFC3339Nano")
	}
	return parsed, nil
}

func validateCredentialExpiry(value string) error {
	_, err := parseCanonicalTimestamp(value)
	return err
}

func validICEURI(value string, allowed map[string]bool) bool {
	if value == "" || len([]byte(value)) > 512 {
		return false
	}
	for _, character := range value {
		if character <= 31 || character == 127 || character == ' ' || character == '\t' || character == '\r' || character == '\n' {
			return false
		}
	}
	parts := canonicalICEURIPattern.FindStringSubmatch(value)
	if parts == nil {
		return false
	}
	scheme := parts[1]
	if !allowed[scheme] || !validICEHost(parts[2]) {
		return false
	}
	if parts[3] != "" {
		port, err := strconv.Atoi(parts[3])
		if err != nil || port < 1 || port > 65_535 {
			return false
		}
	}
	return scheme != "turns" || parts[4] == "" || parts[4] == "tcp"
}

func validICEHost(value string) bool {
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		return net.ParseIP(value[1:len(value)-1]) != nil && strings.Contains(value, ":")
	}
	if address := net.ParseIP(value); address != nil {
		return strings.Contains(value, ".")
	}
	if strings.Trim(value, "0123456789.") == "" {
		return false
	}
	if len(value) > 253 || !strings.Contains(value, ".") {
		return false
	}
	for label := range strings.SplitSeq(value, ".") {
		if len(label) < 1 || len(label) > 63 || !canonicalDNSLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

func canonicalHTTPSOrigin(value *url.URL) string {
	host := value.Hostname()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if value.Port() != "" && value.Port() != "443" {
		host += ":" + value.Port()
	}
	return "https://" + host + "/"
}

func isAcceptedGlobalIPv4(address netip.Addr) bool {
	octets := address.As4()
	a, b, c, d := octets[0], octets[1], octets[2], octets[3]
	return !isReservedGlobalIPv4Range(a, b, c, d)
}

func isReservedGlobalIPv4Range(a, b, c, d byte) bool {
	return a == 0 || a == 10 || a == 127 || a >= 224 ||
		a == 100 && b >= 64 && b <= 127 || a == 169 && b == 254 ||
		a == 172 && b >= 16 && b <= 31 || a == 192 && b == 168 ||
		a == 192 && b == 0 && (c == 2 || c == 0 && d != 9 && d != 10) ||
		a == 192 && b == 88 && c == 99 || a == 198 && (b == 18 || b == 19) ||
		a == 198 && b == 51 && c == 100 || a == 203 && b == 0 && c == 113
}

func sha256Hex(document []byte) string {
	digest := sha256.Sum256(document)
	return hex.EncodeToString(digest[:])
}

func ParseCanonicalTimestamp(value string) (time.Time, error) {
	return parseCanonicalTimestamp(value)
}

func CanonicalJSONLine(value any) ([]byte, error) {
	return canonicalJSONLine(value)
}

func MarshalCanonicalObject(value any) ([]byte, error) {
	return marshalCanonicalObject(value)
}

func ValidICECredential(value string) bool {
	return validICECredential(value)
}

func ValidSTUNURI(value string) bool {
	return validSTUNURI(value)
}

func SHA256Hex(document []byte) string {
	return sha256Hex(document)
}

func validateCanonicalID(value, label string) error {
	if len(value) > 96 || !canonicalIDPattern.MatchString(value) {
		return fmt.Errorf("%s is not a canonical authority ID", label)
	}
	return nil
}

func validOpaqueID(value string) bool {
	return len(value) >= 16 && len(value) <= 128 && opaqueIDPattern.MatchString(value)
}

func validProfileID(value string) bool {
	switch value {
	case "scheduled-public-stun", "scheduled-restricted-udp", "scheduled-coturn":
		return true
	default:
		return false
	}
}
