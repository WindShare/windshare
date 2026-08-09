package outputsession

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"strings"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
)

type sessionState uint8

const (
	sessionOpen sessionState = iota + 1
	sessionPaused
	sessionCompleted
)

type directoryState uint8

const (
	directoryPending directoryState = iota + 1
	directoryAdmitted
	directorySettling
	directorySettled
)

type fileState uint8

const (
	filePending fileState = iota + 1
	fileActive
	fileSettled
)

type claimRef struct {
	kind ClaimKind
	id   ClaimID
}

type parentNameKey struct {
	parent ClaimID
	name   string
}

type admissionReceiptKey [sha256.Size]byte

type directoryAdmissionOperation struct {
	done      chan struct{}
	admission transfer.DirectoryAdmission
	err       error
}

type directoryFinalizationOperation struct {
	done       chan struct{}
	settlement transfer.DirectorySettlement
	err        error
}

type fileBeginOperation struct {
	done  chan struct{}
	start transfer.FileStart
	err   error
}

type fileTransactionOperation struct {
	done       chan struct{}
	action     transactionAction
	argument   uint8
	checkpoint transfer.VerifiedDurableRanges
	settlement transfer.FileSettlement
	err        error
}

type directoryEntry struct {
	claim                   DirectoryClaim
	admission               transfer.DirectoryAdmission
	state                   directoryState
	disposition             DirectoryDisposition
	metadataBytes           uint64
	directUnsettledChildren uint64
	activeDescendants       uint64
	changed                 chan struct{}
	admissionOperation      *directoryAdmissionOperation
	finalizationOperation   *directoryFinalizationOperation
	settlement              transfer.DirectorySettlement
	uncertain               bool
}

type fileEntry struct {
	claim            FileClaim
	state            fileState
	beginOperation   *fileBeginOperation
	operation        *fileTransactionOperation
	transaction      *guardedTransaction
	settlement       transfer.FileSettlement
	terminalAction   transactionAction
	terminalArgument uint8
	uncertain        bool
}

type closeKind uint8

const (
	closePause closeKind = iota + 1
	closeComplete
)

type closeRecord struct {
	set        bool
	kind       closeKind
	pause      transfer.JobPauseReason
	outcome    transfer.DirectTreeOutcome
	settlement transfer.DirectTreeSettlement
	err        error
}

// Session owns only volatile output authority. Native handles and durable
// checkpoint state remain behind the injected executors.
type Session struct {
	gate operationGate
	mu   sync.Mutex

	state         sessionState
	requiredFault fault.Fault
	attention     bool
	close         closeRecord

	intent       transfer.ReceiveIntent
	binding      transfer.DirectTreeSessionBinding
	scope        transfer.DirectoryAdmissionScope
	sessionID    transfer.OutputSessionID
	capabilities transfer.DirectTreeCapabilities
	secret       [sha256.Size]byte
	limits       Limits

	locator     LocatorCanonicalizer
	directories DirectoryExecutor
	files       FileExecutor
	resources   ResourceReleaser
	lifecycle   TreeLifecycleRecorder
	trace       TraceSink

	nextClaimID     ClaimID
	nextOperationID uint64
	rootClaim       ClaimID
	metadataBytes   uint64
	activeFiles     uint64
	fileSlots       uint64

	directoryClaims map[ClaimID]*directoryEntry
	fileClaims      map[ClaimID]*fileEntry
	nodeClaims      map[catalog.NodeID]claimRef
	pathClaims      map[string]claimRef
	locatorClaims   map[string]claimRef
	nameClaims      map[parentNameKey]claimRef
	receiptClaims   map[admissionReceiptKey]ClaimID
}

var _ transfer.DirectTreeSession = (*Session)(nil)

func New(config Config) (*Session, error) {
	scope, limits, err := config.validate()
	if err != nil {
		return nil, err
	}
	session := &Session{
		gate:            newOperationGate(),
		state:           sessionOpen,
		intent:          config.Intent,
		scope:           scope,
		sessionID:       config.SessionID,
		capabilities:    config.Capabilities,
		limits:          limits,
		locator:         config.Locator,
		directories:     config.Directories,
		files:           config.Files,
		resources:       config.Resources,
		lifecycle:       config.Lifecycle,
		trace:           config.Trace,
		directoryClaims: make(map[ClaimID]*directoryEntry),
		fileClaims:      make(map[ClaimID]*fileEntry),
		nodeClaims:      make(map[catalog.NodeID]claimRef),
		pathClaims:      make(map[string]claimRef),
		locatorClaims:   make(map[string]claimRef),
		nameClaims:      make(map[parentNameKey]claimRef),
		receiptClaims:   make(map[admissionReceiptKey]ClaimID),
	}
	session.binding, err = transfer.BindDirectTreeSession(config.Intent)
	if err != nil {
		return nil, errors.Join(ErrInvalidConfiguration, err)
	}
	copy(session.secret[:], config.ReceiptSecret)
	return session, nil
}

func (session *Session) Binding() transfer.DirectTreeSessionBinding {
	if session == nil {
		return transfer.DirectTreeSessionBinding{}
	}
	return session.binding
}

func (session *Session) SessionID() transfer.OutputSessionID {
	if session == nil {
		return transfer.OutputSessionID{}
	}
	return session.sessionID
}

func (session *Session) Capabilities() transfer.DirectTreeCapabilities {
	if session == nil {
		return transfer.DirectTreeCapabilities{}
	}
	return session.capabilities
}

func (session *Session) beginOperation() (*operationLease, uint64, error) {
	if session == nil {
		return nil, 0, ErrInvalidConfiguration
	}
	lease, err := session.gate.acquire()
	if err != nil {
		return nil, 0, err
	}
	session.mu.Lock()
	operationID, err := session.nextOperationLocked()
	session.mu.Unlock()
	if err != nil {
		lease.release()
		return nil, 0, err
	}
	return lease, operationID, nil
}

func (session *Session) nextOperationLocked() (uint64, error) {
	if session.nextOperationID == math.MaxUint64 {
		value := fault.DependencyContractFault()
		session.requirePauseLocked(value, true)
		return 0, fault.Wrap(value, ErrExecutorContract)
	}
	session.nextOperationID++
	return session.nextOperationID, nil
}

func (session *Session) nextClaimLocked() (ClaimID, error) {
	if session.nextClaimID == ClaimID(math.MaxUint64) {
		value := fault.DependencyContractFault()
		session.requirePauseLocked(value, true)
		return 0, fault.Wrap(value, ErrExecutorContract)
	}
	session.nextClaimID++
	return session.nextClaimID, nil
}

func (session *Session) operationRejectionLocked() error {
	if session.state != sessionOpen {
		return sessionClosedError()
	}
	if session.requiredFault.Valid() {
		return fault.Wrap(session.requiredFault, ErrSessionRequiresPause)
	}
	return nil
}

func (session *Session) operationRejectionOrInvariantLocked() error {
	if err := session.operationRejectionLocked(); err != nil {
		return err
	}
	return session.markInvariantFailureLocked()
}

func (session *Session) requirePauseLocked(value fault.Fault, attention bool) {
	if value.Valid() && value.Scope() >= fault.ScopeOutputPause {
		session.requiredFault = fault.Join(session.requiredFault, value)
	}
	if attention {
		session.attention = true
	}
}

func (session *Session) normalizeFailureLocked(ctx context.Context, err error, cut MutationCut) (fault.Fault, error) {
	result := fault.NormalizeBoundary(ctx, err)
	if cut == MutationAmbiguous {
		ambiguous, _ := fault.NewOutput(fault.ScopeOutputPause, fault.OutputMutationAmbiguous)
		value := ambiguous
		if observed, ok := result.Fault(); ok {
			// Mutation ambiguity raises the minimum authority scope; it must not
			// downgrade a collaborator's already-normalized terminal fault.
			value = fault.Join(ambiguous, observed)
		}
		session.requirePauseLocked(value, true)
		if result.Kind() == fault.BoundaryCanceled {
			return value, contextFailure(ctx, err)
		}
		return value, fault.Wrap(value, errors.Join(ErrMutationAmbiguous, err))
	}
	if result.Kind() == fault.BoundaryCanceled {
		return fault.Fault{}, contextFailure(ctx, err)
	}
	value, ok := result.Fault()
	if !ok {
		value = fault.DependencyContractFault()
	}
	session.requirePauseLocked(value, false)
	return value, fault.Wrap(value, err)
}

func contextFailure(ctx context.Context, err error) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return context.Canceled
}

func (session *Session) traceLocked(
	operationID uint64,
	operation OperationKind,
	decision TraceDecision,
	claimID ClaimID,
	kind ClaimKind,
	from ClaimState,
	to ClaimState,
	value fault.Fault,
) TraceEvent {
	return TraceEvent{
		ReceiveIntentDigest:    session.intent.Digest(),
		SessionID:              session.sessionID,
		OperationID:            operationID,
		Operation:              operation,
		Decision:               decision,
		ClaimID:                claimID,
		ClaimKind:              kind,
		From:                   from,
		To:                     to,
		Fault:                  value,
		NodeClaims:             uint64(len(session.nodeClaims)),
		DirectoryClaims:        uint64(len(session.directoryClaims)),
		FileClaims:             uint64(len(session.fileClaims)),
		ActiveFileClaims:       session.activeFiles,
		ReservedFileSlots:      session.fileSlots,
		DirectoryMetadataBytes: session.metadataBytes,
	}
}

func (session *Session) emit(event TraceEvent) {
	if session != nil && session.trace != nil {
		session.trace.RecordOutputSessionTrace(event)
	}
}

func directoryClaimState(state directoryState) ClaimState {
	switch state {
	case directoryPending:
		return ClaimPending
	case directoryAdmitted:
		return ClaimAdmitted
	case directorySettling:
		return ClaimSettling
	case directorySettled:
		return ClaimSettled
	default:
		return 0
	}
}

func fileClaimState(state fileState) ClaimState {
	switch state {
	case filePending:
		return ClaimPending
	case fileActive:
		return ClaimActive
	case fileSettled:
		return ClaimSettled
	default:
		return 0
	}
}

func claimName(path string) string {
	separator := strings.LastIndexByte(path, '/')
	if separator < 0 {
		return path
	}
	return path[separator+1:]
}

func parentPath(path string) string {
	separator := strings.LastIndexByte(path, '/')
	if separator < 0 {
		return ""
	}
	return path[:separator]
}

func receiptKey(admission transfer.DirectoryAdmission) admissionReceiptKey {
	var key admissionReceiptKey
	copy(key[:], admission.Bytes())
	return key
}

func sameAdmission(left, right transfer.DirectoryAdmission) bool {
	return left.Equal(right) && left.SchemaVersion() == right.SchemaVersion() &&
		left.ReceiveIntentDigest() == right.ReceiveIntentDigest() && left.DirectoryID() == right.DirectoryID() &&
		left.Generation() == right.Generation() && left.Path() == right.Path() &&
		left.ModifiedTime() == right.ModifiedTime() && string(left.ParentToken()) == string(right.ParentToken())
}

func (entry *directoryEntry) notifyLocked() {
	close(entry.changed)
	entry.changed = make(chan struct{})
}

func (session *Session) adjustActiveAncestorsLocked(parent ClaimID, add bool) error {
	visited := uint64(0)
	for parent != 0 {
		entry := session.directoryClaims[parent]
		if entry == nil {
			return ErrExecutorContract
		}
		if add {
			entry.activeDescendants++
		} else {
			if entry.activeDescendants == 0 {
				return ErrExecutorContract
			}
			entry.activeDescendants--
		}
		entry.notifyLocked()
		parent = entry.claim.parent
		visited++
		if visited > uint64(len(session.directoryClaims)) {
			return ErrExecutorContract
		}
	}
	return nil
}

func (session *Session) releaseLedgerLocked() {
	// Claim authority is retained through settlement, not beyond it. Releasing
	// the maps here bounds the lifetime of paths, receipts, and the HMAC key even
	// when a caller retains the closed Session solely for idempotent close retry.
	session.secret = [sha256.Size]byte{}
	session.rootClaim = 0
	session.metadataBytes = 0
	session.activeFiles = 0
	session.fileSlots = 0
	session.directoryClaims = nil
	session.fileClaims = nil
	session.nodeClaims = nil
	session.pathClaims = nil
	session.locatorClaims = nil
	session.nameClaims = nil
	session.receiptClaims = nil
}

func (session *Session) markInvariantFailureLocked() error {
	value := fault.DependencyContractFault()
	session.requirePauseLocked(value, true)
	return fault.Wrap(value, ErrExecutorContract)
}
