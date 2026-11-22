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
	ctrlDir, err := openChildDir(a.root, ControlDirName)
	if errors.Is(err, fs.ErrNotExist) {
		a.stage = stageStopped
		return ActiveLookup{authority: a, kind: LookupNotFound}, nil
	}
	if err != nil {
		return ActiveLookup{}, wrapOutputFault(ctx, transferfault.OutputOwnership, "open control directory", err)
	}
	defer ctrlDir.Close()

	dirLock, _, err := acquireLockFile(ctrlDir, dirLockFileName, false, true)
	if err != nil {
		return ActiveLookup{}, wrapOutputFault(ctx, transferfault.OutputOwnership, "lock control directory", err)
	}
	defer dirLock.Close()

	opDirName := hex.EncodeToString(opID.Bytes())
	opDir, err := openChildDir(ctrlDir, opDirName)
	if errors.Is(err, fs.ErrNotExist) {
		a.stage = stageStopped
		return ActiveLookup{authority: a, kind: LookupNotFound}, nil
	}
	if err != nil {
		return ActiveLookup{}, wrapOutputFault(ctx, transferfault.OutputOwnership, "open operation directory", err)
	}

	taskLock, _, lockErr := acquireLockFile(opDir, taskLockFileName, false, false)
	if errors.Is(lockErr, ErrNamespaceLockBusy) {
		_ = opDir.Close()
		a.stage = stageStopped
		return ActiveLookup{authority: a, kind: LookupAlreadyRunning}, nil
	}
	if lockErr != nil {
		_ = opDir.Close()
		return ActiveLookup{}, wrapOutputFault(ctx, transferfault.OutputOwnership, "acquire task lock", lockErr)
	}

	return a.reopenLocked(ctx, selection, opDirName, opDir, taskLock, true)
}

func (a *Authority) selectAutomaticLocked(
	ctx context.Context,
	selection transfer.SelectionSpec,
) (ActiveLookup, error) {
	ctrlDir, err := openChildDir(a.root, ControlDirName)
	if errors.Is(err, fs.ErrNotExist) {
		adm := &pendingAdmission{selection: selection}
		a.admission = adm
		a.stage = stageLookupHeld
		return ActiveLookup{authority: a, kind: LookupCreateReady, admission: adm}, nil
	}
	if err != nil {
		return ActiveLookup{}, wrapOutputFault(ctx, transferfault.OutputOwnership, "open control directory", err)
	}
	defer ctrlDir.Close()

	dirLock, _, err := acquireLockFile(ctrlDir, dirLockFileName, false, true)
	if err != nil {
		return ActiveLookup{}, wrapOutputFault(ctx, transferfault.OutputOwnership, "lock control directory", err)
	}
	defer dirLock.Close()

	matches, err := findAutomaticMatchesLocked(ctrlDir, selection.Digest())
	if err != nil {
		return ActiveLookup{}, wrapOutputFault(ctx, transferfault.OutputOwnership, "list control directory", err)
	}
	switch len(matches) {
	case 0:
		adm := &pendingAdmission{selection: selection}
		a.admission = adm
		a.stage = stageLookupHeld
		return ActiveLookup{authority: a, kind: LookupCreateReady, admission: adm}, nil
	case 1:
		return a.reopenMatchLocked(ctx, ctrlDir, selection, matches[0])
	default:
		a.stage = stageStopped
		return ActiveLookup{authority: a, kind: LookupAmbiguous}, nil
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

func (a *Authority) reopenMatchLocked(
	ctx context.Context,
	ctrlDir *Dir,
	selection transfer.SelectionSpec,
	opDirName string,
) (ActiveLookup, error) {
	opDir, err := openChildDir(ctrlDir, opDirName)
	if err != nil {
		return ActiveLookup{}, wrapOutputFault(ctx, transferfault.OutputOwnership, "open matched operation", err)
	}
	taskLock, _, lockErr := acquireLockFile(opDir, taskLockFileName, false, false)
	if errors.Is(lockErr, ErrNamespaceLockBusy) {
		_ = opDir.Close()
		a.stage = stageStopped
		return ActiveLookup{authority: a, kind: LookupAlreadyRunning}, nil
	}
	if lockErr != nil {
		_ = opDir.Close()
		return ActiveLookup{}, wrapOutputFault(ctx, transferfault.OutputOwnership, "acquire task lock", lockErr)
	}
	return a.reopenLocked(ctx, selection, opDirName, opDir, taskLock, false)
}

func (a *Authority) reopenLocked(
	ctx context.Context,
	selection transfer.SelectionSpec,
	opDirName string,
	opDir *Dir,
	taskLock *FileLock,
	explicitByID bool,
) (ActiveLookup, error) {
	journal, err := openTaskJournal(opDir, a.now)
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
