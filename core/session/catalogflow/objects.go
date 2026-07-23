package catalogflow

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/senderobject"
)

var (
	ErrCatalogObjectConflict          = errors.New("catalog sender object identity already has different content")
	ErrCatalogObjectMissing           = errors.New("catalog sender object is not committed")
	ErrSealedCatalogStoreDestroyed    = errors.New("sealed catalog store was destroyed")
	ErrCatalogObjectVerifierDestroyed = errors.New("catalog object verifier was destroyed")
)

type catalogObjectLifecycle struct {
	mu             sync.Mutex
	active         uint64
	stopped        bool
	drained        chan struct{}
	drainedClosed  bool
	secretsCleared bool
}

func newCatalogObjectLifecycle() catalogObjectLifecycle {
	return catalogObjectLifecycle{drained: make(chan struct{})}
}

func (lifecycle *catalogObjectLifecycle) begin(closedErr error) error {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.stopped {
		return closedErr
	}
	lifecycle.active++
	return nil
}

func (lifecycle *catalogObjectLifecycle) end() {
	lifecycle.mu.Lock()
	lifecycle.active--
	lifecycle.closeDrainLocked()
	lifecycle.mu.Unlock()
}

func (lifecycle *catalogObjectLifecycle) closeDrainLocked() {
	if lifecycle.stopped && lifecycle.active == 0 && !lifecycle.drainedClosed {
		close(lifecycle.drained)
		lifecycle.drainedClosed = true
	}
}

func (lifecycle *catalogObjectLifecycle) stop() {
	lifecycle.mu.Lock()
	if lifecycle.drained == nil {
		lifecycle.drained = make(chan struct{})
	}
	lifecycle.stopped = true
	lifecycle.closeDrainLocked()
	lifecycle.mu.Unlock()
}

func (lifecycle *catalogObjectLifecycle) waitForDrain() {
	lifecycle.stop()
	lifecycle.mu.Lock()
	drained := lifecycle.drained
	lifecycle.mu.Unlock()
	<-drained
}

type DescriptorObjectConfig struct {
	PKHash           []byte
	ShareIDRaw       []byte
	DescriptorKey    []byte
	SenderPrivateKey ed25519.PrivateKey
	Nonce            []byte
}

func SealDescriptor(descriptor catalog.ShareDescriptor, config DescriptorObjectConfig) ([]byte, error) {
	plaintext, err := encodeShareDescriptor(descriptor)
	if err != nil {
		return nil, err
	}
	binding, err := senderobject.NewDescriptorBinding(config.PKHash, config.ShareIDRaw)
	if err != nil {
		return nil, err
	}
	return senderobject.Seal(binding, config.DescriptorKey, config.SenderPrivateKey, config.Nonce, plaintext)
}

func OpenDescriptor(object []byte, pkHash, shareIDRaw, descriptorKey []byte) (catalog.ShareDescriptor, error) {
	binding, err := senderobject.NewDescriptorBinding(pkHash, shareIDRaw)
	if err != nil {
		return catalog.ShareDescriptor{}, err
	}
	var descriptor catalog.ShareDescriptor
	plaintext, err := senderobject.OpenDescriptorBootstrap(binding, descriptorKey, object, func(plaintext []byte) (ed25519.PublicKey, error) {
		decoded, decodeErr := decodeShareDescriptor(plaintext)
		if decodeErr != nil {
			return nil, decodeErr
		}
		publicKey := ed25519.PublicKey(decoded.SenderPublicKey())
		hash, hashErr := link.SenderKeyHash(publicKey)
		if hashErr != nil || !bytes.Equal(hash[:], pkHash) {
			return nil, senderobject.ErrSignature
		}
		descriptor = decoded
		return publicKey, nil
	})
	if err != nil {
		return catalog.ShareDescriptor{}, err
	}
	if descriptor.ShareInstance().IsZero() {
		descriptor, err = decodeShareDescriptor(plaintext)
	}
	return descriptor, err
}

type SealedCatalogStoreConfig struct {
	ShareInstance     catalog.ShareInstance
	CatalogKey        []byte
	SenderPrivateKey  ed25519.PrivateKey
	NonceSource       io.Reader
	MaxPageCacheBytes uint64
	MaxFailureBytes   uint64
}

type failureObjectIdentity struct {
	directory catalog.DirectoryID
	attempt   catalog.ScanAttemptID
}

type storedSenderObject struct {
	plaintextDigest [sha256.Size]byte
	object          []byte
}

const (
	DefaultPageObjectCacheBytes = uint64(1) << 20
	DefaultFailureObjectBytes   = uint64(16) << 20
	objectCacheEntryOverhead    = uint64(96)
)

// SealedCatalogStore owns cloned catalog/signing keys and a bounded
// read-through cache; CatalogStore's backend remains the authority for committed
// page bytes. Keeping the roles separate prevents random-nonce objects from
// becoming RAM-only state and gives Destroy one precise secret owner.
type SealedCatalogStore struct {
	lifecycle catalogObjectLifecycle

	share      catalog.ShareInstance
	key        []byte
	privateKey ed25519.PrivateKey
	nonces     io.Reader

	mu              sync.Mutex
	pageCache       map[catalog.PageCommitment][]byte
	pageCacheOrder  []catalog.PageCommitment
	pageCacheBytes  uint64
	maxCacheBytes   uint64
	failures        map[failureObjectIdentity]storedSenderObject
	failureOrder    []failureObjectIdentity
	failureBytes    uint64
	maxFailureBytes uint64
}

func NewSealedCatalogStore(config SealedCatalogStoreConfig) (*SealedCatalogStore, error) {
	if config.ShareInstance.IsZero() || len(config.CatalogKey) != 32 ||
		len(config.SenderPrivateKey) != ed25519.PrivateKeySize || config.NonceSource == nil {
		return nil, errors.New("catalog sender-object store requires share, key, signer, and nonce source")
	}
	if config.MaxPageCacheBytes == 0 {
		config.MaxPageCacheBytes = DefaultPageObjectCacheBytes
	}
	if config.MaxFailureBytes == 0 {
		config.MaxFailureBytes = DefaultFailureObjectBytes
	}
	return &SealedCatalogStore{
		lifecycle: newCatalogObjectLifecycle(),
		share:     config.ShareInstance, key: bytes.Clone(config.CatalogKey),
		privateKey: append(ed25519.PrivateKey(nil), config.SenderPrivateKey...), nonces: config.NonceSource,
		pageCache: make(map[catalog.PageCommitment][]byte), maxCacheBytes: config.MaxPageCacheBytes,
		failures: make(map[failureObjectIdentity]storedSenderObject), maxFailureBytes: config.MaxFailureBytes,
	}, nil
}

func (store *SealedCatalogStore) beginOperation() error {
	return store.lifecycle.begin(ErrSealedCatalogStoreDestroyed)
}

func (store *SealedCatalogStore) endOperation() {
	store.lifecycle.end()
}

// Stop is safe inside NonceSource.Read: it rejects new work without waiting for
// the operation that owns the callback.
func (store *SealedCatalogStore) Stop() {
	if store == nil {
		return
	}
	store.lifecycle.stop()
}

// Destroy is owner-only and must not run from NonceSource.Read because it waits
// for every operation admitted before Stop to release its cryptographic inputs.
func (store *SealedCatalogStore) Destroy() {
	if store == nil {
		return
	}
	store.lifecycle.waitForDrain()
	store.lifecycle.mu.Lock()
	defer store.lifecycle.mu.Unlock()
	if store.lifecycle.secretsCleared {
		return
	}

	clear(store.key)
	store.key = nil
	clear(store.privateKey)
	store.privateKey = nil
	store.nonces = nil

	store.mu.Lock()
	for commitment, object := range store.pageCache {
		clear(object)
		delete(store.pageCache, commitment)
	}
	store.pageCache = nil
	store.pageCacheOrder = nil
	store.pageCacheBytes = 0
	for identity, object := range store.failures {
		clear(object.object)
		delete(store.failures, identity)
	}
	store.failures = nil
	store.failureOrder = nil
	store.failureBytes = 0
	store.mu.Unlock()
	store.lifecycle.secretsCleared = true
}

func (store *SealedCatalogStore) Seal(input catalog.PageCommitInput) (catalog.SealedPageObject, error) {
	if store == nil {
		return catalog.SealedPageObject{}, ErrObjectIdentity
	}
	if err := store.beginOperation(); err != nil {
		return catalog.SealedPageObject{}, err
	}
	defer store.endOperation()
	if input.ShareInstance != store.share {
		return catalog.SealedPageObject{}, ErrObjectIdentity
	}
	plaintext, err := encodeCatalogPageInput(input)
	if err != nil {
		return catalog.SealedPageObject{}, err
	}
	binding, err := senderobject.NewCatalogPageBinding(store.share.Bytes(), input.DirectoryID.Bytes(), input.PageIndex)
	if err != nil {
		return catalog.SealedPageObject{}, err
	}
	object, err := store.seal(binding, plaintext)
	if err != nil {
		return catalog.SealedPageObject{}, err
	}
	sealed, err := catalog.NewSealedPageObject(object)
	if err != nil {
		return catalog.SealedPageObject{}, err
	}
	store.cachePage(sealed)
	return sealed, nil
}

func (store *SealedCatalogStore) cachePage(object catalog.SealedPageObject) {
	encoded := object.Bytes()
	charge := objectCacheEntryOverhead + uint64(len(encoded))
	if charge > store.maxCacheBytes {
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, exists := store.pageCache[object.Commitment()]; exists {
		return
	}
	for store.pageCacheBytes+charge > store.maxCacheBytes && len(store.pageCacheOrder) > 0 {
		oldest := store.pageCacheOrder[0]
		store.pageCacheOrder = store.pageCacheOrder[1:]
		store.pageCacheBytes -= objectCacheEntryOverhead + uint64(len(store.pageCache[oldest]))
		delete(store.pageCache, oldest)
	}
	store.pageCache[object.Commitment()] = encoded
	store.pageCacheOrder = append(store.pageCacheOrder, object.Commitment())
	store.pageCacheBytes += charge
}

func (store *SealedCatalogStore) SealFailure(record catalog.DirectoryFailureRecord) (catalog.SealedFailureObject, error) {
	if store == nil {
		return catalog.SealedFailureObject{}, ErrObjectIdentity
	}
	if err := store.beginOperation(); err != nil {
		return catalog.SealedFailureObject{}, err
	}
	defer store.endOperation()
	failure, err := directoryFailureFromCatalogRecord(record)
	if err != nil || failure.ShareInstance != store.share {
		return catalog.SealedFailureObject{}, errors.Join(ErrObjectIdentity, err)
	}
	plaintext, err := EncodeDirectoryFailure(failure)
	if err != nil {
		return catalog.SealedFailureObject{}, err
	}
	identity := failureObjectIdentity{directory: failure.DirectoryID, attempt: failure.AttemptID}
	digest := sha256.Sum256(plaintext)
	store.mu.Lock()
	defer store.mu.Unlock()
	if existing, ok := store.failures[identity]; ok {
		if existing.plaintextDigest != digest {
			return catalog.SealedFailureObject{}, ErrCatalogObjectConflict
		}
		return catalog.NewSealedFailureObject(existing.object)
	}
	binding, err := senderobject.NewDirectoryErrorBinding(store.share.Bytes(), failure.DirectoryID.Bytes())
	if err != nil {
		return catalog.SealedFailureObject{}, err
	}
	object, err := store.seal(binding, plaintext)
	if err != nil {
		return catalog.SealedFailureObject{}, err
	}
	store.cacheFailureLocked(identity, digest, object)
	return catalog.NewSealedFailureObject(object)
}

func (store *SealedCatalogStore) cacheFailureLocked(
	identity failureObjectIdentity,
	digest [sha256.Size]byte,
	object []byte,
) {
	charge := objectCacheEntryOverhead + uint64(len(object))
	if charge > store.maxFailureBytes {
		return
	}
	for store.failureBytes+charge > store.maxFailureBytes && len(store.failureOrder) > 0 {
		oldest := store.failureOrder[0]
		store.failureOrder = store.failureOrder[1:]
		store.failureBytes -= objectCacheEntryOverhead + uint64(len(store.failures[oldest].object))
		delete(store.failures, oldest)
	}
	store.failures[identity] = storedSenderObject{plaintextDigest: digest, object: object}
	store.failureOrder = append(store.failureOrder, identity)
	store.failureBytes += charge
}

func directoryFailureFromCatalogRecord(record catalog.DirectoryFailureRecord) (DirectoryFailure, error) {
	code, err := directoryCodeForFailureKind(record.Kind())
	if err != nil {
		return DirectoryFailure{}, err
	}
	return NewDirectoryFailure(DirectoryFailure{
		ShareInstance: record.ShareInstance(), DirectoryID: record.DirectoryID(), AttemptID: record.AttemptID(),
		Code: code, Retryable: record.Retryable(), RetryAfter: record.RetryAfter(),
	})
}

func directoryCodeForFailureKind(kind catalog.FailureKind) (uint16, error) {
	switch kind {
	case catalog.FailureKindStale:
		return DirectoryCodeStale, nil
	case catalog.FailureKindPermission:
		return DirectoryCodePermission, nil
	case catalog.FailureKindCollision:
		return DirectoryCodeCollision, nil
	case catalog.FailureKindTooWide:
		return DirectoryCodeTooWide, nil
	case catalog.FailureKindBudget:
		return DirectoryCodeBudget, nil
	case catalog.FailureKindPermanentIO:
		return DirectoryCodePermanentIO, nil
	case catalog.FailureKindTransientIO:
		return DirectoryCodeTransientIO, nil
	default:
		return 0, ErrObjectIdentity
	}
}

func (store *SealedCatalogStore) seal(binding senderobject.Binding, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, senderobject.NonceBytes)
	if _, err := io.ReadFull(store.nonces, nonce); err != nil {
		return nil, fmt.Errorf("catalog sender-object nonce: %w", err)
	}
	return senderobject.Seal(binding, store.key, store.privateKey, nonce, plaintext)
}

func (store *SealedCatalogStore) LoadSealedPage(ctx context.Context, page catalog.CatalogPage) ([]byte, error) {
	if store == nil {
		return nil, ErrCatalogObjectMissing
	}
	if err := store.beginOperation(); err != nil {
		return nil, err
	}
	defer store.endOperation()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if page.ShareInstance() != store.share || page.Commitment().IsZero() {
		return nil, ErrCatalogObjectMissing
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	object := store.pageCache[page.Commitment()]
	if len(object) == 0 {
		return nil, ErrCatalogObjectMissing
	}
	return bytes.Clone(object), nil
}

func (store *SealedCatalogStore) LoadSealedFailure(ctx context.Context, failure DirectoryFailure) ([]byte, error) {
	if store == nil {
		return nil, ErrCatalogObjectMissing
	}
	if err := store.beginOperation(); err != nil {
		return nil, err
	}
	defer store.endOperation()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if failure.ShareInstance != store.share || failure.DirectoryID.IsZero() || failure.AttemptID.IsZero() {
		return nil, ErrCatalogObjectMissing
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	object := store.failures[failureObjectIdentity{directory: failure.DirectoryID, attempt: failure.AttemptID}].object
	if len(object) == 0 {
		return nil, ErrCatalogObjectMissing
	}
	return bytes.Clone(object), nil
}

type CatalogObjectVerifierConfig struct {
	ShareInstance   catalog.ShareInstance
	CatalogKey      []byte
	SenderPublicKey ed25519.PublicKey
}

// CatalogObjectVerifier owns cloned verification inputs so receiver teardown
// can erase its catalog key without mutating capability material held elsewhere.
type CatalogObjectVerifier struct {
	lifecycle catalogObjectLifecycle

	share     catalog.ShareInstance
	key       []byte
	publicKey ed25519.PublicKey
}

func NewCatalogObjectVerifier(config CatalogObjectVerifierConfig) (*CatalogObjectVerifier, error) {
	if config.ShareInstance.IsZero() || len(config.CatalogKey) != 32 || len(config.SenderPublicKey) != ed25519.PublicKeySize {
		return nil, errors.New("catalog object verifier requires share, key, and sender public key")
	}
	return &CatalogObjectVerifier{
		lifecycle: newCatalogObjectLifecycle(),
		share:     config.ShareInstance, key: bytes.Clone(config.CatalogKey),
		publicKey: append(ed25519.PublicKey(nil), config.SenderPublicKey...),
	}, nil
}

func (verifier *CatalogObjectVerifier) beginVerify() error {
	return verifier.lifecycle.begin(ErrCatalogObjectVerifierDestroyed)
}

func (verifier *CatalogObjectVerifier) endVerify() {
	verifier.lifecycle.end()
}

// Stop closes verifier admission without waiting for already-authenticated
// objects to finish decoding.
func (verifier *CatalogObjectVerifier) Stop() {
	if verifier == nil {
		return
	}
	verifier.lifecycle.stop()
}

// Destroy waits for admitted verification to finish before erasing keys, so
// concurrent authentication never observes partially cleared authority.
func (verifier *CatalogObjectVerifier) Destroy() {
	if verifier == nil {
		return
	}
	verifier.lifecycle.waitForDrain()
	verifier.lifecycle.mu.Lock()
	defer verifier.lifecycle.mu.Unlock()
	if verifier.lifecycle.secretsCleared {
		return
	}
	clear(verifier.key)
	verifier.key = nil
	clear(verifier.publicKey)
	verifier.publicKey = nil
	verifier.lifecycle.secretsCleared = true
}

func (verifier *CatalogObjectVerifier) Verify(
	ctx context.Context,
	share catalog.ShareInstance,
	request ListRequest,
	object []byte,
) (VerifiedObject, error) {
	if verifier == nil {
		return VerifiedObject{}, ErrObjectIdentity
	}
	if err := verifier.beginVerify(); err != nil {
		return VerifiedObject{}, err
	}
	defer verifier.endVerify()
	if err := ctx.Err(); err != nil {
		return VerifiedObject{}, err
	}
	if share != verifier.share || len(object) == 0 || len(object) > catalog.MaxCatalogPageObjectBytes {
		return VerifiedObject{}, ErrObjectIdentity
	}
	binding, err := senderobject.NewCatalogPageBinding(share.Bytes(), request.DirectoryID().Bytes(), request.PageIndex())
	if err == nil {
		if plaintext, openErr := senderobject.Open(binding, verifier.key, verifier.publicKey, object); openErr == nil {
			digest := sha256.Sum256(object)
			page, decodeErr := decodeCatalogPage(plaintext, catalog.PageCommitterFunc(func(catalog.PageCommitInput) (catalog.PageCommitment, error) {
				return catalog.NewPageCommitment(digest[:])
			}))
			if decodeErr != nil {
				return VerifiedObject{}, decodeErr
			}
			return VerifiedPage(page), nil
		}
	}
	_, hasGeneration := request.Generation()
	if request.PageIndex() != 0 || hasGeneration {
		return VerifiedObject{}, senderobject.ErrSignature
	}
	failureBinding, err := senderobject.NewDirectoryErrorBinding(share.Bytes(), request.DirectoryID().Bytes())
	if err != nil {
		return VerifiedObject{}, err
	}
	plaintext, err := senderobject.Open(failureBinding, verifier.key, verifier.publicKey, object)
	if err != nil {
		return VerifiedObject{}, err
	}
	failure, err := DecodeDirectoryFailure(plaintext)
	if err != nil {
		return VerifiedObject{}, err
	}
	return VerifiedFailure(failure), nil
}
