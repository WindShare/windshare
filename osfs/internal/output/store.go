package output

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"sync"
	"time"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const (
	maxIdentityAttempts  = 8
	maxReservationProbes = 1024
)

type LookupKind uint8

const (
	LookupCreateReady LookupKind = iota + 1
	LookupNotFound
	LookupReopened
	LookupAlreadyRunning
	LookupNeedsAttention
	LookupAmbiguous
)

type Config struct {
	RootPath    string
	CreateRoot  bool
	Diagnostics diagnostics.Sink
}

type authorityStage uint8

const (
	stageConstructed authorityStage = iota + 1
	stageBound
	stageLookupHeld
	stageOperationReady
	stageStopped
)

// Authority owns one caller-selected destination container across destination
// binding, operation lookup/creation, and active receive session lifecycle.
type Authority struct {
	rootPath    string
	createRoot  bool
	diagnostics diagnostics.Sink
	random      io.Reader
	now         func() time.Time
	openJournal func(*Dir, func() time.Time) (*taskJournal, error)

	mu          sync.Mutex
	stage       authorityStage
	closed      bool
	root        *Dir
	disposition RootDisposition
	caps        DestinationCapabilities
	bindingID   [32]byte
	admission   *pendingAdmission
	operation   *Operation
}

// claimedTask holds an opened operation directory and its exclusive
// non-blocking task.lock acquired under dirLock so full journal replay can
// proceed under task.lock after dirLock is released.
type claimedTask struct {
	opDirName string
	opDir     *Dir
	taskLock  *FileLock
}

type pendingAdmission struct {
	selection  transfer.SelectionSpec
	redownload bool
}

type ActiveLookup struct {
	authority *Authority
	kind      LookupKind
	admission *pendingAdmission
	operation *Operation
	attention AttentionReason
}

func (l ActiveLookup) Kind() LookupKind { return l.kind }
func (l ActiveLookup) Attention() AttentionReason {
	if l.kind != LookupNeedsAttention {
		return AttentionNone
	}
	return l.attention
}
func (l ActiveLookup) Operation() *Operation {
	if l.kind != LookupReopened {
		return nil
	}
	return l.operation
}

func New(cfg Config) (*Authority, error) {
	return &Authority{
		rootPath:    cfg.RootPath,
		createRoot:  cfg.CreateRoot,
		diagnostics: cfg.Diagnostics,
		random:      rand.Reader,
		now:         time.Now,
		openJournal: openTaskJournal,
		stage:       stageConstructed,
	}, nil
}

func (a *Authority) BindDestination(ctx context.Context) (resultErr error) {
	defer func() {
		resultErr = DiagnoseFailure(FailureDestinationBinding, resultErr)
	}()
	if a == nil || ctx == nil {
		return transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.stage != stageConstructed {
		return transfer.ErrInvalidOutputBinding
	}
	root, disposition, err := OpenContainer(a.rootPath, a.createRoot)
	if err != nil {
		return wrapOutputFault(ctx, transferfault.OutputOwnership, "open destination container", err)
	}
	caps, err := ProbeDestination(root)
	if err != nil {
		return freezeFailure(
			wrapOutputFault(ctx, transferfault.OutputOwnership, "probe destination capabilities", err),
			root.Close(),
		)
	}
	if err := caps.ValidateSupport(); err != nil {
		return freezeFailure(
			wrapOutputFault(ctx, transferfault.OutputUnsupportedFilesystem, "destination lacks required primitives", err),
			root.Close(),
		)
	}
	bindingID := sha256.Sum256([]byte("windshare/destination-binding/v1\x00" + root.path))
	a.root = root
	a.disposition = disposition
	a.caps = caps
	a.bindingID = bindingID
	a.stage = stageBound

	emitTrace(a.diagnostics, traceEvent{
		component:    "session",
		operation:    "admit_destination",
		decision:     "admitted",
		bindingID:    bindingID,
		capabilities: caps,
	})
	return nil
}

func (a *Authority) SelectOperation(
	ctx context.Context,
	selection transfer.SelectionSpec,
	selector receivecontract.OperationSelection,
) (result ActiveLookup, resultErr error) {
	defer func() {
		if resultErr != nil && !errors.Is(resultErr, ErrShareStopped) {
			resultErr = DiagnoseFailure(FailureLookup, resultErr)
		}
	}()
	if a == nil || ctx == nil || selection.IsZero() || !selector.Valid() {
		return ActiveLookup{}, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return ActiveLookup{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.stage != stageBound || a.root == nil {
		return ActiveLookup{}, transfer.ErrInvalidOutputBinding
	}

	switch selector.Kind() {
	case receivecontract.OperationRedownload:
		adm := &pendingAdmission{selection: selection, redownload: true}
		a.admission = adm
		a.stage = stageLookupHeld
		return ActiveLookup{authority: a, kind: LookupCreateReady, admission: adm}, nil

	case receivecontract.OperationByID:
		return a.selectByIDLocked(ctx, selection, selector.ID())

	case receivecontract.OperationAutomatic:
		return a.selectAutomaticLocked(ctx, selection)

	default:
		return ActiveLookup{}, transfer.ErrInvalidOutputBinding
	}
}

func (a *Authority) selectByIDLocked(
	ctx context.Context,
	selection transfer.SelectionSpec,
	opID receivecontract.OperationID,
) (ActiveLookup, error) {
	opDirName := hex.EncodeToString(opID.Bytes())
	claimed, early, err := a.claimByIDUnderDirLock(ctx, opDirName)
	if err != nil || early.kind != 0 {
		return early, err
	}
	return a.reopenClaimedLocked(ctx, selection, claimed, true)
}

func (a *Authority) claimByIDUnderDirLock(
	ctx context.Context,
	opDirName string,
) (claimedTask, ActiveLookup, error) {
	dirLock, err := acquireDirLock(a.root)
	if err != nil {
		return claimedTask{}, ActiveLookup{}, wrapOutputFault(ctx, transferfault.OutputOwnership, "lock destination directory", err)
	}
	defer dirLock.Close()

	ctrlDir, err := openChildDir(a.root, ControlDirName)
	if errors.Is(err, fs.ErrNotExist) {
		a.stage = stageStopped
		return claimedTask{}, ActiveLookup{authority: a, kind: LookupNotFound}, nil
	}
	if err != nil {
		return claimedTask{}, ActiveLookup{}, wrapOutputFault(ctx, transferfault.OutputOwnership, "open control directory", err)
	}
	defer ctrlDir.Close()

	return a.claimOpUnderDirLock(ctx, ctrlDir, opDirName, true)
}

func (a *Authority) selectAutomaticLocked(
	ctx context.Context,
	selection transfer.SelectionSpec,
) (ActiveLookup, error) {
	claimed, early, err := a.claimAutomaticUnderDirLock(ctx, selection)
	if err != nil || early.kind != 0 {
		return early, err
	}
	return a.reopenClaimedLocked(ctx, selection, claimed, false)
}

func (a *Authority) claimAutomaticUnderDirLock(
	ctx context.Context,
	selection transfer.SelectionSpec,
) (claimedTask, ActiveLookup, error) {
	dirLock, err := acquireDirLock(a.root)
	if err != nil {
		return claimedTask{}, ActiveLookup{}, wrapOutputFault(ctx, transferfault.OutputOwnership, "lock destination directory", err)
	}
	defer dirLock.Close()

	ctrlDir, err := openChildDir(a.root, ControlDirName)
	if errors.Is(err, fs.ErrNotExist) {
		adm := &pendingAdmission{selection: selection}
		a.admission = adm
		a.stage = stageLookupHeld
		return claimedTask{}, ActiveLookup{authority: a, kind: LookupCreateReady, admission: adm}, nil
	}
	if err != nil {
		return claimedTask{}, ActiveLookup{}, wrapOutputFault(ctx, transferfault.OutputOwnership, "open control directory", err)
	}
	defer ctrlDir.Close()

	matches, err := findAutomaticMatchesLocked(ctrlDir, selection.Digest())
	if err != nil {
		return claimedTask{}, ActiveLookup{}, wrapOutputFault(ctx, transferfault.OutputOwnership, "list control directory", err)
	}
	switch len(matches) {
	case 0:
		adm := &pendingAdmission{selection: selection}
		a.admission = adm
		a.stage = stageLookupHeld
		return claimedTask{}, ActiveLookup{authority: a, kind: LookupCreateReady, admission: adm}, nil
	case 1:
		return a.claimOpUnderDirLock(ctx, ctrlDir, matches[0], false)
	default:
		a.stage = stageStopped
		return claimedTask{}, ActiveLookup{authority: a, kind: LookupAmbiguous}, nil
	}
}

// findAutomaticMatchesLocked scans non-redownload operations matching wantDigest.
// Callers must hold dirLock on ctrlDir so automatic lookup and creation enforce
// the same uniqueness rule.
func findAutomaticMatchesLocked(
	ctrlDir *Dir,
	wantDigest [transfer.SelectionSpecDigestBytes]byte,
) ([]string, error) {
	names, err := readDirNames(ctrlDir)
	if err != nil {
		return nil, err
	}
	var matches []string
	for _, name := range names {
		if _, ok := isValidOpDirName(name); !ok {
			continue
		}
		opDir, err := openChildDir(ctrlDir, name)
		if err != nil {
			continue
		}
		header, readErr := readJournalHeader(opDir)
		_ = opDir.Close()
		if readErr != nil || header.Redownload {
			continue
		}
		if header.SelectionDigest == wantDigest {
			matches = append(matches, name)
		}
	}
	return matches, nil
}

// claimOpUnderDirLock opens opDirName and acquires its non-blocking task.lock
// while dirLock prevents concurrent retirement. Callers release ctrlDir and
// dirLock before replaying state.log so per-task I/O never blocks unrelated
// tasks sharing the destination container.
func (a *Authority) claimOpUnderDirLock(
	ctx context.Context,
	ctrlDir *Dir,
	opDirName string,
	allowNotFound bool,
) (claimedTask, ActiveLookup, error) {
	opDir, err := openChildDir(ctrlDir, opDirName)
	if allowNotFound && errors.Is(err, fs.ErrNotExist) {
		a.stage = stageStopped
		return claimedTask{}, ActiveLookup{authority: a, kind: LookupNotFound}, nil
	}
	if err != nil {
		opLabel := "open matched operation"
		if allowNotFound {
			opLabel = "open operation directory"
		}
		return claimedTask{}, ActiveLookup{}, wrapOutputFault(ctx, transferfault.OutputOwnership, opLabel, err)
	}
	taskLock, _, lockErr := acquireLockFile(opDir, taskLockFileName, true)
	if errors.Is(lockErr, fs.ErrNotExist) {
		// Do not create task.lock inside an already-emptied operation directory
		// whose final rmdir failed; creating a lock file there would turn a
		// retryable empty cleanup-pending directory into a non-empty unknown entry.
		if empty, emptyErr := isEmptyDir(opDir); emptyErr == nil && empty {
			_ = opDir.Close()
			a.stage = stageStopped
			opID, _ := isValidOpDirName(opDirName)
			emitTrace(a.diagnostics, traceEvent{
				component:   "session",
				operation:   "select_operation",
				decision:    "cleanup_pending",
				bindingID:   a.bindingID,
				operationID: opID,
			})
			return claimedTask{}, ActiveLookup{authority: a, kind: LookupNeedsAttention, attention: AttentionCleanup}, nil
		}
		taskLock, _, lockErr = acquireLockFile(opDir, taskLockFileName, false)
	}
	if errors.Is(lockErr, ErrNamespaceLockBusy) {
		_ = opDir.Close()
		a.stage = stageStopped
		return claimedTask{}, ActiveLookup{authority: a, kind: LookupAlreadyRunning}, nil
	}
	if lockErr != nil {
		_ = opDir.Close()
		return claimedTask{}, ActiveLookup{}, wrapOutputFault(ctx, transferfault.OutputOwnership, "acquire task lock", lockErr)
	}
	return claimedTask{
		opDirName: opDirName,
		opDir:     opDir,
		taskLock:  taskLock,
	}, ActiveLookup{}, nil
}

// reopenClaimedLocked finishes reopening an operation under a.mu and task.lock
// after dirLock has been released. Full journal replay, torn-tail truncation,
// and compaction stay scoped to the task's own lock.
func (a *Authority) reopenClaimedLocked(
	ctx context.Context,
	selection transfer.SelectionSpec,
	claimed claimedTask,
	explicitByID bool,
) (ActiveLookup, error) {
	opDirName, opDir, taskLock := claimed.opDirName, claimed.opDir, claimed.taskLock
	openJournal := a.openJournal
	if openJournal == nil {
		openJournal = openTaskJournal
	}
	journal, err := openJournal(opDir, a.now)
	if err != nil {
		_ = errors.Join(taskLock.Close(), opDir.Close())
		if errors.Is(err, errJournalCorrupt) || errors.Is(err, fs.ErrNotExist) {
			a.stage = stageStopped
			return ActiveLookup{authority: a, kind: LookupNeedsAttention, attention: AttentionOperation}, nil
		}
		return ActiveLookup{}, DiagnoseFailure(FailureOpen, wrapOutputFault(ctx, transferfault.OutputOwnership, "open task journal", err))
	}
	meta := journal.meta()
	if meta.Intent.SelectionSpec().Digest() != selection.Digest() {
		_ = errors.Join(journal.close(), taskLock.Close(), opDir.Close())
		if explicitByID {
			a.stage = stageStopped
			emitTrace(a.diagnostics, traceEvent{
				component:    "session",
				operation:    "select_operation",
				decision:     "selection_mismatch",
				operationID:  meta.Intent.OperationID(),
				intentDigest: meta.Intent.Digest(),
			})
			return ActiveLookup{authority: a, kind: LookupNotFound}, nil
		}
		return ActiveLookup{}, transfer.ErrInvalidOutputBinding
	}
	if meta.Lifecycle == journalLifecycleStopped {
		_ = errors.Join(journal.close(), taskLock.Close(), opDir.Close())
		a.stage = stageStopped
		return ActiveLookup{}, ErrShareStopped
	}
	if meta.Lifecycle == journalLifecycleCleanupPending {
		_ = errors.Join(journal.close(), taskLock.Close(), opDir.Close())
		a.stage = stageStopped
		emitTrace(a.diagnostics, traceEvent{
			component:    "session",
			operation:    "select_operation",
			decision:     "cleanup_pending",
			bindingID:    a.bindingID,
			operationID:  meta.Intent.OperationID(),
			intentDigest: meta.Intent.Digest(),
		})
		return ActiveLookup{authority: a, kind: LookupNeedsAttention, attention: AttentionCleanup}, nil
	}
	if meta.Lifecycle == journalLifecycleAttn {
		_ = errors.Join(journal.close(), taskLock.Close(), opDir.Close())
		a.stage = stageStopped
		reason := meta.Attention
		if !reason.Valid() {
			reason = AttentionOperation
		}
		return ActiveLookup{authority: a, kind: LookupNeedsAttention, attention: reason}, nil
	}

	resDir, ownsRes, err := a.reopenResultDirLocked(meta.Intent)
	if err != nil {
		_ = journal.recordLifecycle(journalLifecycleAttn, AttentionDestination)
		_ = errors.Join(journal.close(), taskLock.Close(), opDir.Close())
		a.stage = stageStopped
		return ActiveLookup{authority: a, kind: LookupNeedsAttention, attention: AttentionDestination}, nil
	}
	op := &Operation{
		authority: a,
		intent:    meta.Intent,
		opDirName: opDirName,
		opDir:     opDir,
		resultDir: resDir,
		ownsRes:   ownsRes,
		taskLock:  taskLock,
		journal:   journal,
	}
	op.refreshNameAdjustments()
	a.operation = op
	a.stage = stageOperationReady
	return ActiveLookup{authority: a, kind: LookupReopened, operation: op}, nil
}

func (a *Authority) reopenResultDirLocked(intent transfer.ReceiveIntent) (*Dir, bool, error) {
	res, ok := intent.MaterializationPlan().DestinationReservation()
	if !ok || res.IsZero() {
		return nil, false, ErrUnsafeNamespace
	}
	switch res.Kind() {
	case receivecontract.ReservationContainerRoot:
		return a.root, false, nil
	case receivecontract.ReservationNamedContainerEntry:
		if res.EntryKind() == receivecontract.ContainerEntrySingleFile {
			return a.root, false, nil
		}
		dir, err := openChildDir(a.root, res.PhysicalName())
		if err != nil {
			return nil, false, err
		}
		return dir, true, nil
	default:
		return nil, false, ErrUnsafeNamespace
	}
}

func (a *Authority) OpenOperation(ctx context.Context, op *Operation) (transfer.DirectTreeSession, error) {
	if a == nil || op == nil || ctx == nil {
		return nil, DiagnoseFailure(FailureOpen, transfer.ErrInvalidOutputBinding)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.operation != op || op.closed || op.completed || op.opened {
		return nil, DiagnoseFailure(FailureOpen, transfer.ErrInvalidOutputBinding)
	}
	session, err := newSession(a, op)
	if err != nil {
		return nil, DiagnoseFailure(FailureOpen, err)
	}
	op.opened = true
	emitTrace(a.diagnostics, traceEvent{
		component:    "session",
		operation:    "admit_destination",
		decision:     "admitted",
		bindingID:    a.bindingID,
		operationID:  op.intent.OperationID(),
		intentDigest: op.intent.Digest(),
		sessionID:    session.sessionID,
		capabilities: a.caps,
	})
	return session, nil
}

func (a *Authority) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	op := a.operation
	root := a.root
	a.operation = nil
	a.admission = nil
	a.root = nil
	a.stage = stageStopped
	a.mu.Unlock()

	var err error
	if op != nil {
		err = errors.Join(err, op.closeResources())
	}
	if root != nil {
		err = errors.Join(err, root.Close())
	}
	return DiagnoseFailure(FailureClose, err)
}
