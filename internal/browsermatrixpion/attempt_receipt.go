package browsermatrixpion

import (
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/netip"
	"time"
)

type AttemptTerminalReceipt struct {
	ProtocolVersion        string                `json:"protocolVersion"`
	AttemptAuthority       AttemptAuthority      `json:"attemptAuthority"`
	TerminalAt             string                `json:"terminalAt"`
	AttemptLeaseIssuedAt   string                `json:"attemptLeaseIssuedAt"`
	AttemptLeaseExpiresAt  string                `json:"attemptLeaseExpiresAt"`
	AttemptLeaseMillis     int64                 `json:"attemptLeaseMillis"`
	State                  string                `json:"state"`
	SelectedPair           *SelectedPairEvidence `json:"selectedPair"`
	ChallengeBindingSHA256 string                `json:"challengeBindingSha256"`
	FailureCode            *string               `json:"failureCode"`
}

type SignedAttemptTerminalReceipt struct {
	ProtocolVersion    string                 `json:"protocolVersion"`
	Receipt            AttemptTerminalReceipt `json:"receipt"`
	ReceiptSHA256      string                 `json:"receiptSha256"`
	SignatureAlgorithm string                 `json:"signatureAlgorithm"`
	Signature          string                 `json:"signature"`
}

func CanonicalAttemptTerminalReceiptDocument(receipt AttemptTerminalReceipt) ([]byte, error) {
	if _, _, _, err := validateAttemptTerminalReceipt(receipt); err != nil {
		return nil, err
	}
	return canonicalJSONLine(receipt)
}

func SignAttemptTerminalReceipt(
	receipt AttemptTerminalReceipt,
	privateKey ed25519.PrivateKey,
) (SignedAttemptTerminalReceipt, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return SignedAttemptTerminalReceipt{}, errors.New("attempt terminal receipt signer is invalid")
	}
	receipt = cloneAttemptTerminalReceipt(receipt)
	document, err := CanonicalAttemptTerminalReceiptDocument(receipt)
	if err != nil {
		return SignedAttemptTerminalReceipt{}, err
	}
	signature := ed25519.Sign(privateKey, document)
	return SignedAttemptTerminalReceipt{
		ProtocolVersion:    ProtocolVersion,
		Receipt:            receipt,
		ReceiptSHA256:      sha256Hex(document),
		SignatureAlgorithm: AttestationSignatureAlgorithm,
		Signature:          base64.RawURLEncoding.EncodeToString(signature),
	}, nil
}

// VerifyAttemptTerminalReceipt cross-binds the terminal signature to both the
// independently verified fixture attestation and the exact create-attempt lease.
func VerifyAttemptTerminalReceipt(
	envelope SignedAttemptTerminalReceipt,
	publicKey ed25519.PublicKey,
	authority VerifiedAttestation,
	created CreateAttemptResponse,
) (AttemptTerminalReceipt, error) {
	receipt, terminalAt, attemptIssuedAt, attemptExpiresAt, err :=
		verifyAttemptTerminalReceiptSignature(envelope, publicKey)
	if err != nil {
		return AttemptTerminalReceipt{}, err
	}
	authorityIssuedAt, authorityExpiresAt := authority.IssuedAt.UTC(), authority.ExpiresAt.UTC()
	attemptAuthority := receipt.AttemptAuthority
	sampleAuthority := attemptAuthority.RequestAuthority.ControlAuthority.SampleAuthority
	fixtureBinding := attemptAuthority.RequestAuthority.FixtureBinding
	if authority.AttestationSHA256 != fixtureBinding.AttestationSHA256 ||
		authority.Attestation.RunID != sampleAuthority.RunID ||
		authority.Attestation.Fixture.ProfileID != sampleAuthority.ProfileID ||
		authority.Attestation.Fixture.AuthorityInstanceID != fixtureBinding.AuthorityInstanceID ||
		authority.Attestation.Fixture.RemoteServiceInstanceID != fixtureBinding.RemoteServiceInstanceID ||
		authority.NetworkBindingSHA256 != fixtureBinding.NetworkBindingSHA256 ||
		authority.RemotePeerBindingSHA256 != fixtureBinding.RemotePeerBindingSHA256 ||
		authority.Attestation.IssuedAt != authorityIssuedAt.Format(canonicalTimestampLayout) ||
		authority.Attestation.ExpiresAt != authorityExpiresAt.Format(canonicalTimestampLayout) ||
		terminalAt.Before(authorityIssuedAt) || !terminalAt.Before(authorityExpiresAt) ||
		attemptIssuedAt.Before(authorityIssuedAt) || attemptExpiresAt.After(authorityExpiresAt) ||
		receipt.AttemptLeaseMillis > authority.Attestation.LeaseMillis {
		return AttemptTerminalReceipt{}, errors.New("attempt terminal receipt crossed its signed fixture authority")
	}
	if created.ProtocolVersion != ProtocolVersion ||
		ValidateAttemptAuthority(attemptAuthority) != nil ||
		created.AttemptAuthority != attemptAuthority ||
		receipt.ChallengeBindingSHA256 != challengeBindingSHA256(attemptAuthority) ||
		created.LeaseIssuedAt != receipt.AttemptLeaseIssuedAt ||
		created.LeaseExpiresAt != receipt.AttemptLeaseExpiresAt ||
		created.LeaseMillis != receipt.AttemptLeaseMillis {
		return AttemptTerminalReceipt{}, errors.New("attempt terminal receipt crossed its create-attempt authority")
	}
	return receipt, nil
}

func verifyAttemptTerminalReceiptSignature(
	envelope SignedAttemptTerminalReceipt,
	publicKey ed25519.PublicKey,
) (AttemptTerminalReceipt, time.Time, time.Time, time.Time, error) {
	if envelope.ProtocolVersion != ProtocolVersion ||
		envelope.SignatureAlgorithm != AttestationSignatureAlgorithm ||
		!sha256Pattern.MatchString(envelope.ReceiptSHA256) ||
		len(publicKey) != ed25519.PublicKeySize {
		return AttemptTerminalReceipt{}, time.Time{}, time.Time{}, time.Time{},
			errors.New("attempt terminal receipt envelope is invalid")
	}
	receipt := cloneAttemptTerminalReceipt(envelope.Receipt)
	terminalAt, attemptIssuedAt, attemptExpiresAt, err := validateAttemptTerminalReceipt(receipt)
	if err != nil {
		return AttemptTerminalReceipt{}, time.Time{}, time.Time{}, time.Time{}, err
	}
	document, err := canonicalJSONLine(receipt)
	if err != nil {
		return AttemptTerminalReceipt{}, time.Time{}, time.Time{}, time.Time{},
			errors.New("attempt terminal receipt cannot be canonicalized")
	}
	digest := sha256Hex(document)
	if subtle.ConstantTimeCompare([]byte(digest), []byte(envelope.ReceiptSHA256)) != 1 {
		return AttemptTerminalReceipt{}, time.Time{}, time.Time{}, time.Time{},
			errors.New("attempt terminal receipt digest is invalid")
	}
	signature, err := base64.RawURLEncoding.DecodeString(envelope.Signature)
	if err != nil || len(envelope.Signature) != 86 || len(signature) != ed25519.SignatureSize ||
		base64.RawURLEncoding.EncodeToString(signature) != envelope.Signature ||
		!ed25519.Verify(publicKey, document, signature) {
		return AttemptTerminalReceipt{}, time.Time{}, time.Time{}, time.Time{},
			errors.New("attempt terminal receipt signature is invalid")
	}
	return receipt, terminalAt, attemptIssuedAt, attemptExpiresAt, nil
}

func validateAttemptTerminalReceipt(
	receipt AttemptTerminalReceipt,
) (time.Time, time.Time, time.Time, error) {
	if receipt.ProtocolVersion != ProtocolVersion ||
		ValidateAttemptAuthority(receipt.AttemptAuthority) != nil ||
		!sha256Pattern.MatchString(receipt.ChallengeBindingSHA256) ||
		receipt.AttemptLeaseMillis < 1 ||
		receipt.AttemptLeaseMillis > maximumLeaseLimit.Milliseconds() {
		return time.Time{}, time.Time{}, time.Time{},
			errors.New("attempt terminal receipt claims are invalid")
	}
	terminalAt, terminalErr := parseCanonicalTimestamp(receipt.TerminalAt)
	attemptIssuedAt, issuedErr := parseCanonicalTimestamp(receipt.AttemptLeaseIssuedAt)
	attemptExpiresAt, expiresErr := parseCanonicalTimestamp(receipt.AttemptLeaseExpiresAt)
	lease := time.Duration(receipt.AttemptLeaseMillis) * time.Millisecond
	if terminalErr != nil || issuedErr != nil || expiresErr != nil ||
		!attemptExpiresAt.After(attemptIssuedAt) ||
		attemptExpiresAt.Sub(attemptIssuedAt) != lease ||
		terminalAt.Before(attemptIssuedAt) || !terminalAt.Before(attemptExpiresAt) {
		return time.Time{}, time.Time{}, time.Time{},
			errors.New("attempt terminal receipt lifetime is invalid")
	}
	switch receipt.State {
	case attemptStateEstablished:
		if receipt.SelectedPair == nil || receipt.FailureCode != nil ||
			validateSelectedPairEvidence(*receipt.SelectedPair) != nil {
			return time.Time{}, time.Time{}, time.Time{},
				errors.New("established attempt terminal receipt is invalid")
		}
	case attemptStateFailed:
		if receipt.SelectedPair != nil || receipt.FailureCode == nil ||
			validateCanonicalID(*receipt.FailureCode, "failure code") != nil {
			return time.Time{}, time.Time{}, time.Time{},
				errors.New("failed attempt terminal receipt is invalid")
		}
	default:
		return time.Time{}, time.Time{}, time.Time{},
			errors.New("attempt terminal receipt state is invalid")
	}
	return terminalAt, attemptIssuedAt, attemptExpiresAt, nil
}

func validateSelectedPairEvidence(pair SelectedPairEvidence) error {
	if validateCandidateEvidence(pair.Local) != nil || validateCandidateEvidence(pair.Remote) != nil {
		return errors.New("selected pair evidence is invalid")
	}
	return nil
}

func validateCandidateEvidence(candidate CandidateEvidence) error {
	switch candidate.CandidateType {
	case "host", "srflx", "prflx", "relay":
	default:
		return errors.New("candidate type is invalid")
	}
	switch candidate.Protocol {
	case "udp", "tcp":
	default:
		return errors.New("candidate protocol is invalid")
	}
	address, err := netip.ParseAddr(candidate.Address)
	if err != nil || candidate.Port == 0 {
		return errors.New("candidate endpoint is invalid")
	}
	family := "ipv6"
	if address.Unmap().Is4() {
		family = "ipv4"
	}
	if candidate.AddressFamily != family {
		return errors.New("candidate address family is invalid")
	}
	return nil
}

func cloneAttemptTerminalReceipt(receipt AttemptTerminalReceipt) AttemptTerminalReceipt {
	if receipt.SelectedPair != nil {
		pair := *receipt.SelectedPair
		receipt.SelectedPair = &pair
	}
	if receipt.FailureCode != nil {
		failureCode := *receipt.FailureCode
		receipt.FailureCode = &failureCode
	}
	return receipt
}

func cloneSignedAttemptTerminalReceipt(
	receipt SignedAttemptTerminalReceipt,
) SignedAttemptTerminalReceipt {
	receipt.Receipt = cloneAttemptTerminalReceipt(receipt.Receipt)
	return receipt
}
