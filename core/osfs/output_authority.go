package osfs

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"math"
	"runtime"
	"sync"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

const maxFilesystemOutputTransactions = 32

const filesystemOutputBackendName = "windshare/native-output/v3"

var filesystemOutputBackendID = func() transfer.OutputBackendID {
	backend, err := transfer.NewOutputBackendID(filesystemOutputBackendName)
	if err != nil {
		panic(err)
	}
	return backend
}()

var (
	errUnsupportedOutputVolume = errors.New("osfs: output root is not on a certified filesystem")
	errOutputRootUnsafe        = errors.New("osfs: output root recovery metadata is unsafe")
	errOutputIntentUnsafe      = errors.New("osfs: resume-intent namespace is unsafe")
	errOutputSessionActive     = errors.New("osfs: output session is already active")
	errOutputSessionClosed     = errors.New("osfs: output session is closed")
	errOutputFileActive        = errors.New("osfs: output file transaction is already active")
	errOutputTransactionLimit  = errors.New("osfs: output transaction limit reached")
	errOutputInspectionLimit   = errors.New("osfs: output namespace inspection limit reached")
	errLegacyOutputState       = errors.New("osfs: legacy v2 output state is untrusted")
	errReservedOutputPath      = errors.New("osfs: selected output path collides with private output state")
)

type FilesystemResumeRoot struct {
	RootPath string
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

type resumeStateEntryPin struct {
	mu       sync.Mutex
	entry    outputV3EntryRef
	consumed bool
}

// resumeStateDirectoryPin shares one fixed root handle across legacy inventory
// items. Each item owns one reference so consuming an item transfers its root
// authority independently from closing the rest of the inventory.
type resumeStateDirectoryPin struct {
	mu         sync.Mutex
	directory  outputV3Directory
	references uint64
}

func newResumeStateDirectoryPin(directory outputV3Directory) *resumeStateDirectoryPin {
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

func (pin *resumeStateDirectoryPin) fixedDirectory() outputV3Directory {
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

func newResumeStateEntryPin(entry outputV3EntryRef) *resumeStateEntryPin {
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

func (pin *resumeStateEntryPin) take() outputV3EntryRef {
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
	sessionKind     outputV3EntryKind
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
		// Portable inventory is available even when v3 certification is unsupported.
		// Discard must reach platform certification to return that typed refusal;
		// discardLegacyState separately requires both live native pins before mutation.
		return reference.legacyName != ""
	}
	if reference.kind == ResumeStateOpaqueUnsafe {
		if reference.root.IsZero() || reference.namespaceName == "" {
			return false
		}
		// Intent-only opaque references are listable but deliberately cannot grant
		// destructive authority. A session-scoped opaque reference is minted only
		// from an enumerated, fixed directory and carries that object's identity.
		return reference.sessionName == "" ||
			(reference.sessionKind != outputV3EntryAbsent && reference.sessionPin.available())
	}
	return !reference.root.IsZero() && !reference.intent.IsZero() && !reference.session.IsZero() &&
		reference.namespaceName != "" && reference.sessionName != "" &&
		reference.sessionKind == outputV3EntryDirectory && reference.sessionPin.available()
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

// ResumeStateInventory owns the native entry pins behind one stable inventory.
// Close releases every unconsumed item. A successful or failed discard consumes
// exactly one item; callers must perform a fresh inventory before retrying.
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
			inventory:  inventory,
			itemID:     itemID,
			intent:     authority.intent,
			session:    authority.session,
			kind:       authority.kind,
			legacyName: authority.legacyName,
		}
	}
	return inventory
}

// Summaries returns an immutable-view copy whose references remain valid only
// while this inventory is open and their item has not been consumed.
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
	return errors.Join(
		reference.sessionPin.Close(), reference.legacyPin.Close(), reference.legacyRoot.Close(),
	)
}

func releaseResumeStateAuthorities(summaries []ResumeStateSummary) error {
	var result error
	for index := range summaries {
		result = errors.Join(result, summaries[index].Reference.releaseAuthority())
	}
	return result
}

// Close deterministically releases all unconsumed native inventory handles.
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

type outputSessionIDGenerator interface {
	NewOutputSessionID() (transfer.OutputSessionID, error)
}

type outputObjectIDGenerator interface {
	NewOutputObjectID() (resumestate.OutputObjectID, error)
}

type FilesystemOutputTraceOperation uint8

const (
	TraceFilesystemCertified FilesystemOutputTraceOperation = iota + 1
	TraceFeatureProbeCompleted
	TraceControlBootstrap
	TraceNativeLock
	TraceSessionOpened
	TraceFilePhaseTransition
	TraceFileRecoveryDecision
	TraceFileSettlement
	TraceSessionSettlement
	TraceStateInstallCutAdopted
	TraceAncestryValidation
)

type FilesystemOutputFileSettlementBoundary uint8

const (
	FilesystemOutputSettlementBeginFile FilesystemOutputFileSettlementBoundary = iota + 1
	FilesystemOutputSettlementCommit
	FilesystemOutputSettlementPause
	FilesystemOutputSettlementJobPause
	FilesystemOutputSettlementBeginFileCleanup
	FilesystemOutputSettlementRetire
)

type FilesystemOutputNativeLockScope uint8

const (
	FilesystemOutputNativeLockCoordinator FilesystemOutputNativeLockScope = iota + 1
	FilesystemOutputNativeLockSession
)

type FilesystemOutputNativeLockMilestone uint8

const (
	FilesystemOutputNativeLockAcquired FilesystemOutputNativeLockMilestone = iota + 1
	FilesystemOutputNativeLockContended
	FilesystemOutputNativeLockAcquireFailed
	FilesystemOutputNativeLockReleased
	FilesystemOutputNativeLockReleaseReportedFailure
)

type FilesystemOutputAncestryBoundary uint8

const (
	FilesystemOutputAncestryAdmission FilesystemOutputAncestryBoundary = iota + 1
	FilesystemOutputAncestryRestart
	FilesystemOutputAncestryBeginFile
	FilesystemOutputAncestryRecovery
	FilesystemOutputAncestryPublicationPre
	FilesystemOutputAncestryPublicationPost
	FilesystemOutputAncestryDirectoryFinalize
	FilesystemOutputAncestrySessionFinalize
)

type FilesystemOutputAncestryDecision uint8

const (
	FilesystemOutputAncestryPrepared FilesystemOutputAncestryDecision = iota + 1
	FilesystemOutputAncestryMatched
	FilesystemOutputAncestryMismatch
	FilesystemOutputAncestryAuthorityDenied
	FilesystemOutputAncestryStructuralUnsafe
)

type FilesystemOutputStateInstallStage uint8

const (
	FilesystemOutputStateCreate FilesystemOutputStateInstallStage = iota + 1
	FilesystemOutputStateReplace
)

type FilesystemOutputTrace struct {
	Operation                 FilesystemOutputTraceOperation
	ResumeIntent              transfer.ResumeIntent
	SessionID                 transfer.OutputSessionID
	LocatorDigest             transfer.OutputLocatorDigest
	OutputObjectID            transfer.OutputObjectIdentity
	PreviousPhase             FilesystemOutputFilePhase
	NextPhase                 FilesystemOutputFilePhase
	RecoveryAction            FilesystemOutputRecoveryAction
	FileSettlement            transfer.FileSettlementKind
	FileSettlementBoundary    FilesystemOutputFileSettlementBoundary
	FilePauseReason           transfer.FilePauseReason
	FileRetireReason          transfer.FileRetireReason
	QuarantineReason          transfer.QuarantineReason
	JobSettlement             transfer.JobSettlementKind
	FailureScope              transfer.OutputFaultScope
	FailureCode               transfer.OutputFaultCode
	Certification             FilesystemOutputCertificationID
	StateGeneration           uint64
	StateInstallStage         FilesystemOutputStateInstallStage
	SelectionIdentity         transfer.SelectionIdentity
	OutputAncestryDigest      FilesystemOutputAncestryDigest
	AncestryBoundary          FilesystemOutputAncestryBoundary
	AncestryDecision          FilesystemOutputAncestryDecision
	AncestryClaimCount        uint32
	NativeLockScope           FilesystemOutputNativeLockScope
	NativeLockMilestone       FilesystemOutputNativeLockMilestone
	MutationReportedFailure   bool
	ParentSyncReportedFailure bool
	Failed                    bool
}

type FilesystemOutputTracer interface {
	// Implementations must tolerate concurrent delivery from independent file and lock workflows.
	TraceFilesystemOutput(FilesystemOutputTrace)
}

type FilesystemOutputTraceFunc func(FilesystemOutputTrace)

func (function FilesystemOutputTraceFunc) TraceFilesystemOutput(event FilesystemOutputTrace) {
	if function != nil {
		function(event)
	}
}

type FilesystemOutputAuthorityConfig struct {
	RootPath   string
	CreateRoot bool
	Tracer     FilesystemOutputTracer
}

type FilesystemOutputAuthority struct {
	rootPath        string
	createRoot      bool
	sessionIDs      outputSessionIDGenerator
	objectIDs       outputObjectIDGenerator
	tracer          FilesystemOutputTracer
	platformFactory func(string, bool) (outputV3Platform, error)
	random          io.Reader
}

func NewFilesystemOutputAuthority(config FilesystemOutputAuthorityConfig) (*FilesystemOutputAuthority, error) {
	return &FilesystemOutputAuthority{
		rootPath: config.RootPath, createRoot: config.CreateRoot,
		sessionIDs: cryptographicOutputSessionIDs{}, objectIDs: cryptographicOutputObjectIDs{}, tracer: config.Tracer,
		platformFactory: openOutputV3Platform, random: rand.Reader,
	}, nil
}

type cryptographicOutputSessionIDs struct{}

func (cryptographicOutputSessionIDs) NewOutputSessionID() (transfer.OutputSessionID, error) {
	var raw [transfer.OutputSessionIdentityBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return transfer.OutputSessionID{}, err
	}
	return transfer.OutputSessionIDFromBytes(raw[:])
}

type cryptographicOutputObjectIDs struct{}

func (cryptographicOutputObjectIDs) NewOutputObjectID() (resumestate.OutputObjectID, error) {
	return resumestate.NewOutputObjectID()
}

func ListResumeState(ctx context.Context, root FilesystemResumeRoot) (*ResumeStateInventory, error) {
	authority, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{})
	if err != nil {
		return nil, err
	}
	return authority.listResumeState(ctx, root)
}

func DiscardResumeState(ctx context.Context, reference ResumeStateRef) (DiscardSettlement, error) {
	authority, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{})
	if err != nil {
		return DiscardSettlement{}, err
	}
	return authority.discardResumeState(ctx, reference)
}

func (authority *FilesystemOutputAuthority) trace(event FilesystemOutputTrace) {
	if authority != nil && authority.tracer != nil {
		authority.tracer.TraceFilesystemOutput(event)
	}
}

func outputFault(scope transfer.OutputFaultScope, code transfer.OutputFaultCode, cause error) error {
	return transfer.NewOutputFault(scope, code, cause)
}
