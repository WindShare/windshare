package records

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/senderobject"
)

var ErrSealerDestroyed = errors.New("content record sealer was destroyed")

type SealerConfig struct {
	ShareInstance  catalog.ShareInstance
	Keys           *content.KeyTree
	SigningKey     ed25519.PrivateKey
	NonceSource    io.Reader
	MaxSealsPerKey uint64
}

type revisionCacheKey struct {
	file     catalog.FileID
	revision content.FileRevision
}

type segmentSealKey struct {
	file     catalog.FileID
	revision content.FileRevision
	segment  uint64
}

type cachedRevision struct {
	descriptor content.FileRevisionDescriptor
	object     []byte
}

// Sealer is share-scoped. Keeping seal accounting and revision seal-once replay
// in one owner prevents reconnects from accidentally resetting nonce ceilings.
// It owns a clone of SigningKey but only borrows Keys, so Destroy never revokes
// a KeyTree that another share-scoped component still owns.
type Sealer struct {
	lifecycleMu    sync.Mutex
	activeSeals    uint64
	stopped        bool
	drained        chan struct{}
	drainedClosed  bool
	secretsCleared bool

	share       catalog.ShareInstance
	keys        *content.KeyTree
	signingKey  ed25519.PrivateKey
	nonceSource io.Reader
	maxSeals    uint64

	revisionMu      sync.Mutex
	segmentMu       sync.Mutex
	nonceMu         sync.Mutex
	revisions       map[revisionCacheKey]cachedRevision
	fileSealUses    map[catalog.FileID]uint64
	segmentSealUses map[segmentSealKey]uint64
}

func NewSealer(config SealerConfig) (*Sealer, error) {
	if config.ShareInstance.IsZero() || config.Keys == nil || len(config.SigningKey) != ed25519.PrivateKeySize || config.NonceSource == nil {
		return nil, errors.New("content record sealer requires share keys, signing key, and nonce source")
	}
	if config.MaxSealsPerKey == 0 {
		config.MaxSealsPerKey = MaxSealsPerAEADKey
	}
	return &Sealer{
		drained: make(chan struct{}),
		share:   config.ShareInstance, keys: config.Keys,
		signingKey: append(ed25519.PrivateKey(nil), config.SigningKey...), nonceSource: config.NonceSource,
		maxSeals:  config.MaxSealsPerKey,
		revisions: make(map[revisionCacheKey]cachedRevision), fileSealUses: make(map[catalog.FileID]uint64),
		segmentSealUses: make(map[segmentSealKey]uint64),
	}, nil
}

func (s *Sealer) beginSeal() error {
	if s == nil {
		return ErrSealerDestroyed
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.stopped {
		return ErrSealerDestroyed
	}
	s.activeSeals++
	return nil
}

func (s *Sealer) endSeal() {
	s.lifecycleMu.Lock()
	s.activeSeals--
	s.closeDrainLocked()
	s.lifecycleMu.Unlock()
}

func (s *Sealer) closeDrainLocked() {
	if s.stopped && s.activeSeals == 0 && !s.drainedClosed {
		close(s.drained)
		s.drainedClosed = true
	}
}

// Stop is callback-safe: it closes admission without waiting for a seal that
// may currently be inside the caller-provided nonce source.
func (s *Sealer) Stop() {
	if s == nil {
		return
	}
	s.lifecycleMu.Lock()
	if s.drained == nil {
		s.drained = make(chan struct{})
	}
	s.stopped = true
	s.closeDrainLocked()
	s.lifecycleMu.Unlock()
}

// Destroy is an owner-side terminal operation. It must not be invoked from a
// NonceSource callback because destruction waits for that in-flight seal.
func (s *Sealer) Destroy() {
	if s == nil {
		return
	}
	s.Stop()
	s.lifecycleMu.Lock()
	drained := s.drained
	s.lifecycleMu.Unlock()
	<-drained

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.secretsCleared {
		return
	}
	clear(s.signingKey)
	s.signingKey = nil
	// KeyTree is borrowed. Releasing the reference avoids extending its secret
	// lifetime without destroying authority still owned by another component.
	s.keys = nil
	s.nonceSource = nil

	s.revisionMu.Lock()
	for key, cached := range s.revisions {
		clear(cached.object)
		delete(s.revisions, key)
	}
	s.revisions = nil
	s.fileSealUses = nil
	s.revisionMu.Unlock()

	s.segmentMu.Lock()
	s.segmentSealUses = nil
	s.segmentMu.Unlock()
	s.secretsCleared = true
}

func (s *Sealer) SealRevision(descriptor content.FileRevisionDescriptor) ([]byte, error) {
	if err := s.beginSeal(); err != nil {
		return nil, err
	}
	defer s.endSeal()
	if descriptor.ShareInstance() != s.share {
		return nil, ErrObjectIdentity
	}
	cacheKey := revisionCacheKey{file: descriptor.FileID(), revision: descriptor.FileRevision()}
	s.revisionMu.Lock()
	defer s.revisionMu.Unlock()
	if cached, ok := s.revisions[cacheKey]; ok {
		if cached.descriptor != descriptor {
			return nil, ErrObjectIdentity
		}
		return bytes.Clone(cached.object), nil
	}
	plaintext, err := encodeRevision(descriptor)
	if err != nil {
		return nil, fmt.Errorf("encode revision object: %w", err)
	}
	key, err := s.keys.FileObjectKey(descriptor.FileID())
	if err != nil {
		return nil, err
	}
	defer key.Destroy()
	if s.fileSealUses[descriptor.FileID()] >= s.maxSeals {
		return nil, ErrSealLimit
	}
	s.fileSealUses[descriptor.FileID()]++
	committed := false
	defer func() {
		if !committed {
			if s.fileSealUses[descriptor.FileID()] <= 1 {
				delete(s.fileSealUses, descriptor.FileID())
			} else {
				s.fileSealUses[descriptor.FileID()]--
			}
		}
	}()
	binding, err := senderobject.NewRevisionBinding(s.share.Bytes(), descriptor.FileID().Bytes())
	if err != nil {
		return nil, ErrObjectIdentity
	}
	object, err := s.sealObject(binding, key.Bytes(), plaintext)
	if err != nil {
		return nil, err
	}
	committed = true
	s.revisions[cacheKey] = cachedRevision{descriptor: descriptor, object: object}
	return bytes.Clone(object), nil
}

func (s *Sealer) SealBlock(record BlockRecord) (SealedBlock, error) {
	if err := s.beginSeal(); err != nil {
		return SealedBlock{}, err
	}
	defer s.endSeal()
	descriptor := record.descriptor
	if descriptor.ShareInstance() != s.share {
		return SealedBlock{}, ErrObjectIdentity
	}
	segment, err := content.SegmentForBlock(record.index, descriptor.Geometry().ChunkSize())
	if err != nil {
		return SealedBlock{}, err
	}
	sealKey := segmentSealKey{file: descriptor.FileID(), revision: descriptor.FileRevision(), segment: segment}
	if !s.reserveSegmentSeal(sealKey) {
		return SealedBlock{}, ErrSealLimit
	}
	committed := false
	defer func() {
		if !committed {
			s.releaseSegmentSeal(sealKey)
		}
	}()
	plaintext, err := encodeBlock(record)
	if err != nil {
		return SealedBlock{}, fmt.Errorf("encode block record: %w", err)
	}
	key, err := s.keys.FileSegmentKey(descriptor.FileID(), descriptor.FileRevision(), record.index, descriptor.Geometry().ChunkSize())
	if err != nil {
		return SealedBlock{}, err
	}
	defer key.Destroy()
	binding, err := senderobject.NewBlockRecordBinding(
		descriptor.ShareInstance().Bytes(), descriptor.FileID().Bytes(), descriptor.FileRevision().Bytes(),
		record.index, uint32(len(record.data)),
	)
	if err != nil {
		return SealedBlock{}, ErrObjectIdentity
	}
	object, err := s.sealObject(binding, key.Bytes(), plaintext)
	if err != nil {
		return SealedBlock{}, err
	}
	if len(object) > MaxBlockRecordObjectBytes {
		return SealedBlock{}, ErrObjectTooLarge
	}
	committed = true
	return SealedBlock{ID: RecordIDFromObject(object), Object: object}, nil
}

func (s *Sealer) reserveSegmentSeal(key segmentSealKey) bool {
	s.segmentMu.Lock()
	defer s.segmentMu.Unlock()
	if s.segmentSealUses[key] >= s.maxSeals {
		return false
	}
	s.segmentSealUses[key]++
	return true
}

func (s *Sealer) releaseSegmentSeal(key segmentSealKey) {
	s.segmentMu.Lock()
	if s.segmentSealUses[key] <= 1 {
		delete(s.segmentSealUses, key)
	} else {
		s.segmentSealUses[key]--
	}
	s.segmentMu.Unlock()
}

func (s *Sealer) sealObject(binding senderobject.Binding, key, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, ObjectNonceBytes)
	s.nonceMu.Lock()
	if _, err := io.ReadFull(s.nonceSource, nonce); err != nil {
		s.nonceMu.Unlock()
		return nil, fmt.Errorf("%w: %w", ErrNonceSource, err)
	}
	s.nonceMu.Unlock()
	object, err := senderobject.Seal(binding, key, s.signingKey, nonce, plaintext)
	return object, mapSenderObjectError(err)
}

type OpenerConfig struct {
	ShareInstance   catalog.ShareInstance
	Keys            *content.KeyTree
	VerificationKey ed25519.PublicKey
}

type Opener struct {
	share           catalog.ShareInstance
	keys            *content.KeyTree
	verificationKey ed25519.PublicKey
}

func NewOpener(config OpenerConfig) (*Opener, error) {
	if config.ShareInstance.IsZero() || config.Keys == nil || len(config.VerificationKey) != ed25519.PublicKeySize {
		return nil, errors.New("content record opener requires share keys and a verification key")
	}
	return &Opener{
		share: config.ShareInstance, keys: config.Keys,
		verificationKey: append(ed25519.PublicKey(nil), config.VerificationKey...),
	}, nil
}

func (o *Opener) OpenRevision(file catalog.FileID, chunkSize uint32, object []byte) (content.FileRevisionDescriptor, error) {
	if file.IsZero() {
		return content.FileRevisionDescriptor{}, ErrObjectIdentity
	}
	key, err := o.keys.FileObjectKey(file)
	if err != nil {
		return content.FileRevisionDescriptor{}, err
	}
	defer key.Destroy()
	binding, err := senderobject.NewRevisionBinding(o.share.Bytes(), file.Bytes())
	if err != nil {
		return content.FileRevisionDescriptor{}, ErrObjectIdentity
	}
	plaintext, err := o.openObject(binding, key.Bytes(), object)
	if err != nil {
		return content.FileRevisionDescriptor{}, err
	}
	return decodeRevision(plaintext, o.share, file, chunkSize)
}

func (o *Opener) OpenBlock(descriptor content.FileRevisionDescriptor, index uint64, object []byte) (BlockRecord, error) {
	if descriptor.ShareInstance() != o.share {
		return BlockRecord{}, ErrObjectIdentity
	}
	plainLength, err := descriptor.Geometry().BlockPlainLength(index)
	if err != nil {
		return BlockRecord{}, err
	}
	key, err := o.keys.FileSegmentKey(descriptor.FileID(), descriptor.FileRevision(), index, descriptor.Geometry().ChunkSize())
	if err != nil {
		return BlockRecord{}, err
	}
	defer key.Destroy()
	binding, err := senderobject.NewBlockRecordBinding(
		descriptor.ShareInstance().Bytes(), descriptor.FileID().Bytes(), descriptor.FileRevision().Bytes(), index, plainLength,
	)
	if err != nil {
		return BlockRecord{}, ErrObjectIdentity
	}
	plaintext, err := o.openObject(binding, key.Bytes(), object)
	if err != nil {
		return BlockRecord{}, err
	}
	return decodeBlock(plaintext, descriptor, index)
}

func (o *Opener) openObject(binding senderobject.Binding, key, object []byte) ([]byte, error) {
	plaintext, err := senderobject.Open(binding, key, o.verificationKey, object)
	return plaintext, mapSenderObjectError(err)
}

func mapSenderObjectError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, senderobject.ErrTooLarge):
		return errors.Join(ErrObjectTooLarge, err)
	case errors.Is(err, senderobject.ErrMalformed):
		return errors.Join(ErrObjectMalformed, err)
	case errors.Is(err, senderobject.ErrSignature):
		return errors.Join(ErrObjectSignature, err)
	case errors.Is(err, senderobject.ErrAuth):
		return errors.Join(ErrObjectAuth, err)
	case errors.Is(err, senderobject.ErrBinding):
		return errors.Join(ErrObjectIdentity, err)
	default:
		return err
	}
}
