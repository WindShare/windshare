package transfer

import (
	"context"
	"crypto/sha256"
	"errors"
	"slices"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

// HistoricalDelivery belongs to a logical file in one operation. It conveys no
// authority over the public path or over any bytes that happen to exist there.
type HistoricalDelivery struct {
	OperationID         receivecontract.OperationID
	FileID              catalog.FileID
	FileRevision        content.FileRevision
	MaterializationPath string
	Size                uint64
}

func (delivery HistoricalDelivery) Valid() bool {
	canonical, err := catalog.CanonicalPath(delivery.MaterializationPath)
	return err == nil && canonical == delivery.MaterializationPath && canonical != "" &&
		!delivery.OperationID.IsZero() && !delivery.FileID.IsZero() && !delivery.FileRevision.IsZero() && delivery.Size <= catalog.MaxFileSize
}

// DeliveryHistory is queried before source revision acquisition. Revision drift
// may block partial reuse, but cannot revoke a delivery the task already made.
type DeliveryHistory interface {
	LookupDelivery(context.Context, catalog.FileID, string) (HistoricalDelivery, bool, error)
}

const (
	OutputSessionIdentityBytes = catalog.IdentityBytes
	OwnedObjectIdentityBytes   = 32
)

var (
	ErrInvalidOutputBinding          = errors.New("transfer output binding is invalid")
	ErrIncompleteMaterializationFile = errors.New("transfer output file is not complete")
	ErrDirectTreeSessionFatal        = errors.New("transfer output session cannot continue")
)

type OutputSessionID [OutputSessionIdentityBytes]byte
type OwnedObjectID [OwnedObjectIdentityBytes]byte
type MaterializationLocatorDigest [sha256.Size]byte

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

func OwnedObjectIDFromBytes(raw []byte) (OwnedObjectID, error) {
	if len(raw) != OwnedObjectIdentityBytes {
		return OwnedObjectID{}, ErrInvalidOutputBinding
	}
	var identity OwnedObjectID
	copy(identity[:], raw)
	if identity.IsZero() {
		return OwnedObjectID{}, ErrInvalidOutputBinding
	}
	return identity, nil
}

func (id OutputSessionID) Bytes() []byte { return append([]byte(nil), id[:]...) }
func (id OutputSessionID) IsZero() bool  { return id == OutputSessionID{} }
func (id OwnedObjectID) Bytes() []byte {
	return append([]byte(nil), id[:]...)
}
func (id OwnedObjectID) IsZero() bool { return id == OwnedObjectID{} }

type MaterializationLocatorKind uint8

const (
	MaterializationPathLocator MaterializationLocatorKind = iota + 1
	// MaterializationObjectLocator identifies a backend-owned output object before or
	// alongside its concrete object identity (for example a stream sink).
	MaterializationObjectLocator
)

type MaterializationLocator struct {
	kind          MaterializationLocatorKind
	canonicalPath string
	digest        MaterializationLocatorDigest
}

func NewPathMaterializationLocator(path string) (MaterializationLocator, error) {
	canonical, err := catalog.CanonicalPath(path)
	if err != nil {
		return MaterializationLocator{}, err
	}
	digest := sha256.Sum256(append([]byte("windshare/output-path/v1\x00"), []byte(canonical)...))
	return MaterializationLocator{kind: MaterializationPathLocator, canonicalPath: canonical, digest: digest}, nil
}

func NewMaterializationObjectLocator(digest []byte) (MaterializationLocator, error) {
	if len(digest) != sha256.Size {
		return MaterializationLocator{}, ErrInvalidOutputBinding
	}
	var owned MaterializationLocatorDigest
	copy(owned[:], digest)
	if owned == (MaterializationLocatorDigest{}) {
		return MaterializationLocator{}, ErrInvalidOutputBinding
	}
	return MaterializationLocator{kind: MaterializationObjectLocator, digest: owned}, nil
}

func (l MaterializationLocator) Kind() MaterializationLocatorKind     { return l.kind }
func (l MaterializationLocator) CanonicalPath() string                { return l.canonicalPath }
func (l MaterializationLocator) Digest() MaterializationLocatorDigest { return l.digest }
func (l MaterializationLocator) IsZero() bool                         { return l.kind == 0 }

// FileMaterializationTarget binds one requested file to an already-opened
// DirectTree session. It exists before WindShare owns a filesystem object, so a
// pre-object collision cannot fabricate an OwnedObjectID merely to identify itself.
type FileMaterializationTarget struct {
	session    OutputSessionID
	descriptor content.FileRevisionDescriptor
	locator    MaterializationLocator
}

func NewFileMaterializationTarget(
	session OutputSessionID,
	descriptor content.FileRevisionDescriptor,
	locator MaterializationLocator,
) (FileMaterializationTarget, error) {
	target := FileMaterializationTarget{
		session: session, descriptor: descriptor, locator: locator,
	}
	if !target.valid() {
		return FileMaterializationTarget{}, ErrInvalidOutputBinding
	}
	return target, nil
}

func (target FileMaterializationTarget) OutputSessionID() OutputSessionID {
	return target.session
}
func (target FileMaterializationTarget) Descriptor() content.FileRevisionDescriptor {
	return target.descriptor
}
func (target FileMaterializationTarget) ShareInstance() catalog.ShareInstance {
	return target.descriptor.ShareInstance()
}
func (target FileMaterializationTarget) FileID() catalog.FileID { return target.descriptor.FileID() }
func (target FileMaterializationTarget) FileRevision() content.FileRevision {
	return target.descriptor.FileRevision()
}
func (target FileMaterializationTarget) ExactSize() uint64               { return target.descriptor.ExactSize() }
func (target FileMaterializationTarget) Locator() MaterializationLocator { return target.locator }

func (target FileMaterializationTarget) valid() bool {
	if target.session.IsZero() ||
		target.descriptor.ShareInstance().IsZero() || target.descriptor.FileID().IsZero() ||
		target.descriptor.FileRevision().IsZero() || target.locator.IsZero() {
		return false
	}
	if target.locator.kind == MaterializationPathLocator {
		canonical, err := catalog.CanonicalPath(target.locator.canonicalPath)
		return err == nil && canonical == target.locator.canonicalPath
	}
	return target.locator.kind == MaterializationObjectLocator &&
		target.locator.digest != (MaterializationLocatorDigest{})
}

// MaterializedFileBinding adds ownership of one concrete output object to the
// immutable requested target. Durable ranges and transaction settlements use
// this stronger identity; immediate collisions use only FileMaterializationTarget.
type MaterializedFileBinding struct {
	target         FileMaterializationTarget
	objectIdentity OwnedObjectID
}

func NewMaterializedFileBinding(
	session OutputSessionID,
	descriptor content.FileRevisionDescriptor,
	locator MaterializationLocator,
	objectIdentity OwnedObjectID,
) (MaterializedFileBinding, error) {
	target, err := NewFileMaterializationTarget(session, descriptor, locator)
	if err != nil {
		return MaterializedFileBinding{}, err
	}
	return BindFileMaterializationTarget(target, objectIdentity)
}

func BindFileMaterializationTarget(
	target FileMaterializationTarget,
	objectIdentity OwnedObjectID,
) (MaterializedFileBinding, error) {
	if !target.valid() || objectIdentity.IsZero() {
		return MaterializedFileBinding{}, ErrInvalidOutputBinding
	}
	return MaterializedFileBinding{target: target, objectIdentity: objectIdentity}, nil
}

func (binding MaterializedFileBinding) Target() FileMaterializationTarget { return binding.target }
func (binding MaterializedFileBinding) OutputSessionID() OutputSessionID {
	return binding.target.OutputSessionID()
}
func (binding MaterializedFileBinding) Descriptor() content.FileRevisionDescriptor {
	return binding.target.Descriptor()
}
func (binding MaterializedFileBinding) ShareInstance() catalog.ShareInstance {
	return binding.target.ShareInstance()
}
func (binding MaterializedFileBinding) FileID() catalog.FileID { return binding.target.FileID() }
func (binding MaterializedFileBinding) FileRevision() content.FileRevision {
	return binding.target.FileRevision()
}
func (binding MaterializedFileBinding) ExactSize() uint64 { return binding.target.ExactSize() }
func (binding MaterializedFileBinding) Locator() MaterializationLocator {
	return binding.target.Locator()
}
func (binding MaterializedFileBinding) ObjectIdentity() OwnedObjectID {
	return binding.objectIdentity
}

func (binding MaterializedFileBinding) valid() bool {
	return binding.target.valid() && !binding.objectIdentity.IsZero()
}

// RangeRetention states which authority can reuse authenticated bytes. A live
// handle and a restart checkpoint are different proofs, even after file Sync.
type RangeRetention uint8

const (
	RangesTransient RangeRetention = iota + 1
	RangesInProcess
	RangesAfterRestart
)

// VerifiedRanges binds progress to its object, revision, and retention scope.
// It never certifies the current bytes of an externally editable public file.
type VerifiedRanges struct {
	retention            RangeRetention
	binding              MaterializedFileBinding
	checkpointGeneration CheckpointGeneration
	ranges               content.RangeSet
}

type CheckpointGeneration uint64

func VerifyRestartRanges(
	binding MaterializedFileBinding,
	checkpointGeneration CheckpointGeneration,
	ranges content.RangeSet,
) (VerifiedRanges, error) {
	return verifyRanges(binding, checkpointGeneration, ranges, RangesAfterRestart)
}

func VerifyProcessRanges(binding MaterializedFileBinding, generation CheckpointGeneration, ranges content.RangeSet) (VerifiedRanges, error) {
	return verifyRanges(binding, generation, ranges, RangesInProcess)
}

// AcknowledgeTransientWrite advances a stream without granting readback authority.
func AcknowledgeTransientWrite(binding MaterializedFileBinding, generation CheckpointGeneration) (VerifiedRanges, error) {
	return verifyRanges(binding, generation, content.RangeSet{}, RangesTransient)
}

func verifyRanges(binding MaterializedFileBinding, checkpointGeneration CheckpointGeneration, ranges content.RangeSet, retention RangeRetention) (VerifiedRanges, error) {
	if !binding.valid() {
		return VerifiedRanges{}, ErrInvalidOutputBinding
	}
	validated, err := content.NewRangeSet(ranges.Ranges())
	if err != nil {
		return VerifiedRanges{}, err
	}
	for _, current := range validated.Ranges() {
		if current.End > binding.ExactSize() {
			return VerifiedRanges{}, ErrInvalidOutputBinding
		}
	}
	return VerifiedRanges{
		binding: binding, checkpointGeneration: checkpointGeneration, ranges: validated, retention: retention,
	}, nil
}

func (r VerifiedRanges) Retention() RangeRetention { return r.retention }

func (r VerifiedRanges) Binding() MaterializedFileBinding { return r.binding }
func (r VerifiedRanges) CheckpointGeneration() CheckpointGeneration {
	return r.checkpointGeneration
}
func (r VerifiedRanges) Ranges() content.RangeSet {
	clone, _ := content.NewRangeSet(r.ranges.Ranges())
	return clone
}

// FileProgress separates scheduling acknowledgement from restart persistence.
// Accepted bytes need not have reached stable storage; only Persisted may be
// reused by a new process. Neither claim proves immunity to later local edits.
type FileProgress struct {
	accepted  VerifiedRanges
	persisted VerifiedRanges
}

func NewFileProgress(accepted, persisted VerifiedRanges) (FileProgress, error) {
	if !accepted.binding.valid() || accepted.retention < RangesTransient || accepted.retention > RangesAfterRestart {
		return FileProgress{}, ErrInvalidOutputBinding
	}
	if persisted.retention != 0 && (persisted.retention != RangesAfterRestart ||
		persisted.binding != accepted.binding || !rangesContain(accepted.ranges, persisted.ranges)) {
		return FileProgress{}, ErrInvalidOutputBinding
	}
	return FileProgress{accepted: accepted, persisted: persisted}, nil
}

func ProgressFromCheckpoint(checkpoint VerifiedRanges) (FileProgress, error) {
	persisted := VerifiedRanges{}
	if checkpoint.Retention() == RangesAfterRestart {
		persisted = checkpoint
	}
	return NewFileProgress(checkpoint, persisted)
}

func (progress FileProgress) Accepted() VerifiedRanges { return progress.accepted }
func (progress FileProgress) Persisted() (VerifiedRanges, bool) {
	return progress.persisted, progress.persisted.retention == RangesAfterRestart
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

// DirectTreeCapabilities contain only execution facts for prefix-visible tree
// materialization. ReceiveIntent already freezes the artifact and plan semantics.
type DirectTreeCapabilities struct {
	Durability           DurabilityLevel
	RandomWrite          bool
	FileFailureIsolation bool
	ModifiedTime         bool
}

func NewDirectTreeCapabilities(capabilities DirectTreeCapabilities) (DirectTreeCapabilities, error) {
	if capabilities.Durability > DurabilityPowerLoss {
		return DirectTreeCapabilities{}, ErrInvalidOutputBinding
	}
	return capabilities, nil
}

func validateDirectTreeSession(intent ReceiveIntent, output DirectTreeSession) error {
	if output == nil || intent.MaterializationPlan().Kind() != receivecontract.PlanDirectTree ||
		output.SessionID().IsZero() {
		return outputContractFault(nil)
	}
	expectedBinding, err := BindDirectTreeSession(intent)
	actualBinding := output.Binding()
	if err != nil || !actualBinding.valid() || actualBinding != expectedBinding {
		return outputContractFault(err)
	}
	capabilities := output.Capabilities()
	if _, err = NewDirectTreeCapabilities(capabilities); err != nil {
		return outputContractFault(err)
	}
	return nil
}

// AuthenticatedSourceDirectory is sender-authenticated catalog ancestry. It is
// deliberately incapable of naming a destination object.
type AuthenticatedSourceDirectory struct {
	DirectoryID     catalog.DirectoryID
	Generation      catalog.DirectoryGeneration
	ParentAdmission DirectoryAdmission
	SourcePath      ordinaryoutput.SourceCatalogPath
	ModifiedTime    catalog.ModifiedTime
}

// OutputDestinationPath is the executor-only coordinate selected by retained
// destination authority. Its explicit root value prevents a logical result-root
// name from being materialized again below an already-reserved result root.
type OutputDestinationPath struct {
	value       string
	sessionRoot bool
	valid       bool
}

func NewOutputDestinationPath(value string) (OutputDestinationPath, error) {
	canonical, err := catalog.CanonicalPath(value)
	if err != nil || value == "" || canonical != value {
		return OutputDestinationPath{}, errors.Join(ErrInvalidOutputBinding, err)
	}
	return OutputDestinationPath{value: canonical, valid: true}, nil
}

func OutputDestinationSessionRoot() OutputDestinationPath {
	return OutputDestinationPath{sessionRoot: true, valid: true}
}

func (path OutputDestinationPath) String() string { return path.value }
func (path OutputDestinationPath) IsSessionRoot() bool {
	return path.valid && path.sessionRoot && path.value == ""
}
func (path OutputDestinationPath) Valid() bool {
	return path.valid && (path.sessionRoot && path.value == "" || !path.sessionRoot && path.value != "")
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
	if file.IsZero() || opened.Handle.IsZero() || descriptor.ShareInstance() != share ||
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
	target FileMaterializationTarget,
	transaction FileTransaction,
	durable VerifiedRanges,
) error {
	if transaction == nil {
		return outputContractFault(nil)
	}
	binding := transaction.Binding()
	if validateMaterializedFileBinding(target, binding) != nil || durable.Binding() != binding {
		return outputContractFault(nil)
	}
	return nil
}

func validateImmediateFileSettlement(
	target FileMaterializationTarget,
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

func validateMaterializedFileBinding(
	target FileMaterializationTarget,
	binding MaterializedFileBinding,
) error {
	if !target.valid() || !binding.valid() || binding.Target() != target {
		return ErrOutputContract
	}
	return nil
}
