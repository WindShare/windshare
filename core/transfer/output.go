package transfer

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

const (
	OutputSessionIdentityBytes = catalog.IdentityBytes
	OutputObjectIdentityBytes  = 32
	MaxOutputBackendIDBytes    = 128
)

var (
	ErrInvalidOutputBinding = errors.New("transfer output binding is invalid")
	ErrIncompleteOutputFile = errors.New("transfer output file is not complete")
	ErrOutputSessionFatal   = errors.New("transfer output session cannot continue")
)

type OutputSessionID [OutputSessionIdentityBytes]byte
type OutputObjectIdentity [OutputObjectIdentityBytes]byte
type OutputLocatorDigest [sha256.Size]byte
type OutputBackendID string

func OutputSessionIDFromBytes(raw []byte) (OutputSessionID, error) {
	if len(raw) != OutputSessionIdentityBytes {
		return OutputSessionID{}, ErrInvalidOutputBinding
	}
	var id OutputSessionID
	copy(id[:], raw)
	if id.IsZero() {
		return OutputSessionID{}, ErrInvalidOutputBinding
	}
	return id, nil
}

func OutputObjectIdentityFromBytes(raw []byte) (OutputObjectIdentity, error) {
	if len(raw) != OutputObjectIdentityBytes {
		return OutputObjectIdentity{}, ErrInvalidOutputBinding
	}
	var identity OutputObjectIdentity
	copy(identity[:], raw)
	if identity.IsZero() {
		return OutputObjectIdentity{}, ErrInvalidOutputBinding
	}
	return identity, nil
}

func NewOutputBackendID(value string) (OutputBackendID, error) {
	if value == "" || len(value) > MaxOutputBackendIDBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return "", ErrInvalidOutputBinding
	}
	return OutputBackendID(value), nil
}

func (id OutputSessionID) Bytes() []byte { return append([]byte(nil), id[:]...) }
func (id OutputSessionID) IsZero() bool  { return id == OutputSessionID{} }
func (id OutputObjectIdentity) Bytes() []byte {
	return append([]byte(nil), id[:]...)
}
func (id OutputObjectIdentity) IsZero() bool { return id == OutputObjectIdentity{} }

type OutputLocatorKind uint8

const (
	OutputPathLocator OutputLocatorKind = iota + 1
	OutputPersistentHandleLocator
)

type OutputLocator struct {
	kind          OutputLocatorKind
	canonicalPath string
	digest        OutputLocatorDigest
}

func NewPathOutputLocator(path string) (OutputLocator, error) {
	canonical, err := catalog.CanonicalPath(path)
	if err != nil {
		return OutputLocator{}, err
	}
	digest := sha256.Sum256(append([]byte("windshare/output-path/v1\x00"), []byte(canonical)...))
	return OutputLocator{kind: OutputPathLocator, canonicalPath: canonical, digest: digest}, nil
}

func NewPersistentHandleOutputLocator(digest []byte) (OutputLocator, error) {
	if len(digest) != sha256.Size {
		return OutputLocator{}, ErrInvalidOutputBinding
	}
	var owned OutputLocatorDigest
	copy(owned[:], digest)
	if owned == (OutputLocatorDigest{}) {
		return OutputLocator{}, ErrInvalidOutputBinding
	}
	return OutputLocator{kind: OutputPersistentHandleLocator, digest: owned}, nil
}

func (l OutputLocator) Kind() OutputLocatorKind     { return l.kind }
func (l OutputLocator) CanonicalPath() string       { return l.canonicalPath }
func (l OutputLocator) Digest() OutputLocatorDigest { return l.digest }
func (l OutputLocator) IsZero() bool                { return l.kind == 0 }

type OutputFileBinding struct {
	backend        OutputBackendID
	session        OutputSessionID
	share          catalog.ShareInstance
	file           catalog.FileID
	revision       content.FileRevision
	exactSize      uint64
	locator        OutputLocator
	objectIdentity OutputObjectIdentity
}

func NewOutputFileBinding(
	backend OutputBackendID,
	session OutputSessionID,
	descriptor content.FileRevisionDescriptor,
	locator OutputLocator,
	objectIdentity OutputObjectIdentity,
) (OutputFileBinding, error) {
	if _, err := NewOutputBackendID(string(backend)); err != nil || session.IsZero() ||
		descriptor.ShareInstance().IsZero() || descriptor.FileID().IsZero() || descriptor.FileRevision().IsZero() ||
		locator.IsZero() || objectIdentity.IsZero() {
		return OutputFileBinding{}, ErrInvalidOutputBinding
	}
	if locator.kind == OutputPathLocator {
		if canonical, err := catalog.CanonicalPath(locator.canonicalPath); err != nil || canonical != locator.canonicalPath {
			return OutputFileBinding{}, ErrInvalidOutputBinding
		}
	}
	return OutputFileBinding{
		backend: backend, session: session, share: descriptor.ShareInstance(), file: descriptor.FileID(),
		revision: descriptor.FileRevision(), exactSize: descriptor.ExactSize(), locator: locator,
		objectIdentity: objectIdentity,
	}, nil
}

func (b OutputFileBinding) BackendID() OutputBackendID           { return b.backend }
func (b OutputFileBinding) OutputSessionID() OutputSessionID     { return b.session }
func (b OutputFileBinding) ShareInstance() catalog.ShareInstance { return b.share }
func (b OutputFileBinding) FileID() catalog.FileID               { return b.file }
func (b OutputFileBinding) FileRevision() content.FileRevision   { return b.revision }
func (b OutputFileBinding) ExactSize() uint64                    { return b.exactSize }
func (b OutputFileBinding) Locator() OutputLocator               { return b.locator }
func (b OutputFileBinding) ObjectIdentity() OutputObjectIdentity { return b.objectIdentity }

type VerifiedDurableRanges struct {
	binding    OutputFileBinding
	generation uint64
	ranges     content.RangeSet
}

func VerifyDurableRanges(binding OutputFileBinding, generation uint64, ranges content.RangeSet) (VerifiedDurableRanges, error) {
	if binding.backend == "" || binding.session.IsZero() || binding.objectIdentity.IsZero() {
		return VerifiedDurableRanges{}, ErrInvalidOutputBinding
	}
	validated, err := content.NewRangeSet(ranges.Ranges())
	if err != nil {
		return VerifiedDurableRanges{}, err
	}
	for _, current := range validated.Ranges() {
		if current.End > binding.exactSize {
			return VerifiedDurableRanges{}, ErrInvalidOutputBinding
		}
	}
	return VerifiedDurableRanges{binding: binding, generation: generation, ranges: validated}, nil
}

func (r VerifiedDurableRanges) Binding() OutputFileBinding { return r.binding }
func (r VerifiedDurableRanges) Generation() uint64         { return r.generation }
func (r VerifiedDurableRanges) Ranges() content.RangeSet {
	clone, _ := content.NewRangeSet(r.ranges.Ranges())
	return clone
}

func MergeRanges(sets ...content.RangeSet) (content.RangeSet, error) {
	all := make([]content.Range, 0)
	for _, set := range sets {
		all = append(all, set.Ranges()...)
	}
	if len(all) == 0 {
		return content.NewRangeSet(nil)
	}
	slices.SortFunc(all, func(left, right content.Range) int {
		if left.Offset < right.Offset {
			return -1
		}
		if left.Offset > right.Offset {
			return 1
		}
		if left.End < right.End {
			return -1
		}
		if left.End > right.End {
			return 1
		}
		return 0
	})
	merged := make([]content.Range, 0, len(all))
	for _, current := range all {
		if current.Offset >= current.End {
			return content.RangeSet{}, content.ErrNonCanonicalRange
		}
		if len(merged) == 0 || current.Offset > merged[len(merged)-1].End {
			merged = append(merged, current)
			continue
		}
		if current.End > merged[len(merged)-1].End {
			merged[len(merged)-1].End = current.End
		}
	}
	return content.NewRangeSet(merged)
}

func MissingRanges(exactSize uint64, durable content.RangeSet) (content.RangeSet, error) {
	if exactSize > catalog.MaxFileSize {
		return content.RangeSet{}, ErrInvalidOutputBinding
	}
	if exactSize == 0 {
		return content.NewRangeSet(nil)
	}
	missing := make([]content.Range, 0, durable.Len()+1)
	cursor := uint64(0)
	for _, current := range durable.Ranges() {
		if current.End > exactSize {
			return content.RangeSet{}, ErrInvalidOutputBinding
		}
		if current.Offset > cursor {
			missing = append(missing, content.Range{Offset: cursor, End: current.Offset})
		}
		cursor = current.End
	}
	if cursor < exactSize {
		missing = append(missing, content.Range{Offset: cursor, End: exactSize})
	}
	return content.NewRangeSet(missing)
}

func RangesCoverFile(exactSize uint64, ranges content.RangeSet) bool {
	if exactSize == 0 {
		return ranges.IsEmpty()
	}
	items := ranges.Ranges()
	return len(items) == 1 && items[0] == (content.Range{Offset: 0, End: exactSize})
}

type DurabilityLevel uint8

const (
	DurabilityNone DurabilityLevel = iota
	DurabilityProcessRestart
	DurabilityPowerLoss
)

type OutputMode uint8

const (
	OutputNativeTree OutputMode = iota + 1
	OutputSingleFileStream
	OutputZIPStream
)

type ArchiveFailureBoundary uint8

const (
	ArchiveFailureNotApplicable ArchiveFailureBoundary = iota
	ArchiveFailureAtMemberStart
)

type OutputCapabilities struct {
	Durability           DurabilityLevel
	Mode                 OutputMode
	RandomWrite          bool
	FileFailureIsolation bool
	ModifiedTime         bool
	ArchiveBoundary      ArchiveFailureBoundary
}

func NewOutputCapabilities(capabilities OutputCapabilities) (OutputCapabilities, error) {
	if capabilities.Durability > DurabilityPowerLoss || capabilities.Mode < OutputNativeTree || capabilities.Mode > OutputZIPStream {
		return OutputCapabilities{}, ErrInvalidOutputBinding
	}
	if capabilities.Mode == OutputZIPStream {
		if capabilities.RandomWrite || capabilities.FileFailureIsolation || capabilities.ArchiveBoundary != ArchiveFailureAtMemberStart {
			return OutputCapabilities{}, ErrInvalidOutputBinding
		}
	} else if capabilities.ArchiveBoundary != ArchiveFailureNotApplicable {
		return OutputCapabilities{}, ErrInvalidOutputBinding
	}
	if capabilities.Mode != OutputNativeTree && capabilities.Durability != DurabilityNone {
		return OutputCapabilities{}, ErrInvalidOutputBinding
	}
	if capabilities.Mode == OutputSingleFileStream && (capabilities.RandomWrite || capabilities.FileFailureIsolation) {
		return OutputCapabilities{}, ErrInvalidOutputBinding
	}
	return capabilities, nil
}

type OutputDirectory struct {
	Path         string
	ModifiedTime catalog.ModifiedTime
}

type OutputFile struct {
	Path         string
	ExpectedSize uint64
	Descriptor   content.FileRevisionDescriptor
}

type FileAbortDisposition uint8

const (
	FileAbortIsolated FileAbortDisposition = iota + 1
	FileAbortSkippedBeforeStart
	FileAbortRequiresJobAbort
)

type FileTransaction interface {
	Binding() OutputFileBinding
	WriteRange(context.Context, uint64, []byte) error
	Checkpoint(context.Context) (VerifiedDurableRanges, error)
	Commit(context.Context) error
	Abort(context.Context, error) (FileAbortDisposition, error)
}

type OutputSession interface {
	BackendID() OutputBackendID
	SessionID() OutputSessionID
	Capabilities() OutputCapabilities
	EnsureDirectory(context.Context, OutputDirectory) error
	FinalizeDirectory(context.Context, OutputDirectory) error
	BeginFile(context.Context, OutputFile) (FileTransaction, VerifiedDurableRanges, error)
	FinishJob(context.Context, JobOutcome) error
	AbortJob(context.Context, error) error
}

type OutputSessionError struct {
	cause error
	fatal bool
}

func NewOutputSessionError(cause error, fatal bool) error {
	if cause == nil {
		cause = errors.New("output operation failed")
	}
	return &OutputSessionError{cause: cause, fatal: fatal}
}

func (e *OutputSessionError) Error() string { return fmt.Sprintf("output session: %v", e.cause) }
func (e *OutputSessionError) Unwrap() error { return e.cause }
func (e *OutputSessionError) RequiresJobAbort() bool {
	return e.fatal
}

func outputFailureRequiresJobAbort(err error, capabilities OutputCapabilities) bool {
	if outputFailureExplicitlyRequiresJobAbort(err) {
		return true
	}
	return !capabilities.FileFailureIsolation
}

type jobAbortRequirement interface {
	error
	RequiresJobAbort() bool
}

func outputFailureExplicitlyRequiresJobAbort(err error) bool {
	if scoped, ok := errors.AsType[jobAbortRequirement](err); ok {
		return scoped.RequiresJobAbort()
	}
	return false
}
