package keyderiv

import (
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
)

// KeyBytes is the uniform output length (AES-256 key) for all derived keys.
const KeyBytes = 32

const (
	V2ReadSecretBytes = 16
	V2IdentityBytes   = 16
	V2PKHashBytes     = 16
)

const (
	V2DescriptorLabel  = "windshare/v2 descriptor"
	V2SessionAuthLabel = "windshare/v2 session-auth"
)

var ErrV2KeyMaterial = errors.New("keyderiv: invalid suite-0x02 key material")

// V2DescriptorKey binds the bootstrap object key to the sender public-key hash.
func V2DescriptorKey(readSecret, pkHash []byte) ([]byte, error) {
	if len(readSecret) != V2ReadSecretBytes || len(pkHash) != V2PKHashBytes {
		return nil, ErrV2KeyMaterial
	}
	return deriveV2(readSecret, V2DescriptorLabel, pkHash), nil
}

// V2SessionAuthKey authenticates the transcript before sender resource admission.
func V2SessionAuthKey(readSecret, shareInstance []byte) ([]byte, error) {
	if len(readSecret) != V2ReadSecretBytes || len(shareInstance) != V2IdentityBytes {
		return nil, ErrV2KeyMaterial
	}
	return deriveV2(readSecret, V2SessionAuthLabel, shareInstance), nil
}

func deriveV2(secret []byte, label string, context []byte) []byte {
	info := make([]byte, 0, len(label)+1+len(context))
	info = append(info, label...)
	info = append(info, 0)
	info = append(info, context...)

	// Per-share uniqueness comes from readSecret, so an empty salt keeps Go and
	// TypeScript aligned on info alone.
	key, err := hkdf.Key(sha256.New, secret, nil, string(info), KeyBytes)
	if err != nil {
		// Unreachable: KeyBytes (32) is far below HKDF-SHA256's 255*32 limit;
		// failing closed beats silently returning wrong key material.
		panic(fmt.Sprintf("keyderiv: HKDF derivation failed: %v", err))
	}
	return key
}
