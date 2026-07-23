package v2

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"time"
)

const (
	registerDomain   = "windshare/v2 relay-register\x00"
	resumeDomain     = "windshare/v2 relay-resume\x00"
	stopDomain       = "windshare/v2 relay-stop\x00"
	descriptorDomain = "windshare/v2 object/descriptor"
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
	initDigest      [sha256.Size]byte
	senderPublicKey [SenderPublicKeyBytes]byte
	valid           bool
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
	if !ed25519.Verify(publicKey(proof.SenderPublicKey), preimage, proof.Signature[:]) {
		return SenderAuthority{}, ErrProof
	}
	encoded, err := init.MarshalBinary()
	if err != nil {
		return SenderAuthority{}, err
	}
	authority := SenderAuthority{initDigest: sha256.Sum256(encoded), senderPublicKey: proof.SenderPublicKey, valid: true}
	return authority, nil
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
	if !ed25519.Verify(publicKey(proof.SenderPublicKey), preimage, proof.Signature[:]) {
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

type VerifiedDescriptor struct {
	initDigest [sha256.Size]byte
	object     []byte
	valid      bool
}

// VerifyDescriptorUpload proves that WS2U bytes are exactly the digest committed
// by REGISTER_INIT and are signed by the challenge-authenticated sender key.
func VerifyDescriptorUpload(init RegisterInit, authority SenderAuthority, upload DescriptorUpload) (VerifiedDescriptor, error) {
	if !authority.Authorizes(init) {
		return VerifiedDescriptor{}, ErrProof
	}
	digest := sha256.Sum256(upload.Object)
	if subtle.ConstantTimeCompare(digest[:], init.DescriptorDigest[:]) != 1 {
		return VerifiedDescriptor{}, ErrProof
	}
	if err := verifyDescriptorSignature(init, publicKey(authority.senderPublicKey), upload.Object); err != nil {
		return VerifiedDescriptor{}, ErrProof
	}
	return VerifiedDescriptor{initDigest: authority.initDigest, object: cloneBytes(upload.Object), valid: true}, nil
}

func verifyDescriptorSignature(init RegisterInit, senderPublicKey ed25519.PublicKey, object []byte) error {
	const (
		headerBytes = 8
		nonceBytes  = 12
		tagBytes    = 16
	)
	if len(senderPublicKey) != ed25519.PublicKeySize || len(object) < headerBytes+nonceBytes+tagBytes+ed25519.SignatureSize ||
		len(object) > MaxDescriptorBytes {
		return ErrProof
	}
	header := object[:headerBytes]
	if header[0] != WireVersion || header[1] != 0 || header[2] != 0 || header[3] != 0 {
		return ErrProof
	}
	ciphertextLength := binary.BigEndian.Uint32(header[4:])
	prefixLength := uint64(headerBytes+nonceBytes) + uint64(ciphertextLength)
	if ciphertextLength < tagBytes || prefixLength+ed25519.SignatureSize != uint64(len(object)) {
		return ErrProof
	}
	context := make([]byte, 0, 1+PKHashBytes+ShareIDBytes)
	context = append(context, Suite)
	context = append(context, init.PKHash[:]...)
	context = append(context, init.ShareID[:]...)
	contextHash := sha256.Sum256(context)
	preimage := make([]byte, 0, len(descriptorDomain)+1+sha256.Size+int(prefixLength))
	preimage = append(preimage, descriptorDomain...)
	preimage = append(preimage, 0)
	preimage = append(preimage, contextHash[:]...)
	preimage = append(preimage, object[:prefixLength]...)
	if !ed25519.Verify(senderPublicKey, preimage, object[prefixLength:]) {
		return ErrProof
	}
	return nil
}

func (d VerifiedDescriptor) ObjectFor(init RegisterInit) ([]byte, bool) {
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
