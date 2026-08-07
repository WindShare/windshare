package transfer

import (
	"crypto/sha256"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// OutputTargetKind identifies the user-owned namespace selected by the picker.
// A catalog OutputLocator is deliberately a different type: catalog paths are
// relative sender names, whereas an output target is a receiver-side root or
// backend-owned opaque capability.
type OutputTargetKind uint8

const (
	OutputFilesystemRootTarget OutputTargetKind = iota + 1
	OutputOpaqueTarget
)

const outputTargetIdentityDomain = "windshare/output-root/v1\x00"

const OutputRootIdentityBytes = sha256.Size

// OutputRootIdentity is the stable identity of a receiver-owned output root.
// It is not an authority by itself; the output backend must still revalidate
// ownership and confinement when OpenOutput is called.
type OutputRootIdentity [sha256.Size]byte

func (identity OutputRootIdentity) Bytes() []byte { return append([]byte(nil), identity[:]...) }
func (identity OutputRootIdentity) IsZero() bool  { return identity == (OutputRootIdentity{}) }

// OutputTarget is an immutable, picker-confirmed destination. Filesystem roots
// retain their canonical absolute path for the native authority, while an
// opaque backend capability carries only its identity.
type OutputTarget struct {
	kind     OutputTargetKind
	rootPath string
	identity OutputRootIdentity
}

// NewFilesystemOutputRootTarget accepts only an absolute path. Requiring the
// caller to resolve relative input at the UI/CLI boundary prevents the same
// intent from silently referring to different roots when the process cwd
// changes; the authority still performs its own root ownership checks.
func NewFilesystemOutputRootTarget(rootPath string) (OutputTarget, error) {
	canonical, err := canonicalOutputRootPath(rootPath)
	if err != nil {
		return OutputTarget{}, err
	}
	identity := sha256.Sum256(append([]byte(outputTargetIdentityDomain), []byte(canonical)...))
	return OutputTarget{kind: OutputFilesystemRootTarget, rootPath: canonical, identity: identity}, nil
}

// NewOpaqueOutputTarget creates a target for a backend-owned capability. The
// bytes may identify an FSA/OPFS handle, stream sink, or output object; transfer
// never interprets them and only the issuing backend can authenticate them.
func NewOpaqueOutputTarget(raw []byte) (OutputTarget, error) {
	if len(raw) != sha256.Size {
		return OutputTarget{}, ErrInvalidOutputBinding
	}
	var identity OutputRootIdentity
	copy(identity[:], raw)
	if identity == (OutputRootIdentity{}) {
		return OutputTarget{}, ErrInvalidOutputBinding
	}
	return OutputTarget{kind: OutputOpaqueTarget, identity: identity}, nil
}

func canonicalOutputRootPath(rootPath string) (string, error) {
	if rootPath == "" || !utf8.ValidString(rootPath) || strings.ContainsRune(rootPath, '\x00') || !filepath.IsAbs(rootPath) {
		return "", ErrInvalidOutputBinding
	}
	clean := filepath.Clean(rootPath)
	if clean == "." || !filepath.IsAbs(clean) {
		return "", ErrInvalidOutputBinding
	}
	return clean, nil
}

func (target OutputTarget) Kind() OutputTargetKind { return target.kind }
func (target OutputTarget) RootPath() string       { return target.rootPath }
func (target OutputTarget) Identity() OutputRootIdentity {
	return target.identity
}
func (target OutputTarget) IsZero() bool {
	return target.kind == 0 || target.identity == (OutputRootIdentity{})
}

func (target OutputTarget) Equal(other OutputTarget) bool {
	return target.kind == other.kind && target.rootPath == other.rootPath && target.identity == other.identity
}

func (target OutputTarget) valid() bool {
	switch target.kind {
	case OutputFilesystemRootTarget:
		canonical, err := canonicalOutputRootPath(target.rootPath)
		return err == nil && canonical == target.rootPath && target.identity != (OutputRootIdentity{})
	case OutputOpaqueTarget:
		return target.rootPath == "" && target.identity != (OutputRootIdentity{})
	default:
		return false
	}
}
