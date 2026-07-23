// Package senderobject implements the transport-neutral suite-0x02 object envelope.
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
	"fmt"

	"github.com/windshare/windshare/core/link"
)

const (
	WireVersion    = byte(2)
	HeaderBytes    = 8
	NonceBytes     = 12
	SignatureBytes = ed25519.SignatureSize
	TagBytes       = 16
	FixedBytes     = HeaderBytes + NonceBytes + SignatureBytes

	MaxDescriptorBytes     = 16 << 10
	MaxCatalogPageBytes    = 60 << 10
	MaxDirectoryErrorBytes = 16 << 10
	MaxRevisionBytes       = 16 << 10
	MaxBlockRecordBytes    = (4 << 20) + 512
	MaxOfflineCommitBytes  = 16 << 10
)

type Domain string

const (
	DomainDescriptor     Domain = "windshare/v2 object/descriptor"
	DomainCatalogPage    Domain = "windshare/v2 object/catalog-page"
	DomainDirectoryError Domain = "windshare/v2 object/directory-error"
	DomainRevision       Domain = "windshare/v2 object/file-revision"
	DomainBlockRecord    Domain = "windshare/v2 object/block-record"
	DomainOfflineCommit  Domain = "windshare/v2 object/offline-commit"
)

var (
	ErrBinding   = errors.New("senderobject: invalid semantic binding")
	ErrKey       = errors.New("senderobject: invalid key material")
	ErrMalformed = errors.New("senderobject: malformed object")
	ErrTooLarge  = errors.New("senderobject: object exceeds its domain limit")
	ErrSignature = errors.New("senderobject: sender signature is invalid")
	ErrAuth      = errors.New("senderobject: ciphertext authentication failed")
)

// Binding owns the exact domain, context, and byte ceiling for one semantic object.
// Callers cannot construct an unbounded or context-free envelope.
type Binding struct {
	domain   Domain
	context  []byte
	maxBytes int
}

func (b Binding) Domain() Domain  { return b.domain }
func (b Binding) Context() []byte { return bytes.Clone(b.context) }
func (b Binding) MaxBytes() int   { return b.maxBytes }

func NewDescriptorBinding(pkHash, shareIDRaw []byte) (Binding, error) {
	if len(pkHash) != link.PKHashBytes || len(shareIDRaw) != link.SenderAuthenticatedShareIDBytes {
		return Binding{}, ErrBinding
	}
	expected, err := link.ShareIDForSenderKeyHash(pkHash)
	if err != nil {
		return Binding{}, ErrBinding
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(expected)
	if err != nil {
		return Binding{}, ErrBinding
	}
	if !bytes.Equal(decoded, shareIDRaw) {
		return Binding{}, ErrBinding
	}
	context := make([]byte, 1+len(pkHash)+len(shareIDRaw))
	context[0] = link.SuiteSenderAuthenticated
	copy(context[1:], pkHash)
	copy(context[1+len(pkHash):], shareIDRaw)
	return newBinding(DomainDescriptor, context, MaxDescriptorBytes), nil
}

func NewCatalogPageBinding(shareInstance, directoryID []byte, page uint32) (Binding, error) {
	context, err := twoIdentities(shareInstance, directoryID)
	if err != nil {
		return Binding{}, err
	}
	context = binary.BigEndian.AppendUint32(context, page)
	return newBinding(DomainCatalogPage, context, MaxCatalogPageBytes), nil
}

func NewDirectoryErrorBinding(shareInstance, directoryID []byte) (Binding, error) {
	context, err := twoIdentities(shareInstance, directoryID)
	if err != nil {
		return Binding{}, err
	}
	return newBinding(DomainDirectoryError, context, MaxDirectoryErrorBytes), nil
}

func NewRevisionBinding(shareInstance, fileID []byte) (Binding, error) {
	context, err := twoIdentities(shareInstance, fileID)
	if err != nil {
		return Binding{}, err
	}
	return newBinding(DomainRevision, context, MaxRevisionBytes), nil
}

func NewBlockRecordBinding(shareInstance, fileID, revision []byte, localBlock uint64, dataLength uint32) (Binding, error) {
	if !identity(shareInstance) || !identity(fileID) || !identity(revision) {
		return Binding{}, ErrBinding
	}
	context := make([]byte, 0, 16*3+8+4)
	context = append(context, shareInstance...)
	context = append(context, fileID...)
	context = append(context, revision...)
	context = binary.BigEndian.AppendUint64(context, localBlock)
	context = binary.BigEndian.AppendUint32(context, dataLength)
	return newBinding(DomainBlockRecord, context, MaxBlockRecordBytes), nil
}

func NewOfflineCommitBinding(shareInstance []byte) (Binding, error) {
	if !identity(shareInstance) {
		return Binding{}, ErrBinding
	}
	return newBinding(DomainOfflineCommit, bytes.Clone(shareInstance), MaxOfflineCommitBytes), nil
}

func newBinding(domain Domain, context []byte, maxBytes int) Binding {
	return Binding{domain: domain, context: bytes.Clone(context), maxBytes: maxBytes}
}

func twoIdentities(left, right []byte) ([]byte, error) {
	if !identity(left) || !identity(right) {
		return nil, ErrBinding
	}
	result := make([]byte, 0, 32)
	result = append(result, left...)
	return append(result, right...), nil
}

func identity(value []byte) bool {
	if len(value) != 16 {
		return false
	}
	for _, item := range value {
		if item != 0 {
			return true
		}
	}
	return false
}

func Seal(binding Binding, key []byte, signingKey ed25519.PrivateKey, nonce, plaintext []byte) ([]byte, error) {
	if err := validateBinding(binding); err != nil {
		return nil, err
	}
	if len(key) != 32 || len(signingKey) != ed25519.PrivateKeySize || len(nonce) != NonceBytes {
		return nil, ErrKey
	}
	if len(plaintext) == 0 {
		return nil, ErrMalformed
	}
	if len(plaintext) > binding.maxBytes-(FixedBytes+TagBytes) {
		return nil, ErrTooLarge
	}
	aead, err := objectAEAD(key)
	if err != nil {
		return nil, ErrKey
	}
	header := make([]byte, HeaderBytes)
	header[0] = WireVersion
	binary.BigEndian.PutUint32(header[4:], uint32(len(plaintext)+aead.Overhead()))
	contextHash := sha256.Sum256(binding.context)
	aad := authenticationData(binding.domain, contextHash, header)
	prefix := make([]byte, 0, FixedBytes+TagBytes+len(plaintext))
	prefix = append(prefix, header...)
	prefix = append(prefix, nonce...)
	prefix = aead.Seal(prefix, nonce, plaintext, aad)
	preimage := signaturePreimage(binding.domain, contextHash, prefix)
	return append(prefix, ed25519.Sign(signingKey, preimage)...), nil
}

func Verify(binding Binding, verificationKey ed25519.PublicKey, object []byte) error {
	if err := validateBinding(binding); err != nil {
		return err
	}
	if len(verificationKey) != ed25519.PublicKeySize {
		return ErrKey
	}
	prefix, signature, _, _, _, err := split(binding, object)
	if err != nil {
		return err
	}
	contextHash := sha256.Sum256(binding.context)
	if !ed25519.Verify(verificationKey, signaturePreimage(binding.domain, contextHash, prefix), signature) {
		return ErrSignature
	}
	return nil
}

func Open(binding Binding, key []byte, verificationKey ed25519.PublicKey, object []byte) ([]byte, error) {
	if err := Verify(binding, verificationKey, object); err != nil {
		return nil, err
	}
	return decrypt(binding, key, object)
}

// OpenDescriptorBootstrap decrypts only to discover the sender key, then checks
// pkHash and the outer signature before releasing plaintext to the caller.
func OpenDescriptorBootstrap(binding Binding, key []byte, object []byte, senderKey func([]byte) (ed25519.PublicKey, error)) ([]byte, error) {
	if binding.domain != DomainDescriptor || senderKey == nil {
		return nil, ErrBinding
	}
	plaintext, err := decrypt(binding, key, object)
	if err != nil {
		return nil, err
	}
	publicKey, err := senderKey(plaintext)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return nil, ErrKey
	}
	hash, err := link.SenderKeyHash(publicKey)
	if err != nil || !bytes.Equal(hash[:], binding.context[1:1+link.PKHashBytes]) {
		return nil, ErrSignature
	}
	if err := Verify(binding, publicKey, object); err != nil {
		return nil, err
	}
	return plaintext, nil
}

func decrypt(binding Binding, key, object []byte) ([]byte, error) {
	if err := validateBinding(binding); err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, ErrKey
	}
	_, _, header, nonce, ciphertext, err := split(binding, object)
	if err != nil {
		return nil, err
	}
	aead, err := objectAEAD(key)
	if err != nil {
		return nil, ErrKey
	}
	contextHash := sha256.Sum256(binding.context)
	plaintext, err := aead.Open(nil, nonce, ciphertext, authenticationData(binding.domain, contextHash, header))
	if err != nil {
		return nil, ErrAuth
	}
	return plaintext, nil
}

func split(binding Binding, object []byte) (prefix, signature, header, nonce, ciphertext []byte, err error) {
	if len(object) > binding.maxBytes {
		return nil, nil, nil, nil, nil, ErrTooLarge
	}
	if len(object) < FixedBytes+TagBytes {
		return nil, nil, nil, nil, nil, ErrMalformed
	}
	header = object[:HeaderBytes]
	if header[0] != WireVersion || header[1] != 0 || header[2] != 0 || header[3] != 0 {
		return nil, nil, nil, nil, nil, ErrMalformed
	}
	ciphertextLength := binary.BigEndian.Uint32(header[4:])
	if ciphertextLength < TagBytes {
		return nil, nil, nil, nil, nil, ErrMalformed
	}
	prefixLength := uint64(HeaderBytes+NonceBytes) + uint64(ciphertextLength)
	if prefixLength+SignatureBytes != uint64(len(object)) {
		return nil, nil, nil, nil, nil, ErrMalformed
	}
	prefix = object[:prefixLength]
	signature = object[prefixLength:]
	nonce = object[HeaderBytes : HeaderBytes+NonceBytes]
	ciphertext = object[HeaderBytes+NonceBytes : prefixLength]
	return prefix, signature, header, nonce, ciphertext, nil
}

func validateBinding(binding Binding) error {
	if binding.domain == "" || len(binding.context) == 0 || binding.maxBytes < FixedBytes+TagBytes+1 {
		return ErrBinding
	}
	return nil
}

func authenticationData(domain Domain, contextHash [sha256.Size]byte, header []byte) []byte {
	result := make([]byte, 0, len(domain)+1+sha256.Size+HeaderBytes)
	result = append(result, domain...)
	result = append(result, 0)
	result = append(result, contextHash[:]...)
	return append(result, header...)
}

func signaturePreimage(domain Domain, contextHash [sha256.Size]byte, prefix []byte) []byte {
	result := make([]byte, 0, len(domain)+1+sha256.Size+len(prefix))
	result = append(result, domain...)
	result = append(result, 0)
	result = append(result, contextHash[:]...)
	return append(result, prefix...)
}

func objectAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("senderobject: create AES: %w", err)
	}
	return cipher.NewGCM(block)
}
