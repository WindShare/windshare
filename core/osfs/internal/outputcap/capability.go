// Package outputcap defines the native capabilities consumed by the resumable
// output state machine. Keeping these contracts below osfs lets platform
// implementations depend on capability semantics without depending on the
// state-machine package that orchestrates them.
package outputcap

import (
	"errors"
	"io"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

var (
	// ErrRecoverableOutputUnsupported reports that the native filesystem cannot
	// satisfy the certified resumable-output contract.
	ErrRecoverableOutputUnsupported = errors.New("osfs: recoverable native output is unsupported")
	// ErrUnsafeNamespace reports that the output namespace cannot safely retain
	// resumable state or receive file content.
	ErrUnsafeNamespace = errors.New("osfs: recoverable output namespace is unsafe")
	// ErrNamespaceCollision reports that a no-replace namespace mutation found an
	// existing entry.
	ErrNamespaceCollision = errors.New("osfs: output namespace entry already exists")
	// ErrNamespaceLockBusy reports that another operation owns the namespace lock.
	ErrNamespaceLockBusy = errors.New("osfs: output namespace lock is already held")
)

// ErrFixedLinkSourceChanged is emitted only before attempting a no-replace
// mutation. That cut lets recovery distinguish an invalidated source witness
// from an unknown final-publication outcome.
var ErrFixedLinkSourceChanged = errors.New("osfs: fixed output link source changed")

// EntryKind separates safe presence from object authority. Callers use it to
// settle a pre-state final-path collision without opening or following a
// directory, reparse point, or other non-regular object.
type EntryKind uint8

const (
	// EntryAbsent means the named directory entry does not exist.
	EntryAbsent EntryKind = iota
	// EntryRegularFile means the named entry is a regular file.
	EntryRegularFile
	// EntryDirectory means the named entry is a directory.
	EntryDirectory
	// EntryOther means the named entry exists but is neither a regular file nor a directory.
	EntryOther
)

// PersistentDirectoryIdentity is an opaque, restart-revalidatable directory
// claim. It identifies an enrolled directory but never grants authority to it;
// authority always comes from a live Directory handle.
type PersistentDirectoryIdentity struct {
	encoded string
}

// NewPersistentDirectoryIdentity copies a native identity encoding into an
// immutable value. Native encodings stay behind this boundary so callers cannot
// mutate a previously admitted identity through a shared byte slice.
func NewPersistentDirectoryIdentity(encoded []byte) PersistentDirectoryIdentity {
	return PersistentDirectoryIdentity{encoded: string(encoded)}
}

// Bytes returns an independent copy suitable for persistent state encoding.
func (identity PersistentDirectoryIdentity) Bytes() []byte {
	return []byte(identity.encoded)
}

// Equal compares the complete opaque native encodings.
func (identity PersistentDirectoryIdentity) Equal(other PersistentDirectoryIdentity) bool {
	return identity.encoded == other.encoded
}

// IsZero reports whether no native identity was supplied.
func (identity PersistentDirectoryIdentity) IsZero() bool {
	return identity.encoded == ""
}

// Platform is intentionally smaller than a general filesystem API. Requiring
// every mutation through fixed directory and file handles makes it difficult
// for the state machine to accidentally turn a persisted locator into authority.
type Platform interface {
	Root() Directory
	AcquirePublicOperationGuard() (PublicOperationGuard, error)
	RootBinding() (resumestate.OutputRootBinding, error)
	Certification() resumestate.CertificationID
	Durability() transfer.DurabilityLevel
	ProbeRecoverableFeatures() error
	ValidateSelectionMetadata(transfer.OutputSelection) error
	ValidateModifiedTime(catalog.ModifiedTime) error
	CanonicalLocatorKey(string) (string, error)
	CanonicalComponentKey(string) (string, error)
	Close() error
}

// PublicOperationGuard retains native placement authority while one public
// namespace operation runs. Root is borrowed from the guard and remains valid
// only until Close; callers must not close it separately.
type PublicOperationGuard interface {
	Root() Directory
	Close() error
}

// Directory is a live, fixed-handle directory capability. Identity methods on
// this interface deliberately return persistent claims separately: the live
// handle authorizes current operations, while the claim can only be revalidated
// after restart.
type Directory interface {
	Close() error
	Duplicate() (Directory, error)
	Sync() error
	Names(limit int) ([]string, error)
	NamesWithPrefix(prefix string, matchLimit int) ([]string, error)
	ObserveEntry(name string) (EntryKind, error)
	ClassifyExactEntry(name string) (EntryKind, bool, error)
	ValidatePublicEntryName(name string) error
	PrepareIdentityClaim() (PersistentDirectoryIdentity, error)
	IdentityClaim() (PersistentDirectoryIdentity, error)
	OpenEntry(name string) (CurrentEntryReference, error)
	EntryMatches(name string, expected CurrentEntryReference) (bool, error)
	OpenPinnedDirectory(expected CurrentEntryReference, private bool) (Directory, error)
	RemoveEntry(name string, expected CurrentEntryReference) error
	SameDirectory(Directory) (bool, error)
	SetModifiedTime(catalog.ModifiedTime) error

	OpenDirectory(name string, private bool) (Directory, error)
	CreateDirectory(name string, private bool) (Directory, error)
	InstallDirectoryNoReplace(candidate Directory, name string) (Directory, error)
	RemoveDirectory(name string, expected Directory) error

	CreateFile(name string, private bool, size int64) (File, error)
	OpenFile(name string, private, writable bool) (File, error)
	LinkFileNoReplace(source File, name string) (File, error)
	ReplacePrivateFile(source File, name string) error
	RemoveFile(name string, expected File) error
	AcquireLock(name string, existingOnly bool) (Lock, bool, error)
}

// PublicEntryNamesValidator lets a native directory resolve a complete
// admission set in one scan. Windows needs this deeper batch boundary because
// DOS aliases are directory metadata; one full enumeration per selected leaf
// would make a large root and selection amplify each other quadratically.
type PublicEntryNamesValidator interface {
	ValidatePublicEntryNames(names []string) error
}

// CreateAuthorityValidator verifies that a live directory capability still
// carries the authority required to create a child.
type CreateAuthorityValidator interface {
	ValidateCreateAuthority() error
}

// MetadataAuthorityValidator verifies that a live directory capability still
// carries the authority required to change metadata.
type MetadataAuthorityValidator interface {
	ValidateMetadataAuthority() error
}

// CurrentEntryReference is a live-handle identity witness for one opened entry.
// It is suitable for same-object checks only while retained and is never a
// restart-stable locator.
type CurrentEntryReference interface {
	Kind() EntryKind
	AllocatedSize() (uint64, error)
	Close() error
}

// File is a live, fixed-handle file capability. Retaining one can witness a
// staged or anchored file across namespace mutations without trusting a path.
type File interface {
	io.ReaderAt
	io.WriterAt
	Close() error
	Sync() error
	Truncate(int64) error
	Size() (uint64, error)
	AllocatedSize() (uint64, error)
	SetModifiedTime(catalog.ModifiedTime) error
	MetadataMatches(uint64, catalog.ModifiedTime) (bool, error)
	SameFile(File) (bool, error)
}

// Lock retains both the namespace lock and the fixed file capability that
// carries its data.
type Lock interface {
	File() File
	Close() error
}
