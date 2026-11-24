package output

import (
	"context"
	"errors"
	"io/fs"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type dirState struct {
	admission     transfer.DirectoryAdmission
	logicalPath   string
	materialized  bool
	isSessionRoot bool
	dir           *Dir
	finalized     bool
	settlement    transfer.DirectorySettlement
}

// settledFileOutcome retains only the compact fields needed to replay
// non-delivered terminal file settlements within the active session.
// Delivered files rely on op.journal's delivery receipts instead of keeping
// per-file transfer.FileSettlement structs in memory.
type settledFileOutcome struct {
	fileID          catalog.FileID
	revision        content.FileRevision
	kind            transfer.FileSettlementKind
	bound           bool
	itemBlockReason transfer.ItemBlockReason
}

// Session implements transfer.DirectTreeSession and transfer.DeliveryHistory
// directly over an active Operation's journal and open directory handles.
type Session struct {
	authority    *Authority
	op           *Operation
	intent       transfer.ReceiveIntent
	expectsRoot  bool
	sessionID    transfer.OutputSessionID
	capabilities transfer.DirectTreeCapabilities
	names        *nameMapper

	mu              sync.Mutex
	closed          bool
	closeSettlement transfer.DirectTreeSettlement
	closeKind       uint8 // 1 = paused, 2 = finalized
	rootSettled     bool
	dirsByLogical   map[string]*dirState
	activeFiles     map[string]*fileTransaction
	settledOutcomes map[string]settledFileOutcome
}

var (
	_ transfer.DirectTreeSession = (*Session)(nil)
	_ transfer.DeliveryHistory   = (*Session)(nil)
)

func newSession(a *Authority, op *Operation) (*Session, error) {
	if op.intent.IsZero() {
		return nil, transfer.ErrInvalidOutputBinding
	}
	tree, ok := op.intent.ArtifactSpec().DirectoryTree()
	if !ok {
		return nil, transfer.ErrInvalidOutputBinding
	}
	sessionID, err := randomSessionID(a.random)
	if err != nil {
		return nil, err
	}
	reservation, ok := op.intent.MaterializationPlan().DestinationReservation()
	if !ok {
		return nil, transfer.ErrInvalidOutputBinding
	}
	mapper, err := newNameMapper(
		op.intent.OperationID(),
		reservation,
		op.journal.nameMappings(),
		op.journal.recordName,
		func(ev nameTraceEvent) {
			emitTrace(a.diagnostics, traceEvent{
				component:    "file",
				operation:    "map_name",
				decision:     "admitted",
				operationID:  op.intent.OperationID(),
				intentDigest: op.intent.Digest(),
				sessionID:    sessionID,
				logicalPath:  ev.logical,
				physicalPath: ev.physical,
				nameAttempt:  ev.attempt,
				nameReused:   ev.reused,
			})
		},
	)
	if err != nil {
		return nil, err
	}
	caps, err := transfer.NewDirectTreeCapabilities(transfer.DirectTreeCapabilities{
		Durability:           transfer.DurabilityProcessRestart,
		RandomWrite:          true,
		FileFailureIsolation: true,
		ModifiedTime:         true,
	})
	if err != nil {
		return nil, err
	}
	return &Session{
		authority:     a,
		op:            op,
		intent:        op.intent,
		expectsRoot:   tree.Kind() != receivecontract.DirectoryTreeSingleFile,
		sessionID:     sessionID,
		capabilities:  caps,
		names:         mapper,
		dirsByLogical: make(map[string]*dirState),
		activeFiles:   make(map[string]*fileTransaction),
	}, nil
}

func (s *Session) SessionID() transfer.OutputSessionID           { return s.sessionID }
func (s *Session) Capabilities() transfer.DirectTreeCapabilities { return s.capabilities }

func (s *Session) LookupDelivery(
	ctx context.Context,
	fileID catalog.FileID,
	materializationRelativePath string,
) (transfer.HistoricalDelivery, bool, error) {
	if ctx == nil {
		return transfer.HistoricalDelivery{}, false, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return transfer.HistoricalDelivery{}, false, err
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return transfer.HistoricalDelivery{}, false, transfer.ErrDirectTreeSessionFatal
	}
	canonical, err := catalog.CanonicalPath(materializationRelativePath)
	if err != nil || canonical != materializationRelativePath {
		return transfer.HistoricalDelivery{}, false, nil
	}
	entry, ok := s.op.journal.lookupDelivered(fileID, canonical)
	if !ok {
		pub, hasPub := s.op.journal.lookupPublishing(canonical)
		if !hasPub || pub.FileID != fileID {
			return transfer.HistoricalDelivery{}, false, nil
		}
		reconciled, err := s.tryReconcilePublishingByPath(ctx, pub)
		if err != nil {
			return transfer.HistoricalDelivery{}, false, err
		}
		if !reconciled {
			return transfer.HistoricalDelivery{}, false, nil
		}
		return transfer.HistoricalDelivery{
			OperationID:         s.intent.OperationID(),
			FileID:              pub.FileID,
			FileRevision:        pub.Revision,
			MaterializationPath: pub.Path,
			Size:                pub.ExactSize,
		}, true, nil
	}
	emitTrace(s.authority.diagnostics, traceEvent{
		component:    "file",
		operation:    "delivery_receipt",
		decision:     "reconciled",
		operationID:  s.intent.OperationID(),
		intentDigest: s.intent.Digest(),
		sessionID:    s.sessionID,
		fileID:       fileID,
	})
	return transfer.HistoricalDelivery{
		OperationID:         s.intent.OperationID(),
		FileID:              entry.FileID,
		FileRevision:        entry.Revision,
		MaterializationPath: entry.Path,
		Size:                entry.ExactSize,
	}, true, nil
}

func (s *Session) AdmitDirectory(
	ctx context.Context,
	request transfer.DirectoryMaterializationRequest,
) (transfer.DirectoryAdmission, error) {
	if ctx == nil {
		return transfer.DirectoryAdmission{}, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return transfer.DirectoryAdmission{}, err
	}
	if !transfer.DirectoryMaterializationMatchesIntent(s.intent, request) {
		return transfer.DirectoryAdmission{}, transfer.ErrInvalidOutputBinding
	}
	dirReq, ok := request.Directory()
	if !ok {
		return transfer.DirectoryAdmission{}, transfer.ErrInvalidOutputBinding
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return transfer.DirectoryAdmission{}, transfer.ErrDirectTreeSessionFatal
	}
	admission, err := transfer.NewDirectoryAdmission(dirReq)
	if err != nil {
		return transfer.DirectoryAdmission{}, err
	}
	logicalPath := dirReq.Path().String()
	if existing, found := s.dirsByLogical[logicalPath]; found {
		if existing.admission.Equal(admission) {
			return existing.admission, nil
		}
		return transfer.DirectoryAdmission{}, transfer.ErrInvalidOutputBinding
	}

	// Traverse-only directories need an admission for child lineage, but do not
	// create a directory on disk.
	if request.Projection().Kind() == ordinaryoutput.ArtifactTraverseOnly {
		state := &dirState{
			admission:    admission,
			logicalPath:  logicalPath,
			materialized: false,
		}
		s.dirsByLogical[logicalPath] = state
		return admission, nil
	}

	dirHandle, isRoot, err := s.openAdmittedDirLocked(ctx, logicalPath, dirReq.ParentAdmission())
	if err != nil {
		return transfer.DirectoryAdmission{}, err
	}
	state := &dirState{
		admission:     admission,
		logicalPath:   logicalPath,
		materialized:  true,
		isSessionRoot: isRoot,
		dir:           dirHandle,
	}
	s.dirsByLogical[logicalPath] = state
	return admission, nil
}

func (s *Session) openAdmittedDirLocked(
	ctx context.Context,
	logicalPath string,
	parentAdmission transfer.DirectoryAdmission,
) (*Dir, bool, error) {
	if parentAdmission.IsZero() {
		// Root of the reserved tree is already bound in op.resultDir.
		return s.op.resultDir, true, nil
	}
	parentState, found := s.dirsByLogical[parentAdmission.Path()]
	if !found || !parentState.admission.Equal(parentAdmission) || !parentState.materialized || parentState.dir == nil {
		// Ancestor may be traverse-only when selecting a deep subdirectory as result root.
		if logicalPath == s.names.requestedRoot && s.names.entryKind == receivecontract.ContainerEntryResultRoot {
			return s.op.resultDir, true, nil
		}
		return nil, false, transfer.ErrInvalidOutputBinding
	}
	leaf, err := s.names.bindLeaf(logicalPath, parentState.dir, true)
	if err != nil {
		return nil, false, wrapOutputFault(ctx, transferfault.OutputNamespaceUnsafe, "map directory name", err)
	}
	childDir, err := ensureSubdirNoAlias(parentState.dir, leaf)
	if err != nil {
		return nil, false, wrapOutputFault(ctx, transferfault.OutputNamespaceUnsafe, "materialize directory", err)
	}
	return childDir, false, nil
}

func ensureSubdirNoAlias(parent *Dir, name string) (*Dir, error) {
	err := mkdirChild(parent, name, publicDirMode)
	if err == nil {
		return openChildDir(parent, name)
	}
	if !errors.Is(err, ErrNamespaceCollision) && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	kind, _, statErr := lstatChild(parent, name)
	if statErr != nil {
		return nil, statErr
	}
	if kind != EntryDirectory {
		return nil, ErrUnsafeNamespace
	}
	exact, matchErr := entryMatchesSpelling(parent, name)
	if matchErr != nil || !exact {
		return nil, errors.Join(ErrUnsafeNamespace, matchErr)
	}
	return openChildDir(parent, name)
}

func (s *Session) FinalizeDirectory(
	ctx context.Context,
	admission transfer.DirectoryAdmission,
) (transfer.DirectorySettlement, error) {
	if ctx == nil {
		return transfer.DirectorySettlement{}, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return transfer.DirectorySettlement{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return transfer.DirectorySettlement{}, transfer.ErrDirectTreeSessionFatal
	}
	state, ok := s.dirsByLogical[admission.Path()]
	if !ok || !state.admission.Equal(admission) {
		return transfer.DirectorySettlement{}, transfer.ErrInvalidOutputBinding
	}
	if state.finalized {
		return state.settlement, nil
	}
	var settlement transfer.DirectorySettlement
	var err error
	if state.materialized && state.dir != nil && admission.ModifiedTime().Present() {
		if mtimeErr := setDirModifiedTimeCatalog(state.dir, admission.ModifiedTime()); mtimeErr != nil {
			dirFault, _ := transferfault.NewOutput(transferfault.ScopeDirectoryLocal, transferfault.OutputDirectoryMetadata)
			settlement, err = transfer.NewIsolatedDirectorySettlement(admission, dirFault)
		} else {
			settlement, err = transfer.NewFinalizedDirectorySettlement(admission)
		}
	} else {
		settlement, err = transfer.NewFinalizedDirectorySettlement(admission)
	}
	if err != nil {
		return transfer.DirectorySettlement{}, err
	}
	state.finalized = true
	state.settlement = settlement
	if state.isSessionRoot {
		s.rootSettled = true
	}
	// Release child directory handles once finalized so large directory trees
	// never exhaust OS file descriptor limits.
	if state.materialized && !state.isSessionRoot && state.dir != nil {
		_ = state.dir.Close()
		state.dir = nil
	}
	return settlement, nil
}

func (s *Session) PauseTree(ctx context.Context) (transfer.DirectTreeSettlement, error) {
	if ctx == nil {
		return transfer.DirectTreeSettlement{}, transfer.ErrInvalidOutputBinding
	}
	s.mu.Lock()
	if s.closed {
		if s.closeKind == 1 {
			settled := s.closeSettlement
			s.mu.Unlock()
			return settled, nil
		}
		s.mu.Unlock()
		return transfer.DirectTreeSettlement{}, transfer.ErrDirectTreeSessionFatal
	}
	active := make([]*fileTransaction, 0, len(s.activeFiles))
	for _, tx := range s.activeFiles {
		active = append(active, tx)
	}
	s.mu.Unlock()

	var pauseErr error
	for _, tx := range active {
		if _, err := tx.Pause(ctx); err != nil {
			pauseErr = errors.Join(pauseErr, err)
		}
	}
	flushErr := s.op.journal.flush()
	s.op.refreshNameAdjustments()

	s.mu.Lock()
	s.closeDirHandlesLocked()
	settlement, settleErr := transfer.NewDirectTreeSettlement(transfer.DirectTreeSettlementPaused)
	s.closed = true
	s.closeKind = 1
	s.closeSettlement = settlement
	s.mu.Unlock()

	s.authority.mu.Lock()
	s.op.opened = false
	s.authority.mu.Unlock()

	emitTrace(s.authority.diagnostics, traceEvent{
		component:    "session",
		operation:    "pause_tree",
		decision:     "settled",
		operationID:  s.intent.OperationID(),
		intentDigest: s.intent.Digest(),
		sessionID:    s.sessionID,
	})
	if err := errors.Join(pauseErr, flushErr, settleErr); err != nil {
		return transfer.DirectTreeSettlement{}, DiagnoseFailure(FailureDurability, err)
	}
	return settlement, nil
}

func (s *Session) FinalizeTree(
	ctx context.Context,
	outcome transfer.DirectTreeOutcome,
) (transfer.DirectTreeSettlement, error) {
	if ctx == nil {
		return transfer.DirectTreeSettlement{}, transfer.ErrInvalidOutputBinding
	}
	s.mu.Lock()
	if s.closed {
		if s.closeKind == 2 {
			settled := s.closeSettlement
			s.mu.Unlock()
			return settled, nil
		}
		s.mu.Unlock()
		return transfer.DirectTreeSettlement{}, transfer.ErrDirectTreeSessionFatal
	}
	if len(s.activeFiles) > 0 {
		s.mu.Unlock()
		return transfer.DirectTreeSettlement{}, transfer.ErrInvalidOutputBinding
	}
	for _, d := range s.dirsByLogical {
		if !d.finalized {
			s.mu.Unlock()
			return transfer.DirectTreeSettlement{}, transfer.ErrInvalidOutputBinding
		}
	}
	if outcome == transfer.DirectTreeOutcomeSuccess && s.expectsRoot && !s.rootSettled {
		s.mu.Unlock()
		return transfer.DirectTreeSettlement{}, transfer.ErrInvalidOutputBinding
	}

	var kind transfer.DirectTreeSettlementKind
	switch outcome {
	case transfer.DirectTreeOutcomeSuccess:
		kind = transfer.DirectTreeSettlementSuccess
	case transfer.DirectTreeOutcomePartial:
		kind = transfer.DirectTreeSettlementPartial
	case transfer.DirectTreeOutcomeFailed:
		kind = transfer.DirectTreeSettlementFailed
	default:
		s.mu.Unlock()
		return transfer.DirectTreeSettlement{}, transfer.ErrInvalidOutputBinding
	}
	settlement, err := transfer.NewDirectTreeSettlement(kind)
	if err != nil {
		s.mu.Unlock()
		return transfer.DirectTreeSettlement{}, err
	}
	s.closeDirHandlesLocked()
	s.closed = true
	s.closeKind = 2
	s.closeSettlement = settlement
	s.mu.Unlock()

	s.op.refreshNameAdjustments()

	if outcome == transfer.DirectTreeOutcomeSuccess {
		// Completed transfer: acquire the directory lock before releasing
		// journal/task.lock and deleting .windshare-tmp/<opID> so no concurrent
		// receiver can reopen the completed operation before removal.
		s.authority.mu.Lock()
		s.op.opened = false
		s.op.completed = true
		root := s.authority.root
		op := s.op
		opDirName := op.opDirName
		if s.authority.operation == op {
			s.authority.operation = nil
		}
		s.authority.mu.Unlock()

		var cleanupErr error
		if root != nil {
			cleanupErr = retireControlDir(root, opDirName, op.closeResources)
		} else {
			cleanupErr = op.closeResources()
		}
		if cleanupErr != nil {
			nativeClass, _ := ClassifyNativeError(cleanupErr)
			emitTrace(s.authority.diagnostics, traceEvent{
				component:    "session",
				operation:    "finalize_tree",
				decision:     "cleanup_pending",
				failed:       true,
				failureStage: FailureClose,
				nativeClass:  nativeClass,
				bindingID:    s.authority.bindingID,
				operationID:  s.intent.OperationID(),
				intentDigest: s.intent.Digest(),
				sessionID:    s.sessionID,
			})
			return transfer.DirectTreeSettlement{}, DiagnoseFailure(FailureClose, cleanupErr)
		}
		return settlement, nil
	}

	// Partial or failed outcome: flush journal and keep operation resumable/discardable.
	flushErr := s.op.journal.flush()
	s.authority.mu.Lock()
	s.op.opened = false
	s.authority.mu.Unlock()
	if flushErr != nil {
		return transfer.DirectTreeSettlement{}, DiagnoseFailure(FailureDurability, flushErr)
	}
	return settlement, nil
}

func (s *Session) closeDirHandlesLocked() {
	for _, state := range s.dirsByLogical {
		if state.materialized && !state.isSessionRoot && state.dir != nil {
			_ = state.dir.Close()
			state.dir = nil
		}
	}
}

func (s *Session) bindTarget(target transfer.FileMaterializationTarget) (transfer.MaterializedFileBinding, error) {
	objectID := ownedObjectIDFor(s.intent.OperationID(), target.Locator().Digest())
	return transfer.BindFileMaterializationTarget(target, objectID)
}

func (s *Session) recordSettledOutcome(canonicalPath string, tx *fileTransaction, outcome settledFileOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tx != nil && s.activeFiles[canonicalPath] == tx {
		delete(s.activeFiles, canonicalPath)
	}
	if s.settledOutcomes == nil {
		s.settledOutcomes = make(map[string]settledFileOutcome)
	}
	s.settledOutcomes[canonicalPath] = outcome
}

func (s *Session) replaySettledOutcome(
	outcome settledFileOutcome,
	target transfer.FileMaterializationTarget,
) (transfer.FileStart, error) {
	if outcome.fileID != target.FileID() || outcome.revision != target.FileRevision() {
		return transfer.FileStart{}, transfer.ErrInvalidOutputBinding
	}
	var (
		settlement transfer.FileSettlement
		err        error
	)
	switch outcome.kind {
	case transfer.FileCollision:
		if outcome.bound {
			binding, bindErr := s.bindTarget(target)
			if bindErr != nil {
				return transfer.FileStart{}, bindErr
			}
			settlement, err = transfer.NewTransactionCollisionFileSettlement(binding)
		} else {
			settlement, err = transfer.NewCollisionFileSettlement(target)
		}
	case transfer.FileItemBlocked:
		stateRef, refErr := transfer.NewMaterializationStateRef(s.sessionID, target.Locator().Digest())
		if refErr != nil {
			return transfer.FileStart{}, refErr
		}
		settlement, err = transfer.NewImmediateItemBlockedFileSettlement(target, stateRef, outcome.itemBlockReason)
	case transfer.FileFailed:
		binding, bindErr := s.bindTarget(target)
		if bindErr != nil {
			return transfer.FileStart{}, bindErr
		}
		settlement, err = transfer.NewFailedFileSettlement(binding)
	default:
		return transfer.FileStart{}, transfer.ErrInvalidOutputBinding
	}
	if err != nil {
		return transfer.FileStart{}, err
	}
	return transfer.NewFileSettlementStart(settlement)
}
