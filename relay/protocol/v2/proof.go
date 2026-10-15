package v2

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"time"

	"github.com/windshare/windshare/core/senderauth"
)

const (
	registerDomain = "windshare/v2 relay-register\x00"
	resumeDomain   = "windshare/v2 relay-resume\x00"
	stopDomain     = "windshare/v2 relay-stop\x00"
)

func RegistrationPreimage(init RegisterInit, challenge Challenge, relayIdentity RelayIdentity) ([]byte, error) {
	if err := init.Validate(); err != nil {
		return nil, err
	}
	purpose, err := purposeForMode(init.Mode)
	if err != nil || challenge.Purpose != purpose {
		return nil, ErrPurpose
	}
	if err := challenge.Validate(); err != nil || !nonzero(relayIdentity[:]) {
		return nil, ErrIdentity
	}
	domain := registerDomain
	if init.Mode == RegistrationResume {
		domain = resumeDomain
	}
	result := make([]byte, 0, len(domain)+ShareIDBytes+ShareInstanceBytes+PKHashBytes+DigestBytes*2+RelayIdentityBytes+ChallengeIDBytes+ChallengeNonceBytes+8)
	result = append(result, domain...)
	result = append(result, init.ShareID[:]...)
	result = append(result, init.ShareInstance[:]...)
	result = append(result, init.PKHash[:]...)
	result = append(result, init.DescriptorDigest[:]...)
	result = append(result, init.ResumeTokenHash[:]...)
	result = append(result, relayIdentity[:]...)
	result = append(result, challenge.ID[:]...)
	result = append(result, challenge.Nonce[:]...)
	return binary.BigEndian.AppendUint64(result, challenge.ExpiresAtUnixSeconds), nil
}

func NewRegisterProof(init RegisterInit, challenge Challenge, relayIdentity RelayIdentity, senderPrivateKey ed25519.PrivateKey) (RegisterProof, error) {
	if len(senderPrivateKey) != ed25519.PrivateKeySize {
		return RegisterProof{}, ErrProof
	}
	preimage, err := RegistrationPreimage(init, challenge, relayIdentity)
	if err != nil {
		return RegisterProof{}, err
	}
	publicKey := senderPrivateKey.Public().(ed25519.PublicKey)
	hash, err := senderKeyHash(publicKey)
	if err != nil || subtle.ConstantTimeCompare(hash[:], init.PKHash[:]) != 1 {
		return RegisterProof{}, ErrProof
	}
	var proof RegisterProof
	proof.Mode = init.Mode
	copy(proof.SenderPublicKey[:], publicKey)
	copy(proof.Signature[:], ed25519.Sign(senderPrivateKey, preimage))
	return proof, nil
}

type SenderAuthority struct {
	initDigest [sha256.Size]byte
	valid      bool
}

func authenticateRegisterProof(init RegisterInit, challenge Challenge, relayIdentity RelayIdentity, proof RegisterProof, now time.Time) (SenderAuthority, error) {
	if proof.Mode != init.Mode || !challengeAlive(challenge, now) {
		if !challengeAlive(challenge, now) {
			return SenderAuthority{}, ErrChallengeExpired
		}
		return SenderAuthority{}, ErrProof
	}
	hash, err := senderKeyHash(publicKey(proof.SenderPublicKey))
	if err != nil || subtle.ConstantTimeCompare(hash[:], init.PKHash[:]) != 1 {
		return SenderAuthority{}, ErrProof
	}
	preimage, err := RegistrationPreimage(init, challenge, relayIdentity)
	if err != nil {
		return SenderAuthority{}, err
	}
	if !senderauth.Verify(publicKey(proof.SenderPublicKey), preimage, proof.Signature[:]) {
		return SenderAuthority{}, ErrProof
	}
	encoded, err := init.MarshalBinary()
	if err != nil {
		return SenderAuthority{}, err
	}
	return SenderAuthority{initDigest: sha256.Sum256(encoded), valid: true}, nil
}

func StopPreimage(init StopInit, challenge Challenge) ([]byte, error) {
	if err := init.Validate(); err != nil {
		return nil, err
	}
	if challenge.Purpose != ChallengeStop {
		return nil, ErrPurpose
	}
	if err := challenge.Validate(); err != nil {
		return nil, err
	}
	result := make([]byte, 0, len(stopDomain)+ShareIDBytes+ShareInstanceBytes+PKHashBytes+RelayIdentityBytes+StopIDBytes+ChallengeIDBytes+ChallengeNonceBytes+8)
	result = append(result, stopDomain...)
	result = append(result, init.ShareID[:]...)
	result = append(result, init.ShareInstance[:]...)
	result = append(result, init.PKHash[:]...)
	result = append(result, init.RelayIdentity[:]...)
	result = append(result, init.StopID[:]...)
	result = append(result, challenge.ID[:]...)
	result = append(result, challenge.Nonce[:]...)
	return binary.BigEndian.AppendUint64(result, challenge.ExpiresAtUnixSeconds), nil
}

func NewStopProof(init StopInit, challenge Challenge, senderPrivateKey ed25519.PrivateKey) (StopProof, error) {
	if len(senderPrivateKey) != ed25519.PrivateKeySize {
		return StopProof{}, ErrProof
	}
	preimage, err := StopPreimage(init, challenge)
	if err != nil {
		return StopProof{}, err
	}
	publicKey := senderPrivateKey.Public().(ed25519.PublicKey)
	hash, err := senderKeyHash(publicKey)
	if err != nil || subtle.ConstantTimeCompare(hash[:], init.PKHash[:]) != 1 {
		return StopProof{}, ErrProof
	}
	var proof StopProof
	copy(proof.SenderPublicKey[:], publicKey)
	copy(proof.Signature[:], ed25519.Sign(senderPrivateKey, preimage))
	return proof, nil
}

type StopAuthority struct {
	initDigest [sha256.Size]byte
	valid      bool
}

func authenticateStopProof(init StopInit, challenge Challenge, proof StopProof, now time.Time) (StopAuthority, error) {
	if !challengeAlive(challenge, now) {
		return StopAuthority{}, ErrChallengeExpired
	}
	hash, err := senderKeyHash(publicKey(proof.SenderPublicKey))
	if err != nil || subtle.ConstantTimeCompare(hash[:], init.PKHash[:]) != 1 {
		return StopAuthority{}, ErrProof
	}
	preimage, err := StopPreimage(init, challenge)
	if err != nil {
		return StopAuthority{}, err
	}
	if !senderauth.Verify(publicKey(proof.SenderPublicKey), preimage, proof.Signature[:]) {
		return StopAuthority{}, ErrProof
	}
	encoded, err := init.MarshalBinary()
	if err != nil {
		return StopAuthority{}, err
	}
	return StopAuthority{initDigest: sha256.Sum256(encoded), valid: true}, nil
}

func (a SenderAuthority) Authorizes(init RegisterInit) bool {
	if !a.valid {
		return false
	}
	encoded, err := init.MarshalBinary()
	if err != nil {
		return false
	}
	digest := sha256.Sum256(encoded)
	return subtle.ConstantTimeCompare(digest[:], a.initDigest[:]) == 1
}

func (a StopAuthority) Authorizes(init StopInit) bool {
	if !a.valid {
		return false
	}
	encoded, err := init.MarshalBinary()
	if err != nil {
		return false
	}
	digest := sha256.Sum256(encoded)
	return subtle.ConstantTimeCompare(digest[:], a.initDigest[:]) == 1
}

// AuthorizedDescriptorUpload proves publication authority for bounded opaque bytes,
// not the validity of the end-to-end object. Receivers own object authentication.
type AuthorizedDescriptorUpload struct {
	initDigest [sha256.Size]byte
	object     []byte
	valid      bool
}

// AuthorizeDescriptorUpload binds WS2U bytes to the authenticated registration
// digest so object formats can evolve without changing relay admission.
func AuthorizeDescriptorUpload(init RegisterInit, authority SenderAuthority, upload DescriptorUpload) (AuthorizedDescriptorUpload, error) {
	if !authority.Authorizes(init) {
		return AuthorizedDescriptorUpload{}, ErrProof
	}
	if len(upload.Object) == 0 || len(upload.Object) > MaxDescriptorBytes {
		return AuthorizedDescriptorUpload{}, ErrMalformed
	}
	object := cloneBytes(upload.Object)
	digest := sha256.Sum256(object)
	if subtle.ConstantTimeCompare(digest[:], init.DescriptorDigest[:]) != 1 {
		return AuthorizedDescriptorUpload{}, ErrProof
	}
	return AuthorizedDescriptorUpload{initDigest: authority.initDigest, object: object, valid: true}, nil
}

func (d AuthorizedDescriptorUpload) ObjectFor(init RegisterInit) ([]byte, bool) {
	if !d.valid {
		return nil, false
	}
	encoded, err := init.MarshalBinary()
	if err != nil {
		return nil, false
	}
	digest := sha256.Sum256(encoded)
	if subtle.ConstantTimeCompare(digest[:], d.initDigest[:]) != 1 {
		return nil, false
	}
	return cloneBytes(d.object), true
}

func challengeAlive(challenge Challenge, now time.Time) bool {
	if challenge.Validate() != nil || now.Unix() < 0 {
		return false
	}
	return uint64(now.Unix()) < challenge.ExpiresAtUnixSeconds
}
