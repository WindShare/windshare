package browsernetworktopology

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"regexp"
	"time"

	"github.com/windshare/windshare/internal/browsermatrixfixture"
	"github.com/windshare/windshare/internal/browsermatrixpion"
)

var (
	ErrInvalidAttemptEvidence = errors.New("invalid browser network matrix attempt evidence")
	attemptIDPattern          = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
)

type PionAuthority string

const (
	PionAuthorityExternalRemote PionAuthority = "external-remote"
)

type AddressFamily string

const (
	AddressFamilyIPv4 AddressFamily = "ipv4"
	AddressFamilyIPv6 AddressFamily = "ipv6"
)

type PionSelectedPair struct {
	SelectedPair        SelectedPairObservation `json:"selectedPair"`
	LocalCandidateType  *CandidateType          `json:"localCandidateType"`
	LocalAddressFamily  *AddressFamily          `json:"localAddressFamily"`
	RemoteCandidateType *CandidateType          `json:"remoteCandidateType"`
	RemoteAddressFamily *AddressFamily          `json:"remoteAddressFamily"`
	Protocol            *TransportProtocol      `json:"protocol"`
	LocalAddress        *string                 `json:"localAddress"`
	LocalPort           *uint16                 `json:"localPort"`
	RemoteAddress       *string                 `json:"remoteAddress"`
	RemotePort          *uint16                 `json:"remotePort"`
}

type ChallengeProof struct {
	BindingSHA256         string `json:"bindingSha256"`
	Challenge             string `json:"challenge"`
	PionChallengeObserved bool   `json:"pionChallengeObserved"`
	BrowserEchoObserved   bool   `json:"browserEchoObserved"`
}

type ExternalFixtureAttemptBinding struct {
	RunID                    string                                      `json:"runId"`
	AuthorityInstanceID      string                                      `json:"authorityInstanceId"`
	RemoteServiceInstanceID  string                                      `json:"remoteServiceInstanceId"`
	AttestationSHA256        string                                      `json:"attestationSha256"`
	AttestationPublicKeySPKI string                                      `json:"attestationPublicKeySpki"`
	SignedAttestation        browsermatrixfixture.AuthorityProbeResponse `json:"signedAttestation"`
	NetworkBindingSHA256     string                                      `json:"networkBindingSha256"`
	RemotePeerBindingSHA256  string                                      `json:"remotePeerBindingSha256"`
	ControllerPublicIP       string                                      `json:"controllerPublicIp"`
	AttestationExpiresAt     string                                      `json:"attestationExpiresAt"`
	RemotePeerPublicIP       string                                      `json:"remotePeerPublicIp"`
	RemotePeerUDPPortMin     uint16                                      `json:"remotePeerUdpPortMin"`
	RemotePeerUDPPortMax     uint16                                      `json:"remotePeerUdpPortMax"`
}

// AttemptEvidence joins independently reported endpoint facts so browser-local
// getStats output cannot authenticate a topology by itself.
type AttemptEvidence struct {
	AttemptAuthority    browsermatrixpion.AttemptAuthority             `json:"attemptAuthority"`
	PionAuthority       PionAuthority                                  `json:"pionAuthority"`
	ExternalFixture     ExternalFixtureAttemptBinding                  `json:"externalFixture"`
	BrowserSelectedPair CandidatePath                                  `json:"browserSelectedPair"`
	PionSelectedPair    PionSelectedPair                               `json:"pionSelectedPair"`
	Challenge           *ChallengeProof                                `json:"challenge"`
	TerminalReceipt     browsermatrixpion.SignedAttemptTerminalReceipt `json:"terminalReceipt"`
}

func (evidence AttemptEvidence) Validate(profileID string, trusted ExternalFixtureTrustProof) error {
	if browsermatrixpion.ValidateAttemptAuthority(evidence.AttemptAuthority) != nil ||
		evidence.AttemptAuthority.RequestAuthority.ControlAuthority.SampleAuthority.ProfileID != profileID {
		return fmt.Errorf("%w: attempt authority is outside its sample profile", ErrInvalidAttemptEvidence)
	}
	expectedAuthority, known := pionAuthorityForProfile(profileID)
	if !known || evidence.PionAuthority != expectedAuthority {
		return fmt.Errorf("%w: Pion authority differs from profile %q", ErrInvalidAttemptEvidence, profileID)
	}
	if err := evidence.BrowserSelectedPair.Validate(); err != nil {
		return errors.Join(ErrInvalidAttemptEvidence, err)
	}
	if err := evidence.PionSelectedPair.Validate(); err != nil {
		return errors.Join(ErrInvalidAttemptEvidence, err)
	}
	verified, publicKey, err := evidence.ExternalFixture.authenticate(profileID, trusted)
	if err != nil {
		return errors.Join(ErrInvalidAttemptEvidence, err)
	}
	receipt, err := evidence.authenticateTerminalReceipt(profileID, publicKey, verified)
	if err != nil {
		return errors.Join(ErrInvalidAttemptEvidence, err)
	}
	if evidence.BrowserSelectedPair.SelectedPair != evidence.PionSelectedPair.SelectedPair {
		return fmt.Errorf("%w: browser and Pion selected-pair presence differ", ErrInvalidAttemptEvidence)
	}

	if profileID == string(ProfileScheduledRestrictedUDP) {
		if evidence.BrowserSelectedPair.SelectedPair != SelectedPairAbsent || evidence.Challenge != nil ||
			receipt.State != "failed" || receipt.FailureCode == nil || *receipt.FailureCode != "ice-failed" {
			return fmt.Errorf("%w: restricted UDP evidence must carry two absent pairs and no challenge", ErrInvalidAttemptEvidence)
		}
		return nil
	}

	if evidence.BrowserSelectedPair.SelectedPair != SelectedPairPresent ||
		evidence.PionSelectedPair.SelectedPair != SelectedPairPresent || evidence.Challenge == nil ||
		receipt.State != "established" || receipt.FailureCode != nil {
		return fmt.Errorf("%w: established connectivity needs two present pairs and a challenge", ErrInvalidAttemptEvidence)
	}
	if !evidence.Challenge.valid() ||
		evidence.Challenge.Challenge != receipt.AttemptAuthority.Challenge ||
		evidence.Challenge.BindingSHA256 != receipt.ChallengeBindingSHA256 {
		return fmt.Errorf("%w: application challenge proof is invalid", ErrInvalidAttemptEvidence)
	}
	if *evidence.BrowserSelectedPair.Protocol != *evidence.PionSelectedPair.Protocol ||
		*evidence.BrowserSelectedPair.RemoteAddress != *evidence.PionSelectedPair.LocalAddress ||
		*evidence.BrowserSelectedPair.RemotePort != *evidence.PionSelectedPair.LocalPort ||
		!optionalBrowserEndpointMatchesPion(
			evidence.BrowserSelectedPair.LocalAddress,
			evidence.BrowserSelectedPair.LocalPort,
			evidence.PionSelectedPair.RemoteAddress,
			evidence.PionSelectedPair.RemotePort,
		) {
		return fmt.Errorf("%w: browser and Pion pairs do not describe the same attempt", ErrInvalidAttemptEvidence)
	}
	if *evidence.PionSelectedPair.LocalAddress != evidence.ExternalFixture.RemotePeerPublicIP ||
		*evidence.PionSelectedPair.LocalPort < evidence.ExternalFixture.RemotePeerUDPPortMin ||
		*evidence.PionSelectedPair.LocalPort > evidence.ExternalFixture.RemotePeerUDPPortMax {
		return fmt.Errorf("%w: Pion selected pair differs from its signed remote endpoint", ErrInvalidAttemptEvidence)
	}
	if !validPionProfilePair(profileID, evidence.PionSelectedPair) {
		return fmt.Errorf("%w: Pion selected pair contradicts its external fixture profile", ErrInvalidAttemptEvidence)
	}
	return nil
}

func (pair PionSelectedPair) Validate() error {
	switch pair.SelectedPair {
	case SelectedPairAbsent:
		if pair.LocalCandidateType != nil || pair.LocalAddressFamily != nil ||
			pair.RemoteCandidateType != nil || pair.RemoteAddressFamily != nil || pair.Protocol != nil ||
			pair.LocalAddress != nil || pair.LocalPort != nil || pair.RemoteAddress != nil || pair.RemotePort != nil {
			return fmt.Errorf("%w: absent Pion pair carries candidate fields", ErrInvalidAttemptEvidence)
		}
	case SelectedPairPresent:
		if pair.LocalCandidateType == nil || pair.LocalAddressFamily == nil ||
			pair.RemoteCandidateType == nil || pair.RemoteAddressFamily == nil || pair.Protocol == nil ||
			pair.LocalAddress == nil || pair.LocalPort == nil || pair.RemoteAddress == nil || pair.RemotePort == nil ||
			*pair.LocalPort == 0 || *pair.RemotePort == 0 ||
			!validCandidateType(*pair.LocalCandidateType) || !validAddressFamily(*pair.LocalAddressFamily) ||
			!validCandidateType(*pair.RemoteCandidateType) || !validAddressFamily(*pair.RemoteAddressFamily) ||
			!validProtocol(*pair.Protocol) ||
			!validPionEndpoint(*pair.LocalAddress, *pair.LocalAddressFamily) ||
			!validPionEndpoint(*pair.RemoteAddress, *pair.RemoteAddressFamily) {
			return fmt.Errorf("%w: present Pion pair lacks a valid candidate, family, or protocol", ErrInvalidAttemptEvidence)
		}
	default:
		return fmt.Errorf("%w: Pion selected-pair observation is unknown", ErrInvalidAttemptEvidence)
	}
	return nil
}

func (proof ChallengeProof) valid() bool {
	return isCanonicalSHA256(proof.BindingSHA256) && attemptIDPattern.MatchString(proof.Challenge) &&
		proof.PionChallengeObserved && proof.BrowserEchoObserved
}

func (binding ExternalFixtureAttemptBinding) authenticate(
	profileID string,
	trusted ExternalFixtureTrustProof,
) (browsermatrixfixture.VerifiedAttestation, []byte, error) {
	if !validIdentifier(binding.RunID) || !validIdentifier(binding.AuthorityInstanceID) ||
		!validIdentifier(binding.RemoteServiceInstanceID) || !isCanonicalSHA256(binding.AttestationSHA256) ||
		!isCanonicalSHA256(binding.NetworkBindingSHA256) || !isCanonicalSHA256(binding.RemotePeerBindingSHA256) ||
		binding.RemotePeerUDPPortMin == 0 || binding.RemotePeerUDPPortMax < binding.RemotePeerUDPPortMin ||
		!canonicalIPv4(binding.ControllerPublicIP) || !canonicalIPv4(binding.RemotePeerPublicIP) ||
		!canonicalTimestamp(binding.AttestationExpiresAt) {
		return browsermatrixfixture.VerifiedAttestation{}, nil,
			fmt.Errorf("external fixture attempt binding is invalid")
	}
	if binding.AttestationPublicKeySPKI != trusted.AttestationPublicKeySPKI {
		return browsermatrixfixture.VerifiedAttestation{}, nil,
			fmt.Errorf("sample attempt crossed its authenticated runtime fixture trust anchor")
	}
	der, err := base64.RawURLEncoding.DecodeString(binding.AttestationPublicKeySPKI)
	if err != nil || base64.RawURLEncoding.EncodeToString(der) != binding.AttestationPublicKeySPKI {
		return browsermatrixfixture.VerifiedAttestation{}, nil,
			fmt.Errorf("sample attempt attestation public key is invalid")
	}
	publicKey, err := parseAttestationPublicKeySPKI(
		binding.AttestationPublicKeySPKI,
		trusted.AttestationPublicKeySHA256,
	)
	if err != nil {
		return browsermatrixfixture.VerifiedAttestation{}, nil,
			fmt.Errorf("sample attempt attestation public key is invalid")
	}
	issuedAt, err := time.Parse(
		browsermatrixfixture.CanonicalTimestampLayout,
		binding.SignedAttestation.Attestation.IssuedAt,
	)
	if err != nil {
		return browsermatrixfixture.VerifiedAttestation{}, nil,
			fmt.Errorf("sample attempt signed fixture lease is invalid")
	}
	verified, err := browsermatrixfixture.VerifyAuthorityProbeResponse(
		binding.SignedAttestation,
		publicKey,
		issuedAt,
		binding.RunID,
		binding.SignedAttestation.Attestation.Nonce,
	)
	if err != nil {
		return browsermatrixfixture.VerifiedAttestation{}, nil,
			fmt.Errorf("sample attempt signed fixture is invalid")
	}
	fixture := verified.Attestation.Fixture
	if fixture.ProfileID != profileID || verified.Attestation.RunID != binding.RunID ||
		verified.AttestationSHA256 != binding.AttestationSHA256 ||
		fixture.AuthorityInstanceID != binding.AuthorityInstanceID ||
		fixture.RemoteServiceInstanceID != binding.RemoteServiceInstanceID ||
		verified.NetworkBindingSHA256 != binding.NetworkBindingSHA256 ||
		verified.RemotePeerBindingSHA256 != binding.RemotePeerBindingSHA256 ||
		fixture.ControllerPublicIP != binding.ControllerPublicIP ||
		verified.Attestation.ExpiresAt != binding.AttestationExpiresAt ||
		fixture.RemotePeerPublicIP != binding.RemotePeerPublicIP ||
		fixture.RemotePeerUDPPortMin != binding.RemotePeerUDPPortMin ||
		fixture.RemotePeerUDPPortMax != binding.RemotePeerUDPPortMax {
		return browsermatrixfixture.VerifiedAttestation{}, nil,
			fmt.Errorf("sample attempt differs from its signed external fixture declaration")
	}
	return verified, publicKey, nil
}

func (evidence AttemptEvidence) authenticateTerminalReceipt(
	profileID string,
	publicKey []byte,
	verified browsermatrixfixture.VerifiedAttestation,
) (browsermatrixpion.AttemptTerminalReceipt, error) {
	claimed := evidence.TerminalReceipt.Receipt
	created := browsermatrixpion.CreateAttemptResponse{
		ProtocolVersion:  browsermatrixpion.ProtocolVersion,
		AttemptAuthority: evidence.AttemptAuthority,
		LeaseIssuedAt:    claimed.AttemptLeaseIssuedAt,
		LeaseExpiresAt:   claimed.AttemptLeaseExpiresAt,
		LeaseMillis:      claimed.AttemptLeaseMillis,
	}
	sampleAuthority := claimed.AttemptAuthority.RequestAuthority.ControlAuthority.SampleAuthority
	fixtureBinding := claimed.AttemptAuthority.RequestAuthority.FixtureBinding
	receipt, err := browsermatrixpion.VerifyAttemptTerminalReceipt(
		evidence.TerminalReceipt,
		publicKey,
		verified,
		created,
	)
	if err != nil {
		return browsermatrixpion.AttemptTerminalReceipt{}, fmt.Errorf("signed terminal receipt is invalid")
	}
	pair, err := pionPairFromTerminalReceipt(receipt.SelectedPair)
	if err != nil || !reflect.DeepEqual(pair, evidence.PionSelectedPair) ||
		receipt.AttemptAuthority != evidence.AttemptAuthority ||
		sampleAuthority.RunID != evidence.ExternalFixture.RunID || sampleAuthority.ProfileID != profileID ||
		fixtureBinding.AttestationSHA256 != evidence.ExternalFixture.AttestationSHA256 ||
		fixtureBinding.AuthorityInstanceID != evidence.ExternalFixture.AuthorityInstanceID ||
		fixtureBinding.RemoteServiceInstanceID != evidence.ExternalFixture.RemoteServiceInstanceID ||
		fixtureBinding.NetworkBindingSHA256 != evidence.ExternalFixture.NetworkBindingSHA256 ||
		fixtureBinding.RemotePeerBindingSHA256 != evidence.ExternalFixture.RemotePeerBindingSHA256 {
		return browsermatrixpion.AttemptTerminalReceipt{},
			fmt.Errorf("signed terminal receipt differs from the exact sample attempt")
	}
	return receipt, nil
}

func pionPairFromTerminalReceipt(pair *browsermatrixpion.SelectedPairEvidence) (PionSelectedPair, error) {
	if pair == nil {
		return PionSelectedPair{SelectedPair: SelectedPairAbsent}, nil
	}
	if pair.Local.Protocol != pair.Remote.Protocol {
		return PionSelectedPair{}, fmt.Errorf("signed terminal Pion candidates use different protocols")
	}
	localType, remoteType := CandidateType(pair.Local.CandidateType), CandidateType(pair.Remote.CandidateType)
	localFamily, remoteFamily := AddressFamily(pair.Local.AddressFamily), AddressFamily(pair.Remote.AddressFamily)
	protocol := TransportProtocol(pair.Local.Protocol)
	localAddress, remoteAddress := pair.Local.Address, pair.Remote.Address
	localPort, remotePort := pair.Local.Port, pair.Remote.Port
	result := PionSelectedPair{
		SelectedPair:       SelectedPairPresent,
		LocalCandidateType: &localType, LocalAddressFamily: &localFamily,
		RemoteCandidateType: &remoteType, RemoteAddressFamily: &remoteFamily,
		Protocol: &protocol, LocalAddress: &localAddress, LocalPort: &localPort,
		RemoteAddress: &remoteAddress, RemotePort: &remotePort,
	}
	if err := result.Validate(); err != nil {
		return PionSelectedPair{}, err
	}
	return result, nil
}

func optionalBrowserEndpointMatchesPion(
	address *string,
	port *uint16,
	pionAddress *string,
	pionPort *uint16,
) bool {
	if pionAddress == nil || pionPort == nil {
		return false
	}
	if address != nil {
		if _, err := netip.ParseAddr(*address); err == nil && *address != *pionAddress {
			return false
		}
	}
	return port == nil || *port == *pionPort
}

func validPionProfilePair(profileID string, pair PionSelectedPair) bool {
	if pair.LocalCandidateType == nil || pair.RemoteCandidateType == nil || pair.Protocol == nil {
		return false
	}
	local, remote, protocol := *pair.LocalCandidateType, *pair.RemoteCandidateType, *pair.Protocol
	switch profileID {
	case string(ProfileScheduledPublicSTUN), string(ProfileManualRealNAT):
		return local != CandidateRelay && remote != CandidateRelay && protocol == ProtocolUDP
	case string(ProfileScheduledCoturn):
		return local != CandidateRelay && remote == CandidateRelay &&
			(protocol == ProtocolUDP || protocol == ProtocolTCP)
	default:
		return false
	}
}

func validPionEndpoint(address string, family AddressFamily) bool {
	parsed, err := netip.ParseAddr(address)
	if err != nil {
		return false
	}
	return family == AddressFamilyIPv4 && parsed.Is4() || family == AddressFamilyIPv6 && parsed.Is6()
}

func canonicalIPv4(value string) bool {
	parsed, err := netip.ParseAddr(value)
	return err == nil && parsed.Is4() && parsed.String() == value
}

func canonicalTimestamp(value string) bool {
	parsed, err := time.Parse(browsermatrixfixture.CanonicalTimestampLayout, value)
	return err == nil && parsed.UTC().Format(browsermatrixfixture.CanonicalTimestampLayout) == value
}

func pionAuthorityForProfile(profileID string) (PionAuthority, bool) {
	_, known := findProfileSpec(profileID)
	return PionAuthorityExternalRemote, known
}

func validAddressFamily(family AddressFamily) bool {
	return family == AddressFamilyIPv4 || family == AddressFamilyIPv6
}
