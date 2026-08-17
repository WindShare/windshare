// Package outputcap defines the native capabilities consumed by the incremental
// output runtime. Keeping these contracts below osfs lets platform
// implementations depend on capability semantics without depending on the
// state-machine package that orchestrates them.
package outputcap

import (
	"errors"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer"
)

// CapabilityFact is deliberately binary. Platforms report one fact per
// semantic guarantee rather than smuggling partial support through an enum.
type CapabilityFact uint8

const (
	CapabilityUnsupported CapabilityFact = iota + 1
	CapabilitySupported
)

func (fact CapabilityFact) Valid() bool {
	return fact == CapabilityUnsupported || fact == CapabilitySupported
}

func (fact CapabilityFact) Supported() bool { return fact == CapabilitySupported }

func (fact CapabilityFact) String() string {
	switch fact {
	case CapabilityUnsupported:
		return "unsupported"
	case CapabilitySupported:
		return "supported"
	default:
		return ""
	}
}

// CapabilityReason is closed because callers may expose it in typed diagnostics
// and must never infer behavior from platform error strings.
type CapabilityReason uint8

const (
	CapabilityReasonNone CapabilityReason = iota + 1
	CapabilityReasonUnsupportedFilesystem
	CapabilityReasonNetworkFilesystem
	CapabilityReasonUserspaceFilesystem
	CapabilityReasonCloudPlaceholder
	CapabilityReasonReparseOrNestedMount
	CapabilityReasonUnsafePublication
	CapabilityReasonUnverifiableOperationRecovery
	CapabilityReasonUnverifiableRangeRecovery
	CapabilityReasonUnverifiableCrashCleanup
	CapabilityReasonCleanupJournalOverflow
	CapabilityReasonCleanupOwnershipUnknown
)

func (reason CapabilityReason) Valid() bool {
	return reason >= CapabilityReasonNone && reason <= CapabilityReasonCleanupOwnershipUnknown
}

func (reason CapabilityReason) String() string {
	switch reason {
	case CapabilityReasonNone:
		return "none"
	case CapabilityReasonUnsupportedFilesystem:
		return "unsupported-filesystem"
	case CapabilityReasonNetworkFilesystem:
		return "network-filesystem"
	case CapabilityReasonUserspaceFilesystem:
		return "userspace-filesystem"
	case CapabilityReasonCloudPlaceholder:
		return "cloud-placeholder"
	case CapabilityReasonReparseOrNestedMount:
		return "reparse-or-nested-mount"
	case CapabilityReasonUnsafePublication:
		return "unsafe-publication"
	case CapabilityReasonUnverifiableOperationRecovery:
		return "operation-recovery-unverifiable"
	case CapabilityReasonUnverifiableRangeRecovery:
		return "range-recovery-unverifiable"
	case CapabilityReasonUnverifiableCrashCleanup:
		return "crash-cleanup-unverifiable"
	case CapabilityReasonCleanupJournalOverflow:
		return "cleanup-journal-overflow"
	case CapabilityReasonCleanupOwnershipUnknown:
		return "cleanup-ownership-unknown"
	default:
		return ""
	}
}

// CapabilityEvidence pairs a guarantee with the closed reason that explains a
// negative result. Supported facts intentionally carry no reason.
type CapabilityEvidence struct {
	fact   CapabilityFact
	reason CapabilityReason
}

func SupportedCapability() CapabilityEvidence {
	return CapabilityEvidence{fact: CapabilitySupported, reason: CapabilityReasonNone}
}

func UnsupportedCapability(reason CapabilityReason) (CapabilityEvidence, error) {
	evidence := CapabilityEvidence{fact: CapabilityUnsupported, reason: reason}
	if !evidence.Valid() {
		return CapabilityEvidence{}, ErrInvalidDestinationCapabilities
	}
	return evidence, nil
}

func (evidence CapabilityEvidence) Fact() CapabilityFact     { return evidence.fact }
func (evidence CapabilityEvidence) Reason() CapabilityReason { return evidence.reason }
func (evidence CapabilityEvidence) Supported() bool          { return evidence.fact.Supported() }
func (evidence CapabilityEvidence) Valid() bool {
	return evidence.fact.Valid() && evidence.reason.Valid() &&
		(evidence.fact == CapabilitySupported) == (evidence.reason == CapabilityReasonNone)
}

// DestinationCapabilities keeps independent proofs independent. In particular,
// failure to prove restart recovery cannot erase a proven no-replace publish.
type DestinationCapabilities struct {
	safePublish       CapabilityEvidence
	operationRecovery CapabilityEvidence
	rangeRecovery     CapabilityEvidence
	crashCleanup      CapabilityEvidence
}

func NewDestinationCapabilities(
	safePublish CapabilityEvidence,
	operationRecovery CapabilityEvidence,
	rangeRecovery CapabilityEvidence,
	crashCleanup CapabilityEvidence,
) (DestinationCapabilities, error) {
	capabilities := DestinationCapabilities{
		safePublish: safePublish, operationRecovery: operationRecovery,
		rangeRecovery: rangeRecovery, crashCleanup: crashCleanup,
	}
	if !capabilities.Valid() {
		return DestinationCapabilities{}, ErrInvalidDestinationCapabilities
	}
	return capabilities, nil
}

func (capabilities DestinationCapabilities) SafePublish() CapabilityEvidence {
	return capabilities.safePublish
}
func (capabilities DestinationCapabilities) OperationRecovery() CapabilityEvidence {
	return capabilities.operationRecovery
}
func (capabilities DestinationCapabilities) RangeRecovery() CapabilityEvidence {
	return capabilities.rangeRecovery
}
func (capabilities DestinationCapabilities) CrashCleanup() CapabilityEvidence {
	return capabilities.crashCleanup
}
func (capabilities DestinationCapabilities) Valid() bool {
	return capabilities.safePublish.Valid() && capabilities.operationRecovery.Valid() &&
		capabilities.rangeRecovery.Valid() && capabilities.crashCleanup.Valid()
}

type ExecutionMode uint8

const (
	ExecutionResumable ExecutionMode = iota + 1
	ExecutionLiveOnly
)

const (
	ExecutionModeResumable = ExecutionResumable
	ExecutionModeLiveOnly  = ExecutionLiveOnly
)

func (mode ExecutionMode) Valid() bool {
	return mode == ExecutionResumable || mode == ExecutionLiveOnly
}

func (mode ExecutionMode) String() string {
	switch mode {
	case ExecutionResumable:
		return "resumable"
	case ExecutionLiveOnly:
		return "live-only"
	default:
		return ""
	}
}

// SelectExecutionMode permits live-only output only when both public visibility
// and restart cleanup are proven. Registry or range weakness alone is not a
// reason to weaken either safety guarantee.
func SelectExecutionMode(capabilities DestinationCapabilities) (ExecutionMode, error) {
	if !capabilities.Valid() {
		return 0, ErrInvalidDestinationCapabilities
	}
	if !capabilities.safePublish.Supported() || !capabilities.crashCleanup.Supported() {
		return 0, ErrOrdinaryOutputUnsupported
	}
	if capabilities.operationRecovery.Supported() && capabilities.rangeRecovery.Supported() {
		return ExecutionResumable, nil
	}
	return ExecutionLiveOnly, nil
}

type PublishNoReplaceOutcome uint8

const (
	PublishNoReplaceCommitted PublishNoReplaceOutcome = iota + 1
	PublishNoReplaceCollision
	PublishNoReplaceIndeterminate
)

const (
	PublishCommitted     = PublishNoReplaceCommitted
	PublishCollision     = PublishNoReplaceCollision
	PublishIndeterminate = PublishNoReplaceIndeterminate
)

func (outcome PublishNoReplaceOutcome) Valid() bool {
	return outcome >= PublishNoReplaceCommitted && outcome <= PublishNoReplaceIndeterminate
}

func (outcome PublishNoReplaceOutcome) String() string {
	switch outcome {
	case PublishNoReplaceCommitted:
		return "committed"
	case PublishNoReplaceCollision:
		return "collision"
	case PublishNoReplaceIndeterminate:
		return "indeterminate"
	default:
		return ""
	}
}

var (
	ErrInvalidDestinationCapabilities = errors.New("osfs: destination capabilities are invalid")
	ErrOrdinaryOutputUnsupported      = errors.New("osfs: ordinary output is unsupported by the destination")
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

// RootOpenDisposition aliases the durable ownership value so platform opens
// and ownership-marker recovery cannot assign different meanings to the same
// certified root fact.
type RootOpenDisposition = checkpointmodel.RootOpenDisposition

const (
	CallerProvidedContainer = checkpointmodel.CallerProvidedContainer
	AuthorityCreatedRoot    = checkpointmodel.AuthorityCreatedRoot
)

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

// Platform is intentionally smaller than a general filesystem API. Requiring
// every mutation through fixed directory and file handles makes it difficult
// for the state machine to accidentally turn a persisted locator into authority.
type Platform interface {
	Root() Directory
	RootOpenDisposition() RootOpenDisposition
	AcquirePublicOperationGuard() (PublicOperationGuard, error)
	RootBinding() (OutputRootBinding, error)
	Certification() CertificationID
	Durability() transfer.DurabilityLevel
	ProbeRecoverableFeatures() error
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

// Directory is a live, fixed-handle directory capability. Restart-stable
// ownership is bound at platform certification boundaries rather than exposed as
// a second, path-like authority on each directory handle.
type Directory interface {
	Close() error
	Duplicate() (Directory, error)
	Sync() error
	Names(limit int) ([]string, error)
	ObserveEntry(name string) (EntryKind, error)
	ClassifyExactEntry(name string) (EntryKind, bool, error)
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

	CreateFile(name string, private bool, size int64) (MutableFile, error)
	OpenObservedFile(name string, private bool) (ObservedFile, error)
	OpenRecoveryDurabilityFile(name string, private bool) (RecoveryDurabilityFile, error)
	OpenMutableFile(name string, private bool) (MutableFile, error)
	LinkFileNoReplace(source FileIdentity, name string) (ObservedFile, error)
	ReplacePrivateFile(source FileIdentity, name string) error
	RemoveFile(name string, expected FileIdentity) error
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

// PersistentDirectoryIdentity exposes only a bounded, opaque native identity
// claim. Consumers hash it into OwnedObjectID; the raw claim never becomes a
// path or a capability and therefore cannot authorize later mutation by itself.
type PersistentDirectoryIdentity interface {
	PersistentDirectoryIdentityClaim() ([]byte, error)
}

// PersistentDirectoryIdentityPreparer exposes the enrollment transition for a
// restart-stable native identity. Implementations may mutate filesystem
// metadata, so callers must hold the directory's live public-operation guard;
// keeping this separate from PersistentDirectoryIdentity preserves a strictly
// read-only recovery probe.
type PersistentDirectoryIdentityPreparer interface {
	PreparePersistentDirectoryIdentityClaim() ([]byte, error)
}

// CurrentEntryReference is a live-handle identity witness for one opened entry.
// It is suitable for same-object checks only while retained and is never a
// restart-stable locator.
type CurrentEntryReference interface {
	Kind() EntryKind
	Close() error
}

// Lock retains both the namespace lock and the fixed file capability that
// carries its data.
type Lock interface {
	File() MutableFile
	Close() error
}
