// Package v2 defines the binary relay contract for sender-authenticated shares.
// Endpoint wiring is deliberately outside this package so v1 cannot silently
// reinterpret a v2 frame or select a runtime suite with an adapter flag.
package v2

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
)

const (
	WireVersion              = byte(2)
	Suite                    = byte(2)
	ShareIDBytes             = 12
	ShareInstanceBytes       = 16
	PKHashBytes              = 16
	DigestBytes              = sha256.Size
	RelayIdentityBytes       = sha256.Size
	ChallengeIDBytes         = 16
	ChallengeNonceBytes      = 32
	SenderPublicKeyBytes     = 32
	SignatureBytes           = 64
	ResumeTokenBytes         = 32
	StopIDBytes              = 16
	RelaySessionIDBytes      = 8
	MaxDescriptorBytes       = 16 << 10
	MaxOpaqueCiphertextBytes = 65_536
)

const (
	senderKeyDomain = "windshare/v2 sender-key\x00"
	shareIDDomain   = "windshare/v2 share-id\x00"
)

var (
	ErrMalformed         = errors.New("relay v2: malformed frame")
	ErrIdentity          = errors.New("relay v2: invalid identity")
	ErrMode              = errors.New("relay v2: unsupported registration mode")
	ErrPurpose           = errors.New("relay v2: invalid challenge purpose")
	ErrProof             = errors.New("relay v2: invalid sender proof")
	ErrChallengeExpired  = errors.New("relay v2: challenge expired")
	ErrChallengeConsumed = errors.New("relay v2: challenge already consumed")
	ErrChallengeBudget   = errors.New("relay v2: challenge budget exhausted")
)

type (
	ShareID        [ShareIDBytes]byte
	ShareInstance  [ShareInstanceBytes]byte
	PKHash         [PKHashBytes]byte
	Digest         [DigestBytes]byte
	RelayIdentity  [RelayIdentityBytes]byte
	ChallengeID    [ChallengeIDBytes]byte
	ChallengeNonce [ChallengeNonceBytes]byte
	ResumeToken    [ResumeTokenBytes]byte
	StopID         [StopIDBytes]byte
	RelaySessionID [RelaySessionIDBytes]byte
)

type RegistrationMode uint8

const (
	RegistrationFresh RegistrationMode = iota
	RegistrationResume
)

func (m RegistrationMode) valid() bool { return m == RegistrationFresh || m == RegistrationResume }

type ChallengePurpose uint8

const (
	ChallengeRegister ChallengePurpose = iota
	ChallengeResume
	ChallengeStop
)

func (p ChallengePurpose) valid() bool { return p <= ChallengeStop }

func purposeForMode(mode RegistrationMode) (ChallengePurpose, error) {
	switch mode {
	case RegistrationFresh:
		return ChallengeRegister, nil
	case RegistrationResume:
		return ChallengeResume, nil
	default:
		return 0, ErrMode
	}
}

func ShareIDFromBytes(raw []byte) (ShareID, error) {
	var value ShareID
	err := copyFixed(value[:], raw, "share ID")
	return value, err
}
func ShareInstanceFromBytes(raw []byte) (ShareInstance, error) {
	var value ShareInstance
	err := copyFixed(value[:], raw, "share instance")
	return value, err
}
func PKHashFromBytes(raw []byte) (PKHash, error) {
	var value PKHash
	err := copyFixed(value[:], raw, "pkHash")
	return value, err
}
func DigestFromBytes(raw []byte) (Digest, error) {
	var value Digest
	err := copyFixed(value[:], raw, "digest")
	return value, err
}
func RelayIdentityFromBytes(raw []byte) (RelayIdentity, error) {
	var value RelayIdentity
	err := copyFixed(value[:], raw, "relay identity")
	return value, err
}
func ChallengeIDFromBytes(raw []byte) (ChallengeID, error) {
	var value ChallengeID
	err := copyFixed(value[:], raw, "challenge ID")
	return value, err
}
func RelaySessionIDFromBytes(raw []byte) (RelaySessionID, error) {
	var value RelaySessionID
	err := copyFixed(value[:], raw, "relay session ID")
	return value, err
}

func copyFixed(destination, raw []byte, label string) error {
	if len(raw) != len(destination) {
		return fmt.Errorf("%w: %s has %d bytes", ErrIdentity, label, len(raw))
	}
	copy(destination, raw)
	return nil
}

func nonzero(raw []byte) bool {
	for _, item := range raw {
		if item != 0 {
			return true
		}
	}
	return false
}

func validRouteIdentity(shareID ShareID, pkHash PKHash) bool {
	expected := sha256.Sum256(append([]byte(shareIDDomain), pkHash[:]...))
	return subtle.ConstantTimeCompare(expected[:ShareIDBytes], shareID[:]) == 1
}

func cloneBytes(value []byte) []byte { return bytes.Clone(value) }

func senderKeyHash(publicKey []byte) (PKHash, error) {
	var result PKHash
	if len(publicKey) != SenderPublicKeyBytes {
		return result, ErrIdentity
	}
	digest := sha256.Sum256(append([]byte(senderKeyDomain), publicKey...))
	copy(result[:], digest[:PKHashBytes])
	return result, nil
}
