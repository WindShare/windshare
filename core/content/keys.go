package content

import (
	"errors"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/internal/keyderiv"
)

const (
	ReadSecretBytes = keyderiv.V2ReadSecretBytes
	PKHashBytes     = keyderiv.V2PKHashBytes
	DerivedKeyBytes = keyderiv.KeyBytes

	DescriptorKeyLabel  = keyderiv.V2DescriptorLabel
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

func (t *KeyTree) Destroy() {
	if t == nil {
		return
	}
	t.mu.Lock()
	clear(t.readSecret[:])
	t.destroyed = true
	t.mu.Unlock()
}
