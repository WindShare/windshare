package content

import (
	"errors"
	"fmt"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/internal/keyderiv"
)

const (
	ReadSecretBytes       = keyderiv.V2ReadSecretBytes
	PKHashBytes           = keyderiv.V2PKHashBytes
	DerivedKeyBytes       = keyderiv.KeyBytes
	SegmentPlaintextBytes = uint64(16) << 30

	DescriptorKeyLabel  = keyderiv.V2DescriptorLabel
	CatalogKeyLabel     = keyderiv.V2CatalogLabel
	FileObjectKeyLabel  = keyderiv.V2FileObjectLabel
	RevisionKeyLabel    = keyderiv.V2RevisionLabel
	FileSegmentKeyLabel = keyderiv.V2FileSegmentLabel
	SessionAuthKeyLabel = keyderiv.V2SessionAuthLabel
)

type DerivedKey struct{ value [DerivedKeyBytes]byte }

func (k DerivedKey) Bytes() []byte { return append([]byte(nil), k.value[:]...) }

func (k *DerivedKey) Destroy() {
	if k == nil {
		return
	}
	clear(k.value[:])
}

type KeyTree struct {
	mu         sync.RWMutex
	readSecret [ReadSecretBytes]byte
	share      catalog.ShareInstance
	destroyed  bool
}

var ErrKeyTreeDestroyed = errors.New("content key tree was destroyed")

func NewKeyTree(readSecret []byte, share catalog.ShareInstance) (*KeyTree, error) {
	if len(readSecret) != ReadSecretBytes || share.IsZero() {
		return nil, errors.New("content key tree requires a 16-byte secret and nonzero share identity")
	}
	tree := &KeyTree{share: share}
	copy(tree.readSecret[:], readSecret)
	return tree, nil
}

func ownDerivedKey(raw []byte, err error) (DerivedKey, error) {
	if err != nil {
		return DerivedKey{}, err
	}
	if len(raw) != DerivedKeyBytes {
		return DerivedKey{}, keyderiv.ErrV2KeyMaterial
	}
	var result DerivedKey
	copy(result.value[:], raw)
	clear(raw)
	return result, nil
}

func (t *KeyTree) DescriptorKey(pkHash []byte) (DerivedKey, error) {
	if t == nil || len(pkHash) != PKHashBytes {
		return DerivedKey{}, errors.New("content descriptor key requires a 16-byte public-key hash")
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.destroyed {
		return DerivedKey{}, ErrKeyTreeDestroyed
	}
	return ownDerivedKey(keyderiv.V2DescriptorKey(t.readSecret[:], pkHash))
}

func (t *KeyTree) CatalogKey() (DerivedKey, error) {
	if t == nil {
		return DerivedKey{}, errors.New("content key tree is nil")
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.destroyed {
		return DerivedKey{}, ErrKeyTreeDestroyed
	}
	return ownDerivedKey(keyderiv.V2CatalogKey(t.readSecret[:], t.share.Bytes()))
}

func (t *KeyTree) SessionAuthKey() (DerivedKey, error) {
	if t == nil {
		return DerivedKey{}, errors.New("content key tree is nil")
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.destroyed {
		return DerivedKey{}, ErrKeyTreeDestroyed
	}
	return ownDerivedKey(keyderiv.V2SessionAuthKey(t.readSecret[:], t.share.Bytes()))
}

func (t *KeyTree) FileObjectKey(file catalog.FileID) (DerivedKey, error) {
	if t == nil || file.IsZero() {
		return DerivedKey{}, errors.New("content file-object key requires a file identity")
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.destroyed {
		return DerivedKey{}, ErrKeyTreeDestroyed
	}
	return t.fileObjectKey(file)
}

func (t *KeyTree) fileObjectKey(file catalog.FileID) (DerivedKey, error) {
	return ownDerivedKey(keyderiv.V2FileObjectKey(t.readSecret[:], t.share.Bytes(), file.Bytes()))
}

func (t *KeyTree) RevisionKey(file catalog.FileID, revision FileRevision) (DerivedKey, error) {
	if t == nil {
		return DerivedKey{}, errors.New("content key tree is nil")
	}
	if revision.IsZero() {
		return DerivedKey{}, errors.New("content revision key requires a revision identity")
	}
	if file.IsZero() {
		return DerivedKey{}, errors.New("content file-object key requires a file identity")
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.destroyed {
		return DerivedKey{}, ErrKeyTreeDestroyed
	}
	return t.revisionKey(file, revision)
}

func (t *KeyTree) revisionKey(file catalog.FileID, revision FileRevision) (DerivedKey, error) {
	fileKey, err := t.fileObjectKey(file)
	if err != nil {
		return DerivedKey{}, err
	}
	defer fileKey.Destroy()
	return ownDerivedKey(keyderiv.V2RevisionKey(fileKey.value[:], revision.Bytes()))
}

func SegmentForBlock(localBlockIndex uint64, chunkSize uint32) (uint64, error) {
	if chunkSize < catalog.MinChunkSize || chunkSize > catalog.MaxChunkSize || chunkSize&(chunkSize-1) != 0 || SegmentPlaintextBytes%uint64(chunkSize) != 0 {
		return 0, fmt.Errorf("%w: segment chunk size %d", ErrInvalidGeometry, chunkSize)
	}
	// All allowed chunks divide the 16 GiB segment. Dividing the block index
	// avoids overflowing LocalBlockIndex*chunkSize at the u64 boundary.
	return localBlockIndex / (SegmentPlaintextBytes / uint64(chunkSize)), nil
}

func (t *KeyTree) FileSegmentKey(file catalog.FileID, revision FileRevision, localBlockIndex uint64, chunkSize uint32) (DerivedKey, error) {
	if t == nil || file.IsZero() || revision.IsZero() {
		return DerivedKey{}, errors.New("content file-segment key requires file and revision identities")
	}
	segment, err := SegmentForBlock(localBlockIndex, chunkSize)
	if err != nil {
		return DerivedKey{}, err
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.destroyed {
		return DerivedKey{}, ErrKeyTreeDestroyed
	}
	revisionKey, err := t.revisionKey(file, revision)
	if err != nil {
		return DerivedKey{}, err
	}
	defer revisionKey.Destroy()
	return ownDerivedKey(keyderiv.V2FileSegmentKey(revisionKey.value[:], segment))
}

func (t *KeyTree) Destroy() {
	if t == nil {
		return
	}
	t.mu.Lock()
	clear(t.readSecret[:])
	t.destroyed = true
	t.mu.Unlock()
}
