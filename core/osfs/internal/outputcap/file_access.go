package outputcap

import (
	"errors"
	"io"

	"github.com/windshare/windshare/core/catalog"
)

// FileIdentity is the common authority required to prove that two concurrently
// open handles still name the same regular file. It intentionally carries no
// content, durability, or metadata-mutation operation.
type FileIdentity interface {
	Close() error
	Size() (uint64, error)
	SameFile(FileIdentity) (bool, error)
}

// ObservedFile can inspect content and metadata without requesting any native
// right that can durably change the file.
type ObservedFile interface {
	FileIdentity
	io.ReaderAt
	MetadataMatches(uint64, catalog.ModifiedTime) (bool, error)
}

// RecoveryDurabilityFile can make an already-authenticated file durable while
// withholding content read, content write, truncation, and metadata mutation.
type RecoveryDurabilityFile interface {
	FileIdentity
	Sync() error
}

// MutableFile is the active transfer authority. Observation is included because
// transfer settlement must verify the metadata installed by this same handle.
type MutableFile interface {
	ObservedFile
	io.WriterAt
	Sync() error
	SetModifiedTime(catalog.ModifiedTime) error
}

// NativeErrorClass is a bounded provider diagnosis. Raw operating-system errors
// remain available to the provider, while higher layers can freeze this class
// before redacting diagnostic detail.
type NativeErrorClass uint8

const (
	NativeErrorAccessDenied NativeErrorClass = iota + 1
	NativeErrorSharingViolation
	NativeErrorNotFound
	NativeErrorInvalidHandle
	NativeErrorUnsupported
	NativeErrorIO
	NativeErrorUnknown
)

func (class NativeErrorClass) Valid() bool {
	return class >= NativeErrorAccessDenied && class <= NativeErrorUnknown
}

func (class NativeErrorClass) String() string {
	switch class {
	case NativeErrorAccessDenied:
		return "access_denied"
	case NativeErrorSharingViolation:
		return "sharing_violation"
	case NativeErrorNotFound:
		return "not_found"
	case NativeErrorInvalidHandle:
		return "invalid_handle"
	case NativeErrorUnsupported:
		return "unsupported"
	case NativeErrorIO:
		return "io"
	case NativeErrorUnknown:
		return "unknown"
	default:
		return ""
	}
}

type NativeErrorCarrier interface {
	NativeErrorClass() NativeErrorClass
}

func ClassifyNativeError(err error) (NativeErrorClass, bool) {
	var carrier NativeErrorCarrier
	if !errors.As(err, &carrier) {
		return 0, false
	}
	class := carrier.NativeErrorClass()
	return class, class.Valid()
}
