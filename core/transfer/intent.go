package transfer

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"slices"

	"github.com/windshare/windshare/core/catalog"
)

// TransferIntentV1 is the version of the durable output namespace contract.
// The version is encoded before any caller-controlled value so a future
// contract cannot accidentally reuse a v1 namespace with different semantics.
const TransferIntentV1 uint8 = 1

const (
	TransferIntentDigestBytes = sha256.Size
	TransferJobIdentityBytes  = catalog.IdentityBytes
	transferIntentDomain      = "windshare/transfer-intent/v1"
)

var (
	ErrInvalidTransferIntent      = errors.New("transfer intent is invalid")
	ErrTransferIntentNotFinal     = errors.New("transfer intent is not final")
	ErrTransferIntentOutputUnset  = errors.New("transfer intent output target is not confirmed")
	ErrInvalidDirectoryAdmission  = errors.New("directory admission is invalid")
	ErrDirectoryAdmissionMismatch = errors.New("directory admission does not match the requested generation")
)

// TransferIntentDigest is the stable identity used by file-local checkpoints.
// It deliberately excludes TransferJobID and OutputSessionID: those identify a
// run, while this digest identifies the user's confirmed output choice.
type TransferIntentDigest [TransferIntentDigestBytes]byte

func TransferIntentDigestFromBytes(raw []byte) (TransferIntentDigest, error) {
	if len(raw) != TransferIntentDigestBytes {
		return TransferIntentDigest{}, ErrInvalidTransferIntent
	}
	var digest TransferIntentDigest
	copy(digest[:], raw)
	if digest.IsZero() {
		return TransferIntentDigest{}, ErrInvalidTransferIntent
	}
	return digest, nil
}

func (digest TransferIntentDigest) Bytes() []byte { return append([]byte(nil), digest[:]...) }
func (digest TransferIntentDigest) IsZero() bool  { return digest == TransferIntentDigest{} }

// TransferJobID is intentionally separate from the intent digest. A retry of
// the same confirmed intent gets a new run identifier while reusing file-local
// checkpoint records under the intent namespace.
type TransferJobID [TransferJobIdentityBytes]byte

func NewTransferJobID() (TransferJobID, error) {
	var raw [TransferJobIdentityBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return TransferJobID{}, err
	}
	return TransferJobIDFromBytes(raw[:])
}

func TransferJobIDFromBytes(raw []byte) (TransferJobID, error) {
	if len(raw) != TransferJobIdentityBytes {
		return TransferJobID{}, ErrInvalidTransferIntent
	}
	var id TransferJobID
	copy(id[:], raw)
	if id.IsZero() {
		return TransferJobID{}, ErrInvalidTransferIntent
	}
	return id, nil
}

func (id TransferJobID) Bytes() []byte { return append([]byte(nil), id[:]...) }
func (id TransferJobID) IsZero() bool  { return id == TransferJobID{} }

// TransferIntent is immutable after construction. SelectionRules already owns
// its maps and normalized path list; constructors below copy the rule value and
// never expose mutable internals through the final intent.
type TransferIntent struct {
	share   catalog.ShareInstance
	root    catalog.DirectoryID
	rules   SelectionRules
	target  OutputTarget
	backend OutputBackendID
	format  OutputMode
	encoded []byte
	digest  TransferIntentDigest
}

// NewTransferIntent freezes a confirmed output target into a durable intent.
// A target is required here by design: picker cancellation must discard the
// draft and must not create an otherwise ambiguous checkpoint namespace.
func NewTransferIntent(
	share catalog.ShareInstance,
	root catalog.DirectoryID,
	rules SelectionRules,
	target OutputTarget,
	backend OutputBackendID,
	format OutputMode,
) (TransferIntent, error) {
	if share.IsZero() || root.IsZero() || !rules.validSnapshot() || !target.valid() {
		return TransferIntent{}, ErrInvalidTransferIntent
	}
	if _, err := NewOutputBackendID(string(backend)); err != nil {
		return TransferIntent{}, err
	}
	if format < OutputNativeTree || format > OutputZIPStream {
		return TransferIntent{}, ErrInvalidTransferIntent
	}
	request, err := NewCanonicalSelectionRequest(share, root, rules)
	if err != nil {
		return TransferIntent{}, err
	}
	encoded := canonicalTransferIntentBytes(request.Bytes(), target, backend, format)
	digest := sha256.Sum256(encoded)
	return TransferIntent{
		share: share, root: root, rules: rules, target: target,
		backend: backend, format: format, encoded: encoded,
		digest: digest,
	}, nil
}

// NewFilesystemTransferIntent is a convenience for CLI/native callers whose
// output picker returns an absolute filesystem root rather than an opaque
// backend capability. Catalog-relative paths must continue to use NewPathOutputLocator and
// never cross this boundary.
func NewFilesystemTransferIntent(
	share catalog.ShareInstance,
	root catalog.DirectoryID,
	rules SelectionRules,
	rootPath string,
	backend OutputBackendID,
	format OutputMode,
) (TransferIntent, error) {
	target, err := NewFilesystemOutputRootTarget(rootPath)
	if err != nil {
		return TransferIntent{}, err
	}
	return NewTransferIntent(share, root, rules, target, backend, format)
}

// NewPathTransferIntent resolves a caller-provided local path immediately and
// freezes it as a filesystem output root. It must not be confused with
// NewPathOutputLocator, whose relative path is a sender catalog/file locator.
// Resolving here makes a relative CLI argument stable before a job can outlive
// a working-directory change.
func NewPathTransferIntent(
	share catalog.ShareInstance,
	root catalog.DirectoryID,
	rules SelectionRules,
	path string,
	backend OutputBackendID,
	format OutputMode,
) (TransferIntent, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return TransferIntent{}, ErrInvalidTransferIntent
	}
	return NewFilesystemTransferIntent(share, root, rules, absolute, backend, format)
}

func (intent TransferIntent) ShareInstance() catalog.ShareInstance { return intent.share }
func (intent TransferIntent) SyntheticRoot() catalog.DirectoryID   { return intent.root }
func (intent TransferIntent) SelectionRules() SelectionRules       { return intent.rules }
func (intent TransferIntent) SelectionMode() SelectionMode         { return intent.rules.Mode() }
func (intent TransferIntent) OutputTarget() OutputTarget           { return intent.target }
func (intent TransferIntent) BackendID() OutputBackendID           { return intent.backend }
func (intent TransferIntent) Format() OutputMode                   { return intent.format }
func (intent TransferIntent) CanonicalBytes() []byte               { return slices.Clone(intent.encoded) }
func (intent TransferIntent) Digest() TransferIntentDigest         { return intent.digest }
func (intent TransferIntent) IsZero() bool                         { return intent.digest.IsZero() }

// CanonicalBytes is a short spelling used by codec/vector consumers.
func (intent TransferIntent) Bytes() []byte { return intent.CanonicalBytes() }

// TransferIntentDraft holds the pre-picker state. It is intentionally not
// digestible and cannot be passed to an output authority until Freeze succeeds.
type TransferIntentDraft struct {
	share  catalog.ShareInstance
	root   catalog.DirectoryID
	rules  SelectionRules
	target OutputTarget
	have   bool
}

func NewTransferIntentDraft(
	share catalog.ShareInstance,
	root catalog.DirectoryID,
	rules SelectionRules,
) (TransferIntentDraft, error) {
	if share.IsZero() || root.IsZero() || !rules.validSnapshot() {
		return TransferIntentDraft{}, ErrInvalidTransferIntent
	}
	return TransferIntentDraft{share: share, root: root, rules: rules}, nil
}

func (draft TransferIntentDraft) ShareInstance() catalog.ShareInstance { return draft.share }
func (draft TransferIntentDraft) SyntheticRoot() catalog.DirectoryID   { return draft.root }
func (draft TransferIntentDraft) SelectionRules() SelectionRules       { return draft.rules }
func (draft TransferIntentDraft) HasOutputTarget() bool                { return draft.have && !draft.target.IsZero() }

func (draft TransferIntentDraft) ConfirmOutput(target OutputTarget) (TransferIntentDraft, error) {
	if draft.share.IsZero() || draft.root.IsZero() || !draft.rules.validSnapshot() || !target.valid() {
		return TransferIntentDraft{}, ErrInvalidTransferIntent
	}
	draft.target = target
	draft.have = true
	return draft, nil
}

func (draft TransferIntentDraft) ConfirmFilesystemRoot(rootPath string) (TransferIntentDraft, error) {
	target, err := NewFilesystemOutputRootTarget(rootPath)
	if err != nil {
		return TransferIntentDraft{}, err
	}
	return draft.ConfirmOutput(target)
}

func (draft TransferIntentDraft) ConfirmPath(path string) (TransferIntentDraft, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return TransferIntentDraft{}, ErrInvalidTransferIntent
	}
	return draft.ConfirmFilesystemRoot(absolute)
}

func (draft TransferIntentDraft) Freeze(backend OutputBackendID, format OutputMode) (TransferIntent, error) {
	if !draft.HasOutputTarget() {
		return TransferIntent{}, ErrTransferIntentOutputUnset
	}
	return NewTransferIntent(draft.share, draft.root, draft.rules, draft.target, backend, format)
}

// NewTransferIntentFromDraft is kept as a named operation for callers that
// model confirmation as a service boundary rather than a method call.
func NewTransferIntentFromDraft(
	draft TransferIntentDraft,
	backend OutputBackendID,
	format OutputMode,
) (TransferIntent, error) {
	return draft.Freeze(backend, format)
}

func canonicalTransferIntentBytes(
	selection []byte,
	target OutputTarget,
	backend OutputBackendID,
	format OutputMode,
) []byte {
	encoded := make([]byte, 0, len(selection)+128)
	encoded = append(encoded, []byte(transferIntentDomain)...)
	encoded = append(encoded, 0)
	encoded = append(encoded, TransferIntentV1)
	encoded = appendCanonicalField(encoded, selection)
	encoded = appendCanonicalField(encoded, []byte{byte(target.Kind())})
	if target.Kind() == OutputFilesystemRootTarget {
		encoded = appendCanonicalField(encoded, []byte(target.RootPath()))
	} else {
		identity := target.Identity()
		encoded = appendCanonicalField(encoded, identity[:])
	}
	encoded = appendCanonicalField(encoded, []byte(backend))
	encoded = appendCanonicalField(encoded, []byte{byte(format)})
	return encoded
}

// EqualCanonical is useful to adapters that receive an intent through a
// transport-neutral boundary and want to reject a non-canonical re-encoding.
func (intent TransferIntent) EqualCanonical(other TransferIntent) bool {
	// Byte equality alone would bless two identically malformed values. Validate
	// both semantic bindings before exposing equality to an adapter boundary.
	return intent.valid() && other.valid() && bytes.Equal(intent.encoded, other.encoded)
}

func (intent TransferIntent) valid() bool {
	if intent.share.IsZero() || intent.root.IsZero() || !intent.rules.validSnapshot() || !intent.target.valid() ||
		intent.format < OutputNativeTree || intent.format > OutputZIPStream {
		return false
	}
	if _, err := NewOutputBackendID(string(intent.backend)); err != nil {
		return false
	}
	request, err := NewCanonicalSelectionRequest(intent.share, intent.root, intent.rules)
	if err != nil {
		return false
	}
	canonical := canonicalTransferIntentBytes(request.Bytes(), intent.target, intent.backend, intent.format)
	digest := sha256.Sum256(canonical)
	return bytes.Equal(intent.encoded, canonical) && intent.digest == digest
}
