package outputruntime

import (
	"crypto/sha256"
	"errors"
	"math"
	"runtime"
	"sync"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

type FilesystemOutputFilePhase uint8

const (
	FilesystemOutputFileReserved FilesystemOutputFilePhase = iota + 1
	FilesystemOutputFileWitnessed
	FilesystemOutputFilePublishing
	FilesystemOutputFilePublishBlocked
	FilesystemOutputFilePublished
	FilesystemOutputFileRetiring
	FilesystemOutputFileQuarantined
)

func filesystemOutputFilePhaseFromState(phase resumestate.FilePhase) FilesystemOutputFilePhase {
	switch phase {
	case resumestate.FileReserved:
		return FilesystemOutputFileReserved
	case resumestate.FileWitnessed:
		return FilesystemOutputFileWitnessed
	case resumestate.FilePublishing:
		return FilesystemOutputFilePublishing
	case resumestate.FilePublishBlocked:
		return FilesystemOutputFilePublishBlocked
	case resumestate.FilePublished:
		return FilesystemOutputFilePublished
	case resumestate.FileRetiring:
		return FilesystemOutputFileRetiring
	case resumestate.FileQuarantined:
		return FilesystemOutputFileQuarantined
	default:
		return 0
	}
}

type FilesystemOutputRecoveryAction uint8

const (
	FilesystemOutputRecoveryRetryObjectCreation FilesystemOutputRecoveryAction = iota + 1
	FilesystemOutputRecoveryInstallWitness
	FilesystemOutputRecoveryRequireRevisionBinding
	FilesystemOutputRecoveryResumeContent
	FilesystemOutputRecoveryInstallPublishing
	FilesystemOutputRecoveryLinkFinalNoReplace
	FilesystemOutputRecoverySyncFinalParent
	FilesystemOutputRecoveryInstallPublished
	FilesystemOutputRecoveryInstallPublishBlocked
	FilesystemOutputRecoveryHoldPublishBlocked
	FilesystemOutputRecoveryRemovePublishedStageAndSync
	FilesystemOutputRecoverySyncPublishedStageParent
	FilesystemOutputRecoveryRemoveRetiringStageAndSync
	FilesystemOutputRecoverySyncStageRemoveAnchorAndSync
	FilesystemOutputRecoverySyncParentsRemoveRecordAndSync
	FilesystemOutputRecoveryInstallRetiring
	FilesystemOutputRecoveryInstallQuarantine
	FilesystemOutputRecoveryHoldQuarantine
	FilesystemOutputRecoveryHoldPublishedCleanup
	FilesystemOutputRecoveryHoldRetiringCleanup
)

func filesystemOutputRecoveryActionFromState(action resumestate.RecoveryAction) FilesystemOutputRecoveryAction {
	switch action {
	case resumestate.RecoveryRetryObjectCreation:
		return FilesystemOutputRecoveryRetryObjectCreation
	case resumestate.RecoveryInstallWitness:
		return FilesystemOutputRecoveryInstallWitness
	case resumestate.RecoveryRequireRevisionBinding:
		return FilesystemOutputRecoveryRequireRevisionBinding
	case resumestate.RecoveryResumeContent:
		return FilesystemOutputRecoveryResumeContent
	case resumestate.RecoveryInstallPublishing:
		return FilesystemOutputRecoveryInstallPublishing
	case resumestate.RecoveryLinkFinalNoReplace:
		return FilesystemOutputRecoveryLinkFinalNoReplace
	case resumestate.RecoverySyncFinalParent:
		return FilesystemOutputRecoverySyncFinalParent
	case resumestate.RecoveryInstallPublished:
		return FilesystemOutputRecoveryInstallPublished
	case resumestate.RecoveryInstallPublishBlocked:
		return FilesystemOutputRecoveryInstallPublishBlocked
	case resumestate.RecoveryHoldPublishBlocked:
		return FilesystemOutputRecoveryHoldPublishBlocked
	case resumestate.RecoveryRemovePublishedStageAndSync:
		return FilesystemOutputRecoveryRemovePublishedStageAndSync
	case resumestate.RecoverySyncPublishedStageParent:
		return FilesystemOutputRecoverySyncPublishedStageParent
	case resumestate.RecoveryRemoveRetiringStageAndSync:
		return FilesystemOutputRecoveryRemoveRetiringStageAndSync
	case resumestate.RecoverySyncStageRemoveAnchorAndSync:
		return FilesystemOutputRecoverySyncStageRemoveAnchorAndSync
	case resumestate.RecoverySyncParentsRemoveRecordAndSync:
		return FilesystemOutputRecoverySyncParentsRemoveRecordAndSync
	case resumestate.RecoveryInstallRetiring:
		return FilesystemOutputRecoveryInstallRetiring
	case resumestate.RecoveryInstallQuarantine:
		return FilesystemOutputRecoveryInstallQuarantine
	case resumestate.RecoveryHoldQuarantine:
		return FilesystemOutputRecoveryHoldQuarantine
	case resumestate.RecoveryHoldPublishedCleanup:
		return FilesystemOutputRecoveryHoldPublishedCleanup
	case resumestate.RecoveryHoldRetiringCleanup:
		return FilesystemOutputRecoveryHoldRetiringCleanup
	default:
		return 0
	}
}

type FilesystemOutputCertificationID string

const (
	FilesystemOutputCertificationLinuxExt4ProcessRestart   FilesystemOutputCertificationID = "linux/ext4/process-restart/v2"
	FilesystemOutputCertificationWindowsNTFSProcessRestart FilesystemOutputCertificationID = "windows/ntfs/process-restart/v1"
)

func filesystemOutputCertificationFromState(certification resumestate.CertificationID) FilesystemOutputCertificationID {
	switch certification {
	case resumestate.CertificationLinuxExt4ProcessRestart:
		return FilesystemOutputCertificationLinuxExt4ProcessRestart
	case resumestate.CertificationWindowsNTFSProcessRestart:
		return FilesystemOutputCertificationWindowsNTFSProcessRestart
	default:
		return ""
	}
}

type FilesystemOutputAncestryDigest [sha256.Size]byte

func (digest FilesystemOutputAncestryDigest) Bytes() []byte { return append([]byte(nil), digest[:]...) }

func filesystemOutputAncestryDigestFromState(binding resumestate.OutputAncestryBinding) FilesystemOutputAncestryDigest {
	var digest FilesystemOutputAncestryDigest
	copy(digest[:], binding.Bytes())
	return digest
}

func outputLocatorDigestFromState(digest resumestate.LocatorDigest) transfer.OutputLocatorDigest {
	return transfer.OutputLocatorDigest(digest)
}

func outputObjectIdentityFromState(id resumestate.OutputObjectID) transfer.OutputObjectIdentity {
	return transfer.OutputObjectIdentity(id)
}

type ResumeSessionLifecycle uint8

const (
	ResumeSessionActive ResumeSessionLifecycle = iota + 1
	ResumeSessionPausing
	ResumeSessionPaused
	ResumeSessionPausedNeedsAttention
	ResumeSessionCompleting
	ResumeSessionDiscarding
)

func resumeSessionLifecycleFromState(lifecycle resumestate.SessionLifecycle) ResumeSessionLifecycle {
	switch lifecycle {
	case resumestate.SessionActive:
		return ResumeSessionActive
	case resumestate.SessionPausing:
		return ResumeSessionPausing
	case resumestate.SessionPaused:
		return ResumeSessionPaused
	case resumestate.SessionPausedNeedsAttention:
		return ResumeSessionPausedNeedsAttention
	case resumestate.SessionCompleting:
		return ResumeSessionCompleting
	case resumestate.SessionDiscarding:
		return ResumeSessionDiscarding
	default:
		return 0
	}
}

type ResumeAttentionScope uint8

const (
	ResumeAttentionFile ResumeAttentionScope = iota + 1
	ResumeAttentionIntent
	ResumeAttentionRoot
	ResumeAttentionLegacy
)

type ResumeAttention struct {
	Scope  ResumeAttentionScope
	Code   string
	State  string
	Detail string
}

type ResumeStateKind uint8

const (
	ResumeStateRecoverable ResumeStateKind = iota + 1
	ResumeStateNeedsAttention
	ResumeStateLegacyUntrusted
	ResumeStateOpaqueUnsafe
)

// resumeStateEntryPin owns a current-name reference. It deliberately cannot be
// serialized or converted into persistent identity; consuming it transfers only
// the live authority captured during this inventory operation.
type resumeStateEntryPin struct {
	mu       sync.Mutex
	entry    outputcap.CurrentEntryReference
	consumed bool
}

// resumeStateDirectoryPin shares one fixed root handle across legacy inventory
// items. Each item owns one reference so consuming an item transfers its root
// authority independently from closing the rest of the inventory.
type resumeStateDirectoryPin struct {
	mu         sync.Mutex
	directory  outputcap.Directory
	references uint64
}

func newResumeStateDirectoryPin(directory outputcap.Directory) *resumeStateDirectoryPin {
	if directory == nil {
		return nil
	}
	pin := &resumeStateDirectoryPin{directory: directory, references: 1}
	runtime.SetFinalizer(pin, func(stale *resumeStateDirectoryPin) { _ = stale.forceClose() })
	return pin
}

func (pin *resumeStateDirectoryPin) retain() bool {
	if pin == nil {
		return false
	}
	pin.mu.Lock()
	defer pin.mu.Unlock()
	if pin.directory == nil || pin.references == 0 || pin.references == math.MaxUint64 {
		return false
	}
	pin.references++
	return true
}

func (pin *resumeStateDirectoryPin) available() bool {
	if pin == nil {
		return false
	}
	pin.mu.Lock()
	defer pin.mu.Unlock()
	return pin.directory != nil && pin.references != 0
}

func (pin *resumeStateDirectoryPin) fixedDirectory() outputcap.Directory {
	if pin == nil {
		return nil
	}
	pin.mu.Lock()
	defer pin.mu.Unlock()
	if pin.references == 0 {
		return nil
	}
	return pin.directory
}

func (pin *resumeStateDirectoryPin) Close() error {
	if pin == nil {
		return nil
	}
	pin.mu.Lock()
	if pin.directory == nil || pin.references == 0 {
		pin.mu.Unlock()
		return nil
	}
	pin.references--
	if pin.references != 0 {
		pin.mu.Unlock()
		return nil
	}
	directory := pin.directory
	pin.directory = nil
	runtime.SetFinalizer(pin, nil)
	pin.mu.Unlock()
	return directory.Close()
}

func (pin *resumeStateDirectoryPin) forceClose() error {
	if pin == nil {
		return nil
	}
	pin.mu.Lock()
	directory := pin.directory
	pin.directory = nil
	pin.references = 0
	runtime.SetFinalizer(pin, nil)
	pin.mu.Unlock()
	return closeOutputV3Directory(directory)
}

func newResumeStateEntryPin(entry outputcap.CurrentEntryReference) *resumeStateEntryPin {
	if entry == nil {
		return nil
	}
	pin := &resumeStateEntryPin{entry: entry}
	runtime.SetFinalizer(pin, func(stale *resumeStateEntryPin) { _ = stale.Close() })
	return pin
}

func (pin *resumeStateEntryPin) available() bool {
	if pin == nil {
		return false
	}
	pin.mu.Lock()
	defer pin.mu.Unlock()
	return !pin.consumed && pin.entry != nil
}

func (pin *resumeStateEntryPin) take() outputcap.CurrentEntryReference {
	if pin == nil {
		return nil
	}
	pin.mu.Lock()
	defer pin.mu.Unlock()
	if pin.consumed || pin.entry == nil {
		return nil
	}
	entry := pin.entry
	pin.entry = nil
	pin.consumed = true
	runtime.SetFinalizer(pin, nil)
	return entry
}

func (pin *resumeStateEntryPin) Close() error {
	if pin == nil {
		return nil
	}
	pin.mu.Lock()
	entry := pin.entry
	pin.entry = nil
	pin.consumed = true
	runtime.SetFinalizer(pin, nil)
	pin.mu.Unlock()
	if entry == nil {
		return nil
	}
	return entry.Close()
}

type ResumeStateRef struct {
	inventory       *ResumeStateInventory
	itemID          uint64
	rootPath        string
	root            resumestate.OutputRootBinding
	intent          transfer.ResumeIntent
	session         transfer.OutputSessionID
	kind            ResumeStateKind
	namespaceName   string
	sessionName     string
	sessionKind     outputcap.EntryKind
	sessionPin      *resumeStateEntryPin
	legacyName      string
	legacyRemovable bool
	legacySize      uint64
	legacyDigest    [32]byte
	legacyPin       *resumeStateEntryPin
	legacyRoot      *resumeStateDirectoryPin
}

func (reference ResumeStateRef) ResumeIntent() transfer.ResumeIntent { return reference.intent }
func (reference ResumeStateRef) SessionID() transfer.OutputSessionID { return reference.session }
func (reference ResumeStateRef) Kind() ResumeStateKind               { return reference.kind }

func (reference ResumeStateRef) validAuthority() bool {
	if reference.rootPath == "" || reference.kind < ResumeStateRecoverable || reference.kind > ResumeStateOpaqueUnsafe {
		return false
	}
	if reference.kind == ResumeStateLegacyUntrusted {
		return reference.legacyName != ""
	}
	if reference.kind == ResumeStateOpaqueUnsafe {
		if reference.root.IsZero() || reference.namespaceName == "" {
			return false
		}
		return reference.sessionName == "" ||
			(reference.sessionKind != outputcap.EntryAbsent && reference.sessionPin.available())
	}
	return !reference.root.IsZero() && !reference.intent.IsZero() && !reference.session.IsZero() &&
		reference.namespaceName != "" && reference.sessionName != "" &&
		reference.sessionKind == outputcap.EntryDirectory && reference.sessionPin.available()
}

type ResumeStateSummary struct {
	Reference      ResumeStateRef
	Lifecycle      ResumeSessionLifecycle
	FileRecords    uint64
	AllocatedBytes uint64
	Attention      []ResumeAttention
}

type resumeStateInventoryItem struct {
	authority ResumeStateRef
}

type ResumeStateInventory struct {
	mu        sync.Mutex
	summaries []ResumeStateSummary
	items     map[uint64]resumeStateInventoryItem
	closed    bool
}

func newResumeStateInventory(summaries []ResumeStateSummary) *ResumeStateInventory {
	inventory := &ResumeStateInventory{
		summaries: summaries,
		items:     make(map[uint64]resumeStateInventoryItem, len(summaries)),
	}
	for index := range inventory.summaries {
		itemID := uint64(index + 1)
		authority := inventory.summaries[index].Reference
		inventory.items[itemID] = resumeStateInventoryItem{authority: authority}
		inventory.summaries[index].Reference = ResumeStateRef{
			inventory: inventory, itemID: itemID, intent: authority.intent,
			session: authority.session, kind: authority.kind, legacyName: authority.legacyName,
		}
	}
	return inventory
}

func (inventory *ResumeStateInventory) Summaries() []ResumeStateSummary {
	if inventory == nil {
		return nil
	}
	inventory.mu.Lock()
	defer inventory.mu.Unlock()
	if inventory.closed {
		return nil
	}
	result := make([]ResumeStateSummary, len(inventory.summaries))
	copy(result, inventory.summaries)
	for index := range result {
		result[index].Attention = append([]ResumeAttention(nil), result[index].Attention...)
	}
	return result
}

func (inventory *ResumeStateInventory) consume(reference ResumeStateRef) (ResumeStateRef, error) {
	if inventory == nil || reference.inventory != inventory || reference.itemID == 0 {
		return ResumeStateRef{}, transfer.ErrInvalidOutputBinding
	}
	inventory.mu.Lock()
	defer inventory.mu.Unlock()
	if inventory.closed {
		return ResumeStateRef{}, transfer.ErrInvalidOutputBinding
	}
	item, found := inventory.items[reference.itemID]
	if !found || !resumeStateReferenceMetadataMatches(reference, item.authority) {
		return ResumeStateRef{}, transfer.ErrInvalidOutputBinding
	}
	delete(inventory.items, reference.itemID)
	return item.authority, nil
}

func resumeStateReferenceMetadataMatches(public, authority ResumeStateRef) bool {
	return public.intent == authority.intent && public.session == authority.session &&
		public.kind == authority.kind && public.legacyName == authority.legacyName
}

func (reference ResumeStateRef) releaseAuthority() error {
	return errors.Join(reference.sessionPin.Close(), reference.legacyPin.Close(), reference.legacyRoot.Close())
}

func releaseResumeStateAuthorities(summaries []ResumeStateSummary) error {
	var result error
	for index := range summaries {
		result = errors.Join(result, summaries[index].Reference.releaseAuthority())
	}
	return result
}

func (inventory *ResumeStateInventory) Close() error {
	if inventory == nil {
		return nil
	}
	inventory.mu.Lock()
	if inventory.closed {
		inventory.mu.Unlock()
		return nil
	}
	inventory.closed = true
	items := inventory.items
	inventory.items = nil
	inventory.summaries = nil
	inventory.mu.Unlock()
	var result error
	for _, item := range items {
		result = errors.Join(result, item.authority.releaseAuthority())
	}
	return result
}

type DiscardSettlementKind uint8

const (
	Discarded DiscardSettlementKind = iota + 1
	DiscardAlreadyAbsent
)

type DiscardSettlement struct {
	Kind         DiscardSettlementKind
	RemovedBytes uint64
}
