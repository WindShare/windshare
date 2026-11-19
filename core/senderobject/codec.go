// Package senderobject seals and authenticates the suite-0x02 bootstrap descriptor.
package senderobject

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"

	"github.com/windshare/windshare/core/internal/keyderiv"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/senderauth"
)

const (
	// The descriptor version owns the signature construction independently of the session wire.
	wireVersion    = byte(3)
	headerBytes    = 8
	NonceBytes     = 12
	signatureBytes = ed25519.SignatureSize
	tagBytes       = 16
	prefixBytes    = headerBytes + NonceBytes
	fixedBytes     = prefixBytes + signatureBytes

	maxDescriptorBytes = 16 << 10
	descriptorDomain   = "windshare/v2 object/descriptor\x00"
)

var (
	ErrBinding   = errors.New("senderobject: invalid descriptor route binding")
	ErrKey       = errors.New("senderobject: invalid key material")
	ErrMalformed = errors.New("senderobject: malformed descriptor object")
	ErrTooLarge  = errors.New("senderobject: descriptor object exceeds its byte limit")
	ErrSignature = errors.New("senderobject: sender signature is invalid")
	ErrAuth      = errors.New("senderobject: ciphertext authentication failed")
)

type descriptorBinding struct {
	pkHash      [link.PKHashBytes]byte
	contextHash [sha256.Size]byte
}

func bindDescriptor(pkHash, shareIDRaw []byte) (descriptorBinding, error) {
	if len(pkHash) != link.PKHashBytes || len(shareIDRaw) != link.SenderAuthenticatedShareIDBytes {
		return descriptorBinding{}, ErrBinding
	}
	expected, err := link.ShareIDForSenderKeyHash(pkHash)
	if err != nil {
		return descriptorBinding{}, ErrBinding
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(expected)
	if err != nil || !bytes.Equal(decoded, shareIDRaw) {
		return descriptorBinding{}, ErrBinding
	}
	context := [1 + link.PKHashBytes + link.SenderAuthenticatedShareIDBytes]byte{link.SuiteSenderAuthenticated}
	copy(context[1:], pkHash)
	copy(context[1+link.PKHashBytes:], shareIDRaw)
	return descriptorBinding{pkHash: [link.PKHashBytes]byte(pkHash), contextHash: sha256.Sum256(context[:])}, nil
}

func SealDescriptor(pkHash, shareIDRaw, key []byte, signingKey ed25519.PrivateKey, nonce, plaintext []byte) ([]byte, error) {
	binding, err := bindDescriptor(pkHash, shareIDRaw)
	if err != nil {
		return nil, err
	}
	if len(key) != keyderiv.KeyBytes || len(signingKey) != ed25519.PrivateKeySize || len(nonce) != NonceBytes {
		return nil, ErrKey
	}
	if len(plaintext) == 0 {
		return nil, ErrMalformed
	}
	if len(plaintext) > maxDescriptorBytes-(fixedBytes+tagBytes) {
		return nil, ErrTooLarge
	}
	aead, err := descriptorAEAD(key)
	if err != nil {
		return nil, ErrKey
	}
	header := [headerBytes]byte{wireVersion}
	binary.BigEndian.PutUint32(header[4:], uint32(len(plaintext)+aead.Overhead()))
	prefix := make([]byte, 0, fixedBytes+tagBytes+len(plaintext))
	prefix = append(prefix, header[:]...)
	prefix = append(prefix, nonce...)
	prefix = aead.Seal(prefix, nonce, plaintext, binding.authenticate(header[:]))
	objectHash := sha256.Sum256(prefix)
	return append(prefix, ed25519.Sign(signingKey, binding.authenticate(objectHash[:]))...), nil
}

// OpenDescriptorBootstrap decrypts only to discover the sender key, then checks
// pkHash and the outer signature before releasing plaintext to the caller.
func OpenDescriptorBootstrap(pkHash, shareIDRaw, key, object []byte, senderIdentity func([]byte) (*senderauth.Verifier, error)) ([]byte, error) {
	if senderIdentity == nil {
		return nil, ErrBinding
	}
	binding, err := bindDescriptor(pkHash, shareIDRaw)
	if err != nil {
		return nil, err
	}
	if len(key) != keyderiv.KeyBytes {
		return nil, ErrKey
	}
	prefixEnd, err := descriptorPrefixEnd(object)
	if err != nil {
		return nil, err
	}
	aead, err := descriptorAEAD(key)
	if err != nil {
		return nil, ErrKey
	}
	plaintext, err := aead.Open(nil, object[headerBytes:prefixBytes], object[prefixBytes:prefixEnd], binding.authenticate(object[:headerBytes]))
	if err != nil {
		return nil, ErrAuth
	}
	// The callback can run caller code. Authenticate the same sealed bytes that
	// produced plaintext even if the caller reuses its input buffer.
	objectHash := sha256.Sum256(object[:prefixEnd])
	signature := [signatureBytes]byte(object[prefixEnd:])
	sender, err := senderIdentity(plaintext)
	if err != nil || !sender.Valid() {
		return nil, ErrKey
	}
	hash, err := link.SenderKeyHash(sender.PublicKey())
	if err != nil || hash != binding.pkHash || !sender.Verify(binding.authenticate(objectHash[:]), signature[:]) {
		return nil, ErrSignature
	}
	return plaintext, nil
}

func descriptorPrefixEnd(object []byte) (int, error) {
	if len(object) > maxDescriptorBytes {
		return 0, ErrTooLarge
	}
	if len(object) < fixedBytes+tagBytes {
		return 0, ErrMalformed
	}
	if object[0] != wireVersion || object[1] != 0 || object[2] != 0 || object[3] != 0 {
		return 0, ErrMalformed
	}
	ciphertextLength := binary.BigEndian.Uint32(object[4:headerBytes])
	if ciphertextLength < tagBytes || uint64(fixedBytes)+uint64(ciphertextLength) != uint64(len(object)) {
		return 0, ErrMalformed
	}
	return len(object) - signatureBytes, nil
}

func (binding descriptorBinding) authenticate(suffix []byte) []byte {
	result := make([]byte, 0, len(descriptorDomain)+sha256.Size+len(suffix))
	result = append(result, descriptorDomain...)
	result = append(result, binding.contextHash[:]...)
	return append(result, suffix...)
}

func descriptorAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
