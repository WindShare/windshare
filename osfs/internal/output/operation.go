package output

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"

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
		resultErr = DiagnoseFailure(FailureCreate, resultErr)
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
	opID, err := randomOperationID(a.random)
	if err != nil {
		return nil, err
	}
	resID, err := randomReservationID(a.random)
	if err != nil {
		return nil, err
	}

	ctrlDir, err := ensureControlDir(a.root)
	if err != nil {
		return nil, wrapOutputFault(ctx, transferfault.OutputOwnership, "prepare control directory", err)
	}
	defer ctrlDir.Close()

	dirLock, _, err := acquireLockFile(ctrlDir, dirLockFileName, false, true)
	if err != nil {
		return nil, wrapOutputFault(ctx, transferfault.OutputOwnership, "lock control directory", err)
	}
	defer dirLock.Close()

	authorityRef, err := ensureAuthorityRef(ctrlDir, a.random)
	if err != nil {
		return nil, wrapOutputFault(ctx, transferfault.OutputOwnership, "load authority ref", err)
	}

	// Reserve top-level destination entry while holding the short control dir lock.
	reservation, resDir, ownsRes, err := a.reserveTopLevelLocked(opID, resID, artifact, layout, authorityRef, ctrlDir)
	if err != nil {
		return nil, wrapOutputFault(ctx, transferfault.OutputOwnership, "reserve destination entry", err)
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
		return nil, err
	}
	intent, err := transfer.NewReceiveIntent(a.admission.selection, artifact, plan)
	if err != nil {
		rollbackReservation()
		return nil, err
	}

	op, err := a.initializeOperationDirLocked(ctx, ctrlDir, intent, resDir, ownsRes)
	if err != nil {
		rollbackReservation()
		return nil, err
	}
	a.admission = nil
	a.operation = op
	a.stage = stageOperationReady
	return op, nil
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
		_ = removeOperationDirectory(a.root, opDirName)
		return nil, wrapOutputFault(ctx, transferfault.OutputOwnership, "open operation directory", err)
	}
	taskLock, _, err := acquireLockFile(opDir, taskLockFileName, false, false)
	if err != nil {
		_ = opDir.Close()
		_ = removeOperationDirectory(a.root, opDirName)
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
		_ = removeOperationDirectory(a.root, opDirName)
		return nil, wrapOutputFault(ctx, transferfault.OutputOwnership, "create task journal", err)
	}
	_ = ctrlDir.Sync()

	return &Operation{
		authority: a,
		intent:    intent,
		opDirName: opDirName,
		opDir:     opDir,
		resultDir: resDir,
		ownsRes:   ownsRes,
		taskLock:  taskLock,
		journal:   journal,
	}, nil
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
		return a.reserveResultRootLocked(opID, resID, artifact, layout, authorityRef)
	case receivecontract.DirectoryTreeSingleFile:
		return a.reserveSingleFileLocked(opID, resID, artifact, layout, authorityRef, ctrlDir)
	default:
		return receivecontract.DestinationReservation{}, nil, false, transfer.ErrInvalidOutputBinding
	}
}

func (a *Authority) reserveResultRootLocked(
	opID receivecontract.OperationID,
	resID receivecontract.DestinationReservationID,
	artifact receivecontract.ArtifactSpec,
	layout receivecontract.DirectoryTreeLayout,
	authorityRef receivecontract.AuthorityRef,
) (receivecontract.DestinationReservation, *Dir, bool, error) {
	rootLayout, ok := layout.ResultRoot()
	if !ok {
		return receivecontract.DestinationReservation{}, nil, false, transfer.ErrInvalidOutputBinding
	}
	for idx := range uint32(maxReservationProbes) {
		logical, err := receivecontract.CollisionName(opID, rootLayout.Name(), idx, false)
		if err != nil {
			return receivecontract.DestinationReservation{}, nil, false, err
		}
		physical, repaired, err := sanitizeTopLevelComponent(opID, logical, a.root)
		if err != nil {
			return receivecontract.DestinationReservation{}, nil, false, err
		}
		err = mkdirChild(a.root, physical, publicDirMode)
		if errors.Is(err, ErrNamespaceCollision) || errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return receivecontract.DestinationReservation{}, nil, false, err
		}
		resDir, err := openChildDir(a.root, physical)
		if err != nil {
			_ = removeChild(a.root, physical, true)
			return receivecontract.DestinationReservation{}, nil, false, err
		}
		res, err := receivecontract.NewNativeNamedEntryReservation(opID, resID, artifact, authorityRef, logical, physical, idx)
		if err != nil {
			_ = resDir.Close()
			_ = removeChild(a.root, physical, true)
			return receivecontract.DestinationReservation{}, nil, false, err
		}
		if repaired {
			emitTrace(a.diagnostics, traceEvent{
				component:    "directory",
				operation:    "map_name",
				decision:     "admitted",
				operationID:  opID,
				logicalPath:  logical,
				physicalPath: physical,
			})
		}
		return res, resDir, true, nil
	}
	return receivecontract.DestinationReservation{}, nil, false, ErrNamespaceCollision
}

func (a *Authority) reserveSingleFileLocked(
	opID receivecontract.OperationID,
	resID receivecontract.DestinationReservationID,
	artifact receivecontract.ArtifactSpec,
	layout receivecontract.DirectoryTreeLayout,
	authorityRef receivecontract.AuthorityRef,
	ctrlDir *Dir,
) (receivecontract.DestinationReservation, *Dir, bool, error) {
	single, ok := layout.SingleFile()
	if !ok {
		return receivecontract.DestinationReservation{}, nil, false, transfer.ErrInvalidOutputBinding
	}
	claimed := existingSingleFileClaims(ctrlDir)
	for idx := range uint32(maxReservationProbes) {
		logical, err := receivecontract.CollisionName(opID, single.SuggestedName, idx, true)
		if err != nil {
			return receivecontract.DestinationReservation{}, nil, false, err
		}
		physical, repaired, err := sanitizeTopLevelComponent(opID, logical, a.root)
		if err != nil {
			return receivecontract.DestinationReservation{}, nil, false, err
		}
		if _, taken := claimed[physical]; taken {
			continue
		}
		kind, _, err := lstatChild(a.root, physical)
		if err != nil {
			return receivecontract.DestinationReservation{}, nil, false, err
		}
		if kind != EntryAbsent {
			continue
		}
		res, err := receivecontract.NewNativeNamedEntryReservation(opID, resID, artifact, authorityRef, logical, physical, idx)
		if err != nil {
			return receivecontract.DestinationReservation{}, nil, false, err
		}
		if repaired {
			emitTrace(a.diagnostics, traceEvent{
				component:    "file",
				operation:    "map_name",
				decision:     "admitted",
				operationID:  opID,
				logicalPath:  logical,
				physicalPath: physical,
			})
		}
		return res, a.root, false, nil
	}
	return receivecontract.DestinationReservation{}, nil, false, ErrNamespaceCollision
}

func existingSingleFileClaims(ctrlDir *Dir) map[string]struct{} {
	claimed := make(map[string]struct{})
	names, err := readDirNames(ctrlDir)
	if err != nil {
		return claimed
	}
	for _, name := range names {
		if _, ok := isValidOpDirName(name); !ok {
			continue
		}
		opDir, err := openChildDir(ctrlDir, name)
		if err != nil {
			continue
		}
		state, _, err := readJournalState(opDir)
		_ = opDir.Close()
		if err != nil || !state.hasMeta {
			continue
		}
		if res, ok := state.meta.Intent.MaterializationPlan().DestinationReservation(); ok &&
			res.Kind() == receivecontract.ReservationNamedContainerEntry &&
			res.EntryKind() == receivecontract.ContainerEntrySingleFile {
			claimed[res.PhysicalName()] = struct{}{}
		}
	}
	return claimed
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
	if op == nil || op.authority == nil || ctx == nil || !op.Idle() {
		return transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	a := op.authority
	opID := op.intent.OperationID()
	rootPath := a.rootPath
	if err := a.Close(); err != nil {
		return err
	}
	summary, err := DiscardOperation(ctx, rootPath, opID)
	if err == nil && summary.State() != RecoveryOperationDiscarded {
		return errors.New("owned output cleanup remains pending")
	}
	return err
}

func (op *Operation) closeResources() error {
	if op == nil || op.closed {
		return nil
	}
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
func retireControlDir(root *Dir, opDirName string) error {
	ctrlDir, err := openChildDir(root, ControlDirName)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	dirLock, _, lockErr := acquireLockFile(ctrlDir, dirLockFileName, false, true)
	if lockErr != nil {
		_ = ctrlDir.Close()
		return lockErr
	}
	removeErr := removeOperationDirectory(root, opDirName)
	names, listErr := readDirNames(ctrlDir)
	empty := listErr == nil
	for _, name := range names {
		if name != dirLockFileName && name != authorityIDFile {
			empty = false
			break
		}
	}
	if empty && removeErr == nil {
		// Release the directory lock before unlinking .lock and .windshare-tmp.
		_ = dirLock.Close()
		_ = ctrlDir.Close()
		_ = os.Remove(filepath.Join(root.path, ControlDirName, dirLockFileName))
		_ = os.Remove(filepath.Join(root.path, ControlDirName, authorityIDFile))
		_ = os.Remove(filepath.Join(root.path, ControlDirName))
		return nil
	}
	return errors.Join(removeErr, dirLock.Close(), ctrlDir.Close())
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
