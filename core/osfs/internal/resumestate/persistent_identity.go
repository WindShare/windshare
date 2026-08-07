// Package resumestate defines the platform-neutral durable state used by the
// native filesystem output backend. It deliberately models identity and crash
// recovery without persisting inode numbers or Windows file IDs.
package resumestate

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
)

const (
	// SchemaVersion identifies the current durable output namespace. The former
	// selection-shaped journal is intentionally not decoded: FileCheckpointV1 is
	// the only resumable file state that a reopened output may trust.
	SchemaVersion                 = uint32(1)
	OutputObjectIDBytes           = sha256.Size
	OutputRootBindingBytes        = sha256.Size
	OutputAncestryBindingBytes    = sha256.Size
	BootstrapNonceBytes           = sha256.Size
	MaxRootIdentityClaimBytes     = 256
	MaxAncestryIdentityClaimBytes = 256
	MaxDurableRangesPerFile       = 16_384
	MaxFilesPerSession            = 1_048_576
	MaxSelectedEntriesPerSession  = 1_048_576
	MaxSessionsPerIntent          = 64
)

var (
	ErrInvalidState      = errors.New("osfs resumestate value is invalid")
	ErrCorruptState      = errors.New("osfs resumestate record is corrupt")
	ErrInvalidTransition = errors.New("osfs resumestate transition is invalid")
)

type (
	// OutputObjectID owns an internal namespace; current-object same-file checks,
	// rather than these random bytes, prove that stage, anchor, and final are links
	// to one live object.
	OutputObjectID [OutputObjectIDBytes]byte
	LocatorDigest  [sha256.Size]byte
	BootstrapNonce [BootstrapNonceBytes]byte
)

// OutputRootBinding is derived from both canonical volume and current-directory
// identity claims. The claims remain comparison hints, but binding both prevents
// copied control state from authenticating beneath a different root handle.
type OutputRootBinding struct {
	certification CertificationID
	digest        [OutputRootBindingBytes]byte
}

// OutputAncestryIdentityClaim is one exact native directory identity in the
// canonical root-to-selected-parent closure. The opaque claim is consumed only
// while deriving a binding; native identity material is never persisted.
type OutputAncestryIdentityClaim struct {
	CanonicalPath string
	IdentityClaim []byte
}

// OutputAncestryBinding commits to the complete ordered ancestry closure for
// one canonical selection. A fixed digest keeps the durable header bounded even
// when the selection approaches its admission limit.
type OutputAncestryBinding struct {
	digest [OutputAncestryBindingBytes]byte
}

func NewOutputAncestryBinding(
	root OutputRootBinding,
	selection transfer.SelectionIdentity,
	claims []OutputAncestryIdentityClaim,
) (OutputAncestryBinding, error) {
	if !root.valid() || selection.IsZero() || len(claims) == 0 ||
		len(claims) > MaxSelectedEntriesPerSession+1 {
		return OutputAncestryBinding{}, fmt.Errorf("%w: output ancestry binding inputs", ErrInvalidState)
	}
	if claims[0].CanonicalPath != "" {
		return OutputAncestryBinding{}, fmt.Errorf("%w: output ancestry root claim", ErrInvalidState)
	}
	hash := sha256.New()
	writeAncestryBindingBytes(hash, []byte("windshare/output-ancestry-binding/v1"))
	writeAncestryBindingBytes(hash, root.Bytes())
	writeAncestryBindingBytes(hash, selection.Bytes())
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(len(claims)))
	_, _ = hash.Write(count[:])
	for index, claim := range claims {
		canonical := claim.CanonicalPath
		if canonical == "" {
			if index != 0 {
				return OutputAncestryBinding{}, fmt.Errorf("%w: output ancestry root ordering", ErrInvalidState)
			}
		} else {
			validated, err := catalog.CanonicalPath(canonical)
			if err != nil || validated != canonical {
				return OutputAncestryBinding{}, fmt.Errorf("%w: output ancestry canonical path", ErrInvalidState)
			}
		}
		if index > 0 && claims[index-1].CanonicalPath >= canonical {
			return OutputAncestryBinding{}, fmt.Errorf("%w: output ancestry order or duplicate", ErrInvalidState)
		}
		if len(claim.IdentityClaim) == 0 || len(claim.IdentityClaim) > MaxAncestryIdentityClaimBytes {
			return OutputAncestryBinding{}, fmt.Errorf("%w: output ancestry identity claim", ErrInvalidState)
		}
		writeAncestryBindingBytes(hash, []byte(canonical))
		writeAncestryBindingBytes(hash, claim.IdentityClaim)
	}
	var digest [OutputAncestryBindingBytes]byte
	copy(digest[:], hash.Sum(nil))
	if digest == ([OutputAncestryBindingBytes]byte{}) {
		return OutputAncestryBinding{}, fmt.Errorf("%w: output ancestry binding is zero", ErrInvalidState)
	}
	return OutputAncestryBinding{digest: digest}, nil
}

func outputAncestryBindingFromBytes(raw []byte) (OutputAncestryBinding, error) {
	if len(raw) != OutputAncestryBindingBytes {
		return OutputAncestryBinding{}, fmt.Errorf("%w: output ancestry binding", ErrInvalidState)
	}
	var digest [OutputAncestryBindingBytes]byte
	copy(digest[:], raw)
	if digest == ([OutputAncestryBindingBytes]byte{}) {
		return OutputAncestryBinding{}, fmt.Errorf("%w: output ancestry binding is zero", ErrInvalidState)
	}
	return OutputAncestryBinding{digest: digest}, nil
}

func writeAncestryBindingBytes(writer io.Writer, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = writer.Write(length[:])
	_, _ = writer.Write(value)
}

func (binding OutputAncestryBinding) Bytes() []byte {
	return append([]byte(nil), binding.digest[:]...)
}
func (binding OutputAncestryBinding) String() string { return hex.EncodeToString(binding.digest[:]) }
func (binding OutputAncestryBinding) IsZero() bool {
	return binding.digest == ([OutputAncestryBindingBytes]byte{})
}
func (binding OutputAncestryBinding) valid() bool { return !binding.IsZero() }

func NewOutputRootBinding(
	certification CertificationID,
	volumeIdentity []byte,
	objectIdentity []byte,
) (OutputRootBinding, error) {
	validatedCertification, certificationErr := NewCertificationID(string(certification))
	if certificationErr != nil || len(volumeIdentity) == 0 || len(volumeIdentity) > MaxRootIdentityClaimBytes ||
		len(objectIdentity) == 0 || len(objectIdentity) > MaxRootIdentityClaimBytes {
		return OutputRootBinding{}, fmt.Errorf("%w: output root identity claims", ErrInvalidState)
	}
	hash := sha256.New()
	writeRootBindingClaim(hash, []byte("windshare/output-root-binding/v1"))
	writeRootBindingClaim(hash, []byte(validatedCertification))
	writeRootBindingClaim(hash, volumeIdentity)
	writeRootBindingClaim(hash, objectIdentity)
	var digest [OutputRootBindingBytes]byte
	copy(digest[:], hash.Sum(nil))
	if digest == ([OutputRootBindingBytes]byte{}) {
		return OutputRootBinding{}, fmt.Errorf("%w: output root binding is zero", ErrInvalidState)
	}
	return OutputRootBinding{certification: validatedCertification, digest: digest}, nil
}

func outputRootBindingFromBytes(
	certification CertificationID,
	raw []byte,
) (OutputRootBinding, error) {
	validatedCertification, certificationErr := NewCertificationID(string(certification))
	if certificationErr != nil || len(raw) != OutputRootBindingBytes {
		return OutputRootBinding{}, fmt.Errorf("%w: output root binding", ErrInvalidState)
	}
	var digest [OutputRootBindingBytes]byte
	copy(digest[:], raw)
	if digest == ([OutputRootBindingBytes]byte{}) {
		return OutputRootBinding{}, fmt.Errorf("%w: output root binding is zero", ErrInvalidState)
	}
	return OutputRootBinding{certification: validatedCertification, digest: digest}, nil
}

func writeRootBindingClaim(writer io.Writer, value []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	_, _ = writer.Write(length[:])
	_, _ = writer.Write(value)
}

func (binding OutputRootBinding) Certification() CertificationID { return binding.certification }
func (binding OutputRootBinding) Bytes() []byte {
	return append([]byte(nil), binding.digest[:]...)
}
func (binding OutputRootBinding) String() string { return hex.EncodeToString(binding.digest[:]) }
func (binding OutputRootBinding) IsZero() bool {
	return binding.certification == "" && binding.digest == ([OutputRootBindingBytes]byte{})
}

func (binding OutputRootBinding) valid() bool {
	_, err := NewCertificationID(string(binding.certification))
	return err == nil && binding.digest != ([OutputRootBindingBytes]byte{})
}

func NewOutputObjectID() (OutputObjectID, error) {
	return GenerateOutputObjectID(rand.Reader)
}

func NewBootstrapNonce() (BootstrapNonce, error) {
	return GenerateBootstrapNonce(rand.Reader)
}

// GenerateOutputObjectID accepts an injected entropy source so allocation
// failures and collisions can be tested. Exclusive namespace creation remains
// the final collision arbiter even when the source is cryptographically secure.
func GenerateOutputObjectID(random io.Reader) (OutputObjectID, error) {
	var id OutputObjectID
	if random == nil {
		return id, fmt.Errorf("%w: output object entropy source is nil", ErrInvalidState)
	}
	if _, err := io.ReadFull(random, id[:]); err != nil {
		return OutputObjectID{}, fmt.Errorf("generate output object ID: %w", err)
	}
	if id.IsZero() {
		return OutputObjectID{}, fmt.Errorf("%w: output object ID is zero", ErrInvalidState)
	}
	return id, nil
}

func GenerateBootstrapNonce(random io.Reader) (BootstrapNonce, error) {
	var nonce BootstrapNonce
	if random == nil {
		return nonce, fmt.Errorf("%w: bootstrap entropy source is nil", ErrInvalidState)
	}
	if _, err := io.ReadFull(random, nonce[:]); err != nil {
		return BootstrapNonce{}, fmt.Errorf("generate bootstrap nonce: %w", err)
	}
	if nonce.IsZero() {
		return BootstrapNonce{}, fmt.Errorf("%w: bootstrap nonce is zero", ErrInvalidState)
	}
	return nonce, nil
}

func OutputObjectIDFromBytes(raw []byte) (OutputObjectID, error) {
	return fixedIDFromBytes[OutputObjectID](raw, OutputObjectIDBytes, "output object")
}

func LocatorDigestFromBytes(raw []byte) (LocatorDigest, error) {
	return fixedIDFromBytes[LocatorDigest](raw, sha256.Size, "locator digest")
}

func BootstrapNonceFromBytes(raw []byte) (BootstrapNonce, error) {
	return fixedIDFromBytes[BootstrapNonce](raw, BootstrapNonceBytes, "bootstrap nonce")
}

func fixedIDFromBytes[T ~[sha256.Size]byte](raw []byte, size int, name string) (T, error) {
	var id T
	if len(raw) != size {
		return id, fmt.Errorf("%w: %s ID has %d bytes", ErrInvalidState, name, len(raw))
	}
	copy(id[:], raw)
	var zero T
	if id == zero {
		return zero, fmt.Errorf("%w: %s ID is zero", ErrInvalidState, name)
	}
	return id, nil
}

func fixedIDBytes[T ~[sha256.Size]byte](id T) []byte  { return append([]byte(nil), id[:]...) }
func fixedIDString[T ~[sha256.Size]byte](id T) string { return hex.EncodeToString(id[:]) }

func (id OutputObjectID) Bytes() []byte { return fixedIDBytes(id) }
func (id LocatorDigest) Bytes() []byte  { return fixedIDBytes(id) }
func (id BootstrapNonce) Bytes() []byte { return fixedIDBytes(id) }

func (id OutputObjectID) String() string { return fixedIDString(id) }
func (id LocatorDigest) String() string  { return fixedIDString(id) }
func (id BootstrapNonce) String() string { return fixedIDString(id) }

func (id OutputObjectID) IsZero() bool { return id == OutputObjectID{} }
func (id LocatorDigest) IsZero() bool  { return id == LocatorDigest{} }
func (id BootstrapNonce) IsZero() bool { return id == BootstrapNonce{} }
