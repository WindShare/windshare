package osfs

import (
	"errors"
	"io"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

var (
	errOutputV3Unsupported = errors.New("osfs: recoverable native output is unsupported")
	errOutputV3Unsafe      = errors.New("osfs: recoverable output namespace is unsafe")
	errOutputV3Collision   = errors.New("osfs: output namespace entry already exists")
	errOutputV3LockBusy    = errors.New("osfs: output namespace lock is already held")
)

// outputV3EntryKind separates safe presence from object authority. Callers use
// it to settle a pre-state final-path collision without opening or following a
// directory, reparse point, or other non-regular object.
type outputV3EntryKind uint8

const (
	outputV3EntryAbsent outputV3EntryKind = iota
	outputV3EntryRegularFile
	outputV3EntryDirectory
	outputV3EntryOther
)

// outputV3Platform is intentionally smaller than a general filesystem API.
// Requiring every mutation through fixed directory and file handles makes it
// difficult for the state machine to accidentally turn a persisted locator
// back into authority.
type outputV3Platform interface {
	Root() outputV3Directory
	AcquirePublicOperationGuard() (outputV3PublicOperationGuard, error)
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

// outputV3PublicOperationGuard retains native placement authority while one
// public-namespace operation runs. Root is borrowed from the guard and remains
// valid only until Close; callers must not close it separately.
type outputV3PublicOperationGuard interface {
	Root() outputV3Directory
	Close() error
}

type outputV3Directory interface {
	Close() error
	Duplicate() (outputV3Directory, error)
	Sync() error
	Names(limit int) ([]string, error)
	NamesWithPrefix(prefix string, matchLimit int) ([]string, error)
	ObserveEntry(name string) (outputV3EntryKind, error)
	ClassifyExactEntry(name string) (outputV3EntryKind, bool, error)
	ValidatePublicEntryName(name string) error
	PrepareIdentityClaim() ([]byte, error)
	IdentityClaim() ([]byte, error)
	OpenEntry(name string) (outputV3EntryRef, error)
	EntryMatches(name string, expected outputV3EntryRef) (bool, error)
	OpenPinnedDirectory(expected outputV3EntryRef, private bool) (outputV3Directory, error)
	RemoveEntry(name string, expected outputV3EntryRef) error
	SameDirectory(outputV3Directory) (bool, error)
	SetModifiedTime(catalog.ModifiedTime) error

	OpenDirectory(name string, private bool) (outputV3Directory, error)
	CreateDirectory(name string, private bool) (outputV3Directory, error)
	InstallDirectoryNoReplace(candidate outputV3Directory, name string) (outputV3Directory, error)
	RemoveDirectory(name string, expected outputV3Directory) error

	CreateFile(name string, private bool, size int64) (outputV3File, error)
	OpenFile(name string, private, writable bool) (outputV3File, error)
	LinkFileNoReplace(source outputV3File, name string) (outputV3File, error)
	ReplacePrivateFile(source outputV3File, name string) error
	RemoveFile(name string, expected outputV3File) error
	AcquireLock(name string, existingOnly bool) (outputV3Lock, bool, error)
}

// outputV3PublicEntryNamesValidator lets a native directory resolve a complete
// admission set in one scan. Windows needs this deeper batch boundary because
// DOS aliases are directory metadata; one full enumeration per selected leaf
// would make a large root and selection amplify each other quadratically.
type outputV3PublicEntryNamesValidator interface {
	ValidatePublicEntryNames(names []string) error
}

type outputV3CreateAuthorityValidator interface {
	ValidateCreateAuthority() error
}

type outputV3MetadataAuthorityValidator interface {
	ValidateMetadataAuthority() error
}

type outputV3EntryRef interface {
	Kind() outputV3EntryKind
	AllocatedSize() (uint64, error)
	Close() error
}

type outputV3File interface {
	io.ReaderAt
	io.WriterAt
	Close() error
	Sync() error
	Truncate(int64) error
	Size() (uint64, error)
	AllocatedSize() (uint64, error)
	SetModifiedTime(catalog.ModifiedTime) error
	MetadataMatches(uint64, catalog.ModifiedTime) (bool, error)
	SameFile(outputV3File) (bool, error)
}

type outputV3Lock interface {
	File() outputV3File
	Close() error
}
