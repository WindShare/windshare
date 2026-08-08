package transfer

import (
	"crypto/sha256"
	"errors"
	"path/filepath"
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

// NativeFilesystemOutputBackendID is the semantic backend identity carried by
// TransferIntent. Journal/schema revisions belong to the physical state layout,
// so they must not silently change the user's confirmed output contract.
const NativeFilesystemOutputBackendID OutputBackendID = "windshare/native-output"

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
	// OutputObjectLocator identifies a backend-owned output object before or
	// alongside its concrete object identity (for example a stream sink).
	OutputObjectLocator
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

func NewOutputObjectLocator(digest []byte) (OutputLocator, error) {
	if len(digest) != sha256.Size {
		return OutputLocator{}, ErrInvalidOutputBinding
	}
	var owned OutputLocatorDigest
	copy(owned[:], digest)
	if owned == (OutputLocatorDigest{}) {
		return OutputLocator{}, ErrInvalidOutputBinding
	}
	return OutputLocator{kind: OutputObjectLocator, digest: owned}, nil
}

func (l OutputLocator) Kind() OutputLocatorKind     { return l.kind }
func (l OutputLocator) CanonicalPath() string       { return l.canonicalPath }
func (l OutputLocator) Digest() OutputLocatorDigest { return l.digest }
func (l OutputLocator) IsZero() bool                { return l.kind == 0 }

// OutputFileTarget is the complete authority requested from an output backend.
// It exists before WindShare owns a filesystem object, which keeps a pre-object
// collision from fabricating an OutputObjectIdentity merely to identify itself.
type OutputFileTarget struct {
	backend    OutputBackendID
	session    OutputSessionID
	descriptor content.FileRevisionDescriptor
	locator    OutputLocator
}

func NewOutputFileTarget(
	backend OutputBackendID,
	session OutputSessionID,
	descriptor content.FileRevisionDescriptor,
	locator OutputLocator,
) (OutputFileTarget, error) {
	target := OutputFileTarget{
		backend: backend, session: session, descriptor: descriptor, locator: locator,
	}
	if !target.valid() {
		return OutputFileTarget{}, ErrInvalidOutputBinding
	}
	return target, nil
}

func (target OutputFileTarget) BackendID() OutputBackendID { return target.backend }
func (target OutputFileTarget) OutputSessionID() OutputSessionID {
	return target.session
}
func (target OutputFileTarget) Descriptor() content.FileRevisionDescriptor {
	return target.descriptor
}
func (target OutputFileTarget) ShareInstance() catalog.ShareInstance {
	return target.descriptor.ShareInstance()
}
func (target OutputFileTarget) FileID() catalog.FileID { return target.descriptor.FileID() }
func (target OutputFileTarget) FileRevision() content.FileRevision {
	return target.descriptor.FileRevision()
}
func (target OutputFileTarget) ExactSize() uint64      { return target.descriptor.ExactSize() }
func (target OutputFileTarget) Locator() OutputLocator { return target.locator }

func (target OutputFileTarget) valid() bool {
	if _, err := NewOutputBackendID(string(target.backend)); err != nil || target.session.IsZero() ||
		target.descriptor.ShareInstance().IsZero() || target.descriptor.FileID().IsZero() ||
		target.descriptor.FileRevision().IsZero() || target.locator.IsZero() {
		return false
	}
	if target.locator.kind == OutputPathLocator {
		canonical, err := catalog.CanonicalPath(target.locator.canonicalPath)
		return err == nil && canonical == target.locator.canonicalPath
	}
	return target.locator.kind == OutputObjectLocator &&
		target.locator.digest != (OutputLocatorDigest{})
}

// OutputFileBinding adds ownership of one concrete output object to the
// immutable requested target. Durable ranges and transaction settlements use
// this stronger identity; immediate collisions use only OutputFileTarget.
type OutputFileBinding struct {
	target         OutputFileTarget
	objectIdentity OutputObjectIdentity
}

func NewOutputFileBinding(
	backend OutputBackendID,
	session OutputSessionID,
	descriptor content.FileRevisionDescriptor,
	locator OutputLocator,
	objectIdentity OutputObjectIdentity,
) (OutputFileBinding, error) {
	target, err := NewOutputFileTarget(backend, session, descriptor, locator)
	if err != nil {
		return OutputFileBinding{}, err
	}
	return BindOutputFileTarget(target, objectIdentity)
}

func BindOutputFileTarget(
	target OutputFileTarget,
	objectIdentity OutputObjectIdentity,
) (OutputFileBinding, error) {
	if !target.valid() || objectIdentity.IsZero() {
		return OutputFileBinding{}, ErrInvalidOutputBinding
	}
	return OutputFileBinding{target: target, objectIdentity: objectIdentity}, nil
}

func (binding OutputFileBinding) Target() OutputFileTarget { return binding.target }
func (binding OutputFileBinding) BackendID() OutputBackendID {
	return binding.target.BackendID()
}
func (binding OutputFileBinding) OutputSessionID() OutputSessionID {
	return binding.target.OutputSessionID()
}
func (binding OutputFileBinding) Descriptor() content.FileRevisionDescriptor {
	return binding.target.Descriptor()
}
func (binding OutputFileBinding) ShareInstance() catalog.ShareInstance {
	return binding.target.ShareInstance()
}
func (binding OutputFileBinding) FileID() catalog.FileID { return binding.target.FileID() }
func (binding OutputFileBinding) FileRevision() content.FileRevision {
	return binding.target.FileRevision()
}
func (binding OutputFileBinding) ExactSize() uint64      { return binding.target.ExactSize() }
func (binding OutputFileBinding) Locator() OutputLocator { return binding.target.Locator() }
func (binding OutputFileBinding) ObjectIdentity() OutputObjectIdentity {
	return binding.objectIdentity
}

func (binding OutputFileBinding) valid() bool {
	return binding.target.valid() && !binding.objectIdentity.IsZero()
}

type VerifiedDurableRanges struct {
	binding              OutputFileBinding
	checkpointGeneration CheckpointGeneration
	ranges               content.RangeSet
}

type CheckpointGeneration uint64

func VerifyDurableRanges(
	binding OutputFileBinding,
	checkpointGeneration CheckpointGeneration,
	ranges content.RangeSet,
) (VerifiedDurableRanges, error) {
	if !binding.valid() {
		return VerifiedDurableRanges{}, ErrInvalidOutputBinding
	}
	validated, err := content.NewRangeSet(ranges.Ranges())
	if err != nil {
		return VerifiedDurableRanges{}, err
	}
	for _, current := range validated.Ranges() {
		if current.End > binding.ExactSize() {
			return VerifiedDurableRanges{}, ErrInvalidOutputBinding
		}
	}
	return VerifiedDurableRanges{
		binding: binding, checkpointGeneration: checkpointGeneration, ranges: validated,
	}, nil
}

func (r VerifiedDurableRanges) Binding() OutputFileBinding { return r.binding }
func (r VerifiedDurableRanges) CheckpointGeneration() CheckpointGeneration {
	return r.checkpointGeneration
}
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

func validateOutputSession(intent TransferIntent, output OutputSession) error {
	if output == nil {
		return outputContractFault(nil)
	}
	backend, err := NewOutputBackendID(string(output.BackendID()))
	capabilities := output.Capabilities()
	if err != nil || backend != intent.BackendID() || output.SessionID().IsZero() ||
		capabilities.Mode != intent.Format() {
		return outputContractFault(nil)
	}
	if _, err := NewOutputCapabilities(capabilities); err != nil {
		return outputContractFault(err)
	}
	return nil
}

type OutputDirectory struct {
	// DirectoryID and Generation are authenticated catalog identity, not a
	// filesystem inode. They let an output backend bind each mutation to the
	// exact committed generation that made the directory visible.
	DirectoryID     catalog.DirectoryID
	Generation      catalog.DirectoryGeneration
	ParentAdmission DirectoryAdmission
	Path            string
	ModifiedTime    catalog.ModifiedTime
}

type OutputFile struct {
	Path            string
	ExpectedSize    uint64
	Descriptor      content.FileRevisionDescriptor
	Target          OutputFileTarget
	ParentAdmission DirectoryAdmission
}

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

func validateOpenedFile(share catalog.ShareInstance, entry catalog.Entry, opened OpenedRevision) error {
	file, isFile := entry.FileID()
	if !isFile {
		return ErrRevisionIdentity
	}
	return validateOpenedPlanFile(share, file, entry.ExpectedSize(), entry.ModifiedTime(), opened)
}

func validateOpenedPlanFile(
	share catalog.ShareInstance,
	file catalog.FileID,
	expectedSize uint64,
	modified catalog.ModifiedTime,
	opened OpenedRevision,
) error {
	descriptor := opened.Descriptor
	if file.IsZero() || opened.LeaseID.IsZero() || descriptor.ShareInstance() != share ||
		descriptor.FileID() != file || descriptor.FileRevision().IsZero() ||
		descriptor.ExactSize() != expectedSize {
		return ErrRevisionIdentity
	}
	if modified.Present() && descriptor.ModifiedTime() != modified {
		return ErrRevisionIdentity
	}
	return nil
}

func validateOutputTransaction(
	target OutputFileTarget,
	transaction FileTransaction,
	durable VerifiedDurableRanges,
) error {
	if transaction == nil {
		return outputContractFault(nil)
	}
	binding := transaction.Binding()
	if validateOutputFileBinding(target, binding) != nil || durable.Binding() != binding {
		return outputContractFault(nil)
	}
	return nil
}

func validateImmediateFileSettlement(
	target OutputFileTarget,
	settlement FileSettlement,
) error {
	// matchesTarget already proves the settlement's kind-specific binding and
	// quarantine invariants. Immediate pause remains forbidden because it would
	// bypass the transaction that owns resumable progress.
	if !settlement.matchesTarget(target) || settlement.Kind() == FilePaused {
		return ErrOutputContract
	}
	return nil
}

func validateOutputFileBinding(
	target OutputFileTarget,
	binding OutputFileBinding,
) error {
	if !target.valid() || !binding.valid() || binding.Target() != target {
		return ErrOutputContract
	}
	return nil
}
