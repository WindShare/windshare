package output

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

// Operation owns an active task lock, journal, and top-level destination root
// for one receive intent.
type Operation struct {
	authority *Authority
	intent    transfer.ReceiveIntent
	opDirName string
	opDir     *Dir
	resultDir *Dir // container root for CatalogRoot/SingleFile; reserved dir for ResultRoot
	ownsRes   bool // true when resultDir is distinct from authority.root and must be closed
	taskLock  *FileLock
	journal   *taskJournal

	summaryMu       sync.Mutex
	nameAdjustments NameAdjustmentSummary

	opened    bool
	completed bool
	closed    bool
}

func (a *Authority) CreateOperation(
	ctx context.Context,
	lookup ActiveLookup,
	artifact receivecontract.ArtifactSpec,
) (_ *Operation, resultErr error) {
	defer func() {
		if resultErr != nil && !isAdmissionOutcomeError(resultErr) {
			resultErr = DiagnoseFailure(FailureCreate, resultErr)
		}
	}()
	if a == nil || ctx == nil || artifact.IsZero() {
		return nil, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.stage != stageLookupHeld ||
		lookup.authority != a || lookup.kind != LookupCreateReady ||
		lookup.admission == nil || lookup.admission != a.admission {
		return nil, transfer.ErrInvalidOutputBinding
	}
	layout, ok := artifact.DirectoryTree()
	if !ok {
		return nil, transfer.ErrInvalidOutputBinding
	}

	op, claimed, err := a.createOrClaimUnderDirLock(ctx, artifact, layout)
	if err != nil {
		return nil, err
	}
	if op != nil {
		return op, nil
	}
	return a.finishRecheckedReopenLocked(ctx, claimed)
}

func (a *Authority) createOrClaimUnderDirLock(
	ctx context.Context,
	artifact receivecontract.ArtifactSpec,
	layout receivecontract.DirectoryTreeLayout,
) (*Operation, claimedTask, error) {
	dirLock, err := acquireDirLock(a.root)
	if err != nil {
		return nil, claimedTask{}, wrapOutputFault(ctx, transferfault.OutputOwnership, "lock destination directory", err)
	}
	defer dirLock.Close()

	ctrlDir, err := ensureControlDir(a.root)
	if err != nil {
		return nil, claimedTask{}, wrapOutputFault(ctx, transferfault.OutputOwnership, "prepare control directory", err)
	}
	defer ctrlDir.Close()

	// SelectOperation releases dirLock before network shape resolution, so an
	// automatic admission must re-check uniqueness under dirLock before creating
	// a new task. Explicit --redownload intentionally creates a separate task.
	if !a.admission.redownload {
		claimed, recheckErr := a.recheckAutomaticCreateUnderDirLock(ctx, ctrlDir, a.admission.selection)
		if recheckErr != nil {
			return nil, claimedTask{}, recheckErr
		}
		if claimed.taskLock != nil {
			return nil, claimed, nil
		}
	}

	opID, err := randomOperationID(a.random)
	if err != nil {
		return nil, claimedTask{}, err
	}
	resID, err := randomReservationID(a.random)
	if err != nil {
		return nil, claimedTask{}, err
	}

	authorityRef, err := ensureAuthorityRef(ctrlDir, a.random)
	if err != nil {
		return nil, claimedTask{}, wrapOutputFault(ctx, transferfault.OutputOwnership, "load authority ref", err)
	}

	// Reserve top-level destination entry while holding the short control dir lock.
	reservation, resDir, ownsRes, err := a.reserveTopLevelLocked(opID, resID, artifact, layout, authorityRef, ctrlDir)
	if err != nil {
		return nil, claimedTask{}, wrapOutputFault(ctx, transferfault.OutputOwnership, "reserve destination entry", err)
	}
	rollbackReservation := func() {
		if ownsRes {
			_ = resDir.Close()
			_ = removeChild(a.root, reservation.PhysicalName(), true)
		}
	}
	plan, err := receivecontract.NewDirectTreePlan(artifact, reservation)
	if err != nil {
		rollbackReservation()
		return nil, claimedTask{}, err
	}
	intent, err := transfer.NewReceiveIntent(a.admission.selection, artifact, plan)
	if err != nil {
		rollbackReservation()
		return nil, claimedTask{}, err
	}

	op, err := a.initializeOperationDirLocked(ctx, ctrlDir, intent, resDir, ownsRes)
	if err != nil {
		rollbackReservation()
		return nil, claimedTask{}, err
	}
	a.admission = nil
	a.operation = op
	a.stage = stageOperationReady
	return op, claimedTask{}, nil
}

func (a *Authority) recheckAutomaticCreateUnderDirLock(
	ctx context.Context,
	ctrlDir *Dir,
	selection transfer.SelectionSpec,
) (claimedTask, error) {
	matches, err := a.findAutomaticMatchesLocked(ctrlDir, selection.Digest(), "create_operation", FailureCreate)
	if err != nil {
		return claimedTask{}, wrapOutputFault(ctx, transferfault.OutputOwnership, "list control directory", err)
	}
	switch len(matches) {
	case 0:
		return claimedTask{}, nil
	case 1:
		claimed, early, err := a.claimOpUnderDirLock(ctx, ctrlDir, matches[0], false)
		if err != nil {
			a.admission = nil
			a.stage = stageStopped
			return claimedTask{}, err
		}
		if early.kind == LookupAlreadyRunning {
			a.admission = nil
			emitTrace(a.diagnostics, traceEvent{
				component: "session",
				operation: "create_operation",
				decision:  "already_running",
				bindingID: a.bindingID,
			})
			return claimedTask{}, ErrAlreadyRunning
		}
		if early.kind != 0 {
			a.admission = nil
			a.stage = stageStopped
			return claimedTask{}, transfer.ErrInvalidOutputBinding
		}
		return claimed, nil
	default:
		a.admission = nil
		a.stage = stageStopped
		emitTrace(a.diagnostics, traceEvent{
			component: "session",
			operation: "create_operation",
			decision:  "ambiguous",
			bindingID: a.bindingID,
		})
		return claimedTask{}, ErrAmbiguous
	}
}

func (a *Authority) finishRecheckedReopenLocked(
	ctx context.Context,
	claimed claimedTask,
) (*Operation, error) {
	selection := a.admission.selection
	reopened, err := a.reopenClaimedLocked(ctx, selection, claimed, false)
	a.admission = nil
	if err != nil {
		a.stage = stageStopped
		return nil, err
	}
	switch reopened.kind {
	case LookupReopened:
		emitTrace(a.diagnostics, traceEvent{
			component:    "session",
			operation:    "create_operation",
			decision:     "reopened",
			bindingID:    a.bindingID,
			operationID:  reopened.operation.intent.OperationID(),
			intentDigest: reopened.operation.intent.Digest(),
		})
		return reopened.operation, nil
	case LookupNeedsAttention:
		emitTrace(a.diagnostics, traceEvent{
			component: "session",
			operation: "create_operation",
			decision:  "needs_attention",
			bindingID: a.bindingID,
		})
		return nil, &AttentionError{Reason: reopened.attention}
	default:
		a.stage = stageStopped
		return nil, transfer.ErrInvalidOutputBinding
	}
}

func (a *Authority) initializeOperationDirLocked(
	ctx context.Context,
	ctrlDir *Dir,
	intent transfer.ReceiveIntent,
	resDir *Dir,
	ownsRes bool,
) (*Operation, error) {
	opDirName := hex.EncodeToString(intent.OperationID().Bytes())
	if err := mkdirChild(ctrlDir, opDirName, privateDirMode); err != nil {
		return nil, wrapOutputFault(ctx, transferfault.OutputOwnership, "create operation directory", err)
	}
	opDir, err := openChildDir(ctrlDir, opDirName)
	if err != nil {
		_ = removeOperationDirectory(ctrlDir, opDirName)
		return nil, wrapOutputFault(ctx, transferfault.OutputOwnership, "open operation directory", err)
	}
	taskLock, _, err := acquireLockFile(opDir, taskLockFileName, false)
	if err != nil {
		_ = opDir.Close()
		_ = removeOperationDirectory(ctrlDir, opDirName)
		return nil, wrapOutputFault(ctx, transferfault.OutputOwnership, "lock new operation", err)
	}
	meta := taskMeta{
		Intent:          intent,
		SelectionDigest: a.admission.selection.Digest(),
		Redownload:      a.admission.redownload,
		Lifecycle:       journalLifecycleActive,
		Attention:       AttentionNone,
		StateGen:        1,
	}
	journal, err := createTaskJournal(opDir, meta, a.now)
	if err != nil {
		_ = errors.Join(taskLock.Close(), opDir.Close())
		_ = removeOperationDirectory(ctrlDir, opDirName)
		return nil, wrapOutputFault(ctx, transferfault.OutputOwnership, "create task journal", err)
	}
	_ = ctrlDir.Sync()

	op := &Operation{
		authority: a,
		intent:    intent,
		opDirName: opDirName,
		opDir:     opDir,
		resultDir: resDir,
		ownsRes:   ownsRes,
		taskLock:  taskLock,
		journal:   journal,
	}
	op.refreshNameAdjustments()
	return op, nil
}

func (a *Authority) reserveTopLevelLocked(
	opID receivecontract.OperationID,
	resID receivecontract.DestinationReservationID,
	artifact receivecontract.ArtifactSpec,
	layout receivecontract.DirectoryTreeLayout,
	authorityRef receivecontract.AuthorityRef,
	ctrlDir *Dir,
) (receivecontract.DestinationReservation, *Dir, bool, error) {
	switch layout.Kind() {
	case receivecontract.DirectoryTreeCatalogRoot:
		res, err := receivecontract.NewNativeContainerRootReservation(opID, resID, artifact, authorityRef)
		if err != nil {
			return receivecontract.DestinationReservation{}, nil, false, err
		}
		return res, a.root, false, nil
	case receivecontract.DirectoryTreeResultRoot:
		rootLayout, ok := layout.ResultRoot()
		if !ok {
			return receivecontract.DestinationReservation{}, nil, false, transfer.ErrInvalidOutputBinding
		}
		return a.reserveNamedEntryLocked(
			opID, resID, artifact, authorityRef, ctrlDir,
			receivecontract.ContainerEntryResultRoot, rootLayout.Name(),
		)
	case receivecontract.DirectoryTreeSingleFile:
		single, ok := layout.SingleFile()
		if !ok {
			return receivecontract.DestinationReservation{}, nil, false, transfer.ErrInvalidOutputBinding
		}
		return a.reserveNamedEntryLocked(
			opID, resID, artifact, authorityRef, ctrlDir,
			receivecontract.ContainerEntrySingleFile, single.SuggestedName,
		)
	default:
		return receivecontract.DestinationReservation{}, nil, false, transfer.ErrInvalidOutputBinding
	}
}

func (a *Authority) reserveNamedEntryLocked(
	opID receivecontract.OperationID,
	resID receivecontract.DestinationReservationID,
	artifact receivecontract.ArtifactSpec,
	authorityRef receivecontract.AuthorityRef,
	ctrlDir *Dir,
	entryKind receivecontract.ContainerEntryKind,
	requestedName string,
) (receivecontract.DestinationReservation, *Dir, bool, error) {
	claimed, err := a.existingTopLevelClaimsLocked(ctrlDir)
	if err != nil {
		return receivecontract.DestinationReservation{}, nil, false, err
	}
	fileLike := entryKind == receivecontract.ContainerEntrySingleFile
	traceComponent := "directory"
	if fileLike {
		traceComponent = "file"
	}
	for idx := range uint32(maxReservationProbes) {
		logical, err := receivecontract.CollisionName(opID, requestedName, idx, fileLike)
		if err != nil {
			return receivecontract.DestinationReservation{}, nil, false, err
		}
		physical, repaired, err := sanitizeTopLevelComponent(opID, logical, a.root)
		if err != nil {
			return receivecontract.DestinationReservation{}, nil, false, err
		}
		if _, taken := claimed[catalog.SiblingNameKey(physical)]; taken {
			continue
		}
		resDir, ownsRes, collided, err := a.claimPublicEntryLocked(entryKind, physical)
		if err != nil {
			return receivecontract.DestinationReservation{}, nil, false, err
		}
		if collided {
			continue
		}
		res, err := receivecontract.NewNativeNamedEntryReservation(opID, resID, artifact, authorityRef, logical, physical, idx)
		if err != nil {
			if ownsRes {
				_ = resDir.Close()
				_ = removeChild(a.root, physical, true)
			}
			return receivecontract.DestinationReservation{}, nil, false, err
		}
		if repaired {
			emitTrace(a.diagnostics, traceEvent{
				component:    traceComponent,
				operation:    "map_name",
				decision:     "admitted",
				operationID:  opID,
				logicalPath:  logical,
				physicalPath: physical,
			})
		}
		return res, resDir, ownsRes, nil
	}
	return receivecontract.DestinationReservation{}, nil, false, ErrNamespaceCollision
}

func (a *Authority) claimPublicEntryLocked(
	entryKind receivecontract.ContainerEntryKind,
	physical string,
) (resDir *Dir, ownsRes bool, collided bool, err error) {
	switch entryKind {
	case receivecontract.ContainerEntryResultRoot:
		err = mkdirChild(a.root, physical, publicDirMode)
		if errors.Is(err, ErrNamespaceCollision) || errors.Is(err, fs.ErrExist) {
			return nil, false, true, nil
		}
		if err != nil {
			return nil, false, false, err
		}
		resDir, err = openChildDir(a.root, physical)
		if err != nil {
			_ = removeChild(a.root, physical, true)
			return nil, false, false, err
		}
		return resDir, true, false, nil
	case receivecontract.ContainerEntrySingleFile:
		kind, _, err := lstatChild(a.root, physical)
		if err != nil {
			return nil, false, false, err
		}
		if kind != EntryAbsent {
			return nil, false, true, nil
		}
		return a.root, false, false, nil
	default:
		return nil, false, false, transfer.ErrInvalidOutputBinding
	}
}

// existingTopLevelClaimsLocked collects normalized container-root physical names
// held by unfinished operations in ctrlDir. Single files stay staged privately
// until commit, and a paused result root may have been removed from disk before
// resume; checking every retained reservation prevents another file or folder
// task from claiming the same top-level name (including case-folded aliases on
// Windows/macOS).
func (a *Authority) existingTopLevelClaimsLocked(ctrlDir *Dir) (map[string]struct{}, error) {
	claimed := make(map[string]struct{})
	err := a.scanOperationHeadersLocked(ctrlDir, "create_operation", FailureCreate, func(_ string, header taskHeader) {
		if res, ok := header.Intent.MaterializationPlan().DestinationReservation(); ok &&
			res.Kind() == receivecontract.ReservationNamedContainerEntry {
			claimed[catalog.SiblingNameKey(res.PhysicalName())] = struct{}{}
		}
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

func (op *Operation) ReceiveIntent() (transfer.ReceiveIntent, bool) {
	if op == nil || op.closed || op.intent.IsZero() {
		return transfer.ReceiveIntent{}, false
	}
	return op.intent, true
}

func (op *Operation) Idle() bool {
	if op == nil || op.authority == nil {
		return false
	}
	op.authority.mu.Lock()
	defer op.authority.mu.Unlock()
	return !op.authority.closed && op.authority.operation == op && !op.closed && !op.completed && !op.opened
}

func (op *Operation) RecordShareStopped(ctx context.Context) error {
	if op == nil || op.authority == nil || ctx == nil {
		return transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	op.authority.mu.Lock()
	defer op.authority.mu.Unlock()
	if op.authority.closed || op.authority.operation != op || op.closed || op.completed || op.opened || op.journal == nil {
		return transfer.ErrInvalidOutputBinding
	}
	if op.journal.meta().Lifecycle == journalLifecycleStopped {
		return nil
	}
	return op.journal.recordLifecycle(journalLifecycleStopped, AttentionNone)
}

func (op *Operation) Discard(ctx context.Context) error {
	if op == nil || op.authority == nil || ctx == nil {
		return transfer.ErrInvalidOutputBinding
	}
	a := op.authority
	a.mu.Lock()
	if a.closed || a.operation != op || op.closed || op.completed || op.opened || a.root == nil {
		a.mu.Unlock()
		return transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		a.mu.Unlock()
		return err
	}
	a.closed = true
	a.operation = nil
	a.admission = nil
	root := a.root
	a.root = nil
	a.stage = stageStopped
	opDirName := op.opDirName
	a.mu.Unlock()

	// Acquire the directory lock before closing op so task.lock is never
	// released ahead of directory removal.
	retireErr := retireControlDir(root, opDirName, op.closeResources)
	if retireErr != nil {
		nativeClass, _ := ClassifyNativeError(retireErr)
		emitTrace(a.diagnostics, traceEvent{
			component:    "session",
			operation:    "discard_operation",
			decision:     "cleanup_pending",
			failed:       true,
			failureStage: FailureClose,
			nativeClass:  nativeClass,
			bindingID:    a.bindingID,
			operationID:  op.intent.OperationID(),
			intentDigest: op.intent.Digest(),
		})
	}
	rootErr := root.Close()
	if err := errors.Join(retireErr, rootErr); err != nil {
		return DiagnoseFailure(FailureClose, err)
	}
	return nil
}

func (op *Operation) closeResources() error {
	if op == nil || op.closed {
		return nil
	}
	op.refreshNameAdjustments()
	op.closed = true
	var err error
	if op.ownsRes && op.resultDir != nil {
		err = errors.Join(err, op.resultDir.Close())
		op.resultDir = nil
	}
	if op.journal != nil {
		err = errors.Join(err, op.journal.close())
		op.journal = nil
	}
	if op.taskLock != nil {
		err = errors.Join(err, op.taskLock.Close())
		op.taskLock = nil
	}
	if op.opDir != nil {
		err = errors.Join(err, op.opDir.Close())
		op.opDir = nil
	}
	return err
}

// retireControlDir deletes one completed or discarded operation directory and
// removes .windshare-tmp altogether when no other operations remain.
//
// When beforeRemove is non-nil, it runs after acquiring the directory lock and
// before removing opDirName so an active owner can release task.lock and open
// handles (required on Windows before directory deletion) without exposing a
// gap where another instance can reopen opDirName. Acquiring the blocking
// directory lock while holding task.lock is deadlock-free because every
// task.lock acquisition in this package is non-blocking.
func retireControlDir(root *Dir, opDirName string, beforeRemove func() error) error {
	dirLock, lockErr := acquireDirLock(root)
	if lockErr != nil {
		var releaseErr error
		if beforeRemove != nil {
			releaseErr = beforeRemove()
		}
		return errors.Join(lockErr, releaseErr)
	}
	ctrlDir, err := openChildDir(root, ControlDirName)
	if errors.Is(err, fs.ErrNotExist) {
		var releaseErr error
		if beforeRemove != nil {
			releaseErr = beforeRemove()
		}
		return errors.Join(releaseErr, dirLock.Close())
	}
	if err != nil {
		var releaseErr error
		if beforeRemove != nil {
			releaseErr = beforeRemove()
		}
		return errors.Join(err, releaseErr, dirLock.Close())
	}
	if beforeRemove != nil {
		if releaseErr := beforeRemove(); releaseErr != nil {
			return errors.Join(releaseErr, ctrlDir.Close(), dirLock.Close())
		}
	}
	removeErr, closeErr := removeOperationLocked(root, ctrlDir, dirLock, opDirName)
	return errors.Join(removeErr, closeErr)
}

// removeOperationLocked deletes opDirName while holding ctrlDir and dirLock,
// removes .windshare-tmp while still holding dirLock when no other operations
// remain, and always closes ctrlDir before releasing dirLock.
func removeOperationLocked(root, ctrlDir *Dir, dirLock *DirLock, opDirName string) (removeErr, closeErr error) {
	removeErr = removeOperationDirectory(ctrlDir, opDirName)
	if removeErr != nil {
		// Record cleanup-pending only on the failure path so normal completion
		// and discard pay zero extra journal writes or fsyncs, while subsequent
		// list/reopen calls see that retirement already started.
		markOperationCleanupPending(ctrlDir, opDirName)
		return removeErr, errors.Join(ctrlDir.Close(), dirLock.Close())
	}
	names, listErr := readDirNames(ctrlDir)
	empty := listErr == nil
	for _, name := range names {
		if name != authorityIDFile {
			empty = false
			break
		}
	}
	if empty {
		// Close ctrlDir before unlinking .windshare-tmp (required on Windows)
		// while keeping dirLock held across removal so concurrent receivers
		// never observe a half-removed control directory.
		_ = removeChild(ctrlDir, authorityIDFile, false)
		ctrlCloseErr := ctrlDir.Close()
		_ = removeChild(root, ControlDirName, true)
		return nil, errors.Join(ctrlCloseErr, dirLock.Close())
	}
	return nil, errors.Join(ctrlDir.Close(), dirLock.Close())
}

func markOperationCleanupPending(ctrlDir *Dir, opDirName string) {
	opDir, err := openChildDir(ctrlDir, opDirName)
	if err != nil {
		return
	}
	defer opDir.Close()
	journal, err := openTaskJournal(opDir, nil)
	if err != nil {
		return
	}
	if journal.meta().Lifecycle != journalLifecycleCleanupPending {
		_ = journal.recordLifecycle(journalLifecycleCleanupPending, AttentionCleanup)
	}
	_ = journal.close()
}

func ensureControlDir(root *Dir) (*Dir, error) {
	if err := mkdirChild(root, ControlDirName, privateDirMode); err != nil &&
		!errors.Is(err, ErrNamespaceCollision) && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	return openChildDir(root, ControlDirName)
}

func ensureAuthorityRef(ctrlDir *Dir, random io.Reader) (receivecontract.AuthorityRef, error) {
	if f, err := openChildFile(ctrlDir, authorityIDFile, os.O_RDONLY, 0); err == nil {
		raw, readErr := io.ReadAll(io.LimitReader(f, receivecontract.AuthorityRefBytes+1))
		_ = f.Close()
		if readErr == nil && len(raw) == receivecontract.AuthorityRefBytes {
			if ref, err := receivecontract.AuthorityRefFromBytes(raw); err == nil {
				return ref, nil
			}
		}
	}
	for range maxIdentityAttempts {
		var raw [receivecontract.AuthorityRefBytes]byte
		if _, err := io.ReadFull(random, raw[:]); err != nil {
			return receivecontract.AuthorityRef{}, err
		}
		ref, err := receivecontract.AuthorityRefFromBytes(raw[:])
		if err != nil {
			continue
		}
		f, err := openChildFile(ctrlDir, authorityIDFile, os.O_RDWR|os.O_CREATE|os.O_TRUNC, privateFileMode)
		if err != nil {
			return receivecontract.AuthorityRef{}, err
		}
		_, writeErr := f.Write(ref.Bytes())
		syncErr := f.Sync()
		closeErr := f.Close()
		if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
			return receivecontract.AuthorityRef{}, err
		}
		return ref, nil
	}
	return receivecontract.AuthorityRef{}, transfer.ErrInvalidOutputBinding
}

func randomOperationID(random io.Reader) (receivecontract.OperationID, error) {
	for range maxIdentityAttempts {
		var raw [receivecontract.StableIdentityBytes]byte
		if _, err := io.ReadFull(random, raw[:]); err != nil {
			return receivecontract.OperationID{}, err
		}
		if id, err := receivecontract.OperationIDFromBytes(raw[:]); err == nil {
			return id, nil
		}
	}
	return receivecontract.OperationID{}, transfer.ErrInvalidOutputBinding
}

func randomReservationID(random io.Reader) (receivecontract.DestinationReservationID, error) {
	for range maxIdentityAttempts {
		var raw [receivecontract.StableIdentityBytes]byte
		if _, err := io.ReadFull(random, raw[:]); err != nil {
			return receivecontract.DestinationReservationID{}, err
		}
		if id, err := receivecontract.DestinationReservationIDFromBytes(raw[:]); err == nil {
			return id, nil
		}
	}
	return receivecontract.DestinationReservationID{}, transfer.ErrInvalidOutputBinding
}

func randomSessionID(random io.Reader) (transfer.OutputSessionID, error) {
	for range maxIdentityAttempts {
		var raw [transfer.OutputSessionIdentityBytes]byte
		if _, err := io.ReadFull(random, raw[:]); err != nil {
			return transfer.OutputSessionID{}, err
		}
		if id, err := transfer.OutputSessionIDFromBytes(raw[:]); err == nil {
			return id, nil
		}
	}
	return transfer.OutputSessionID{}, transfer.ErrInvalidOutputBinding
}
