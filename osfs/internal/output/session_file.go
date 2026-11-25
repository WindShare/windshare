package output

import (
	"context"
	"crypto/sha256"
	"errors"
	"io/fs"
	"os"
	"strings"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type fileParent struct {
	state *dirState
	dir   *Dir
}

func (s *Session) BeginFile(
	ctx context.Context,
	file transfer.MaterializationFile,
) (transfer.FileStart, error) {
	if ctx == nil {
		return transfer.FileStart{}, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return transfer.FileStart{}, err
	}
	if !transfer.MaterializationFileMatchesIntent(s.intent, file) ||
		file.Target().OutputSessionID() != s.sessionID {
		return transfer.FileStart{}, transfer.ErrInvalidOutputBinding
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return transfer.FileStart{}, transfer.ErrDirectTreeSessionFatal
	}
	canonicalPath := file.MaterializationRelativePath().String()
	if outcome, exists := s.settledOutcomes[canonicalPath]; exists {
		return s.replaySettledOutcome(outcome, file.Target())
	}
	if _, active := s.activeFiles[canonicalPath]; active {
		return transfer.FileStart{}, transfer.ErrInvalidOutputBinding
	}
	parent, err := s.resolveFileParentLocked(file)
	if err != nil {
		return transfer.FileStart{}, err
	}

	desc := file.Descriptor()
	target := file.Target()
	binding, err := s.bindTarget(target)
	if err != nil {
		return transfer.FileStart{}, err
	}

	// 1. Check delivery receipt first: already-published files are skipped even
	// if the user moved, edited, or deleted the public output.
	if _, delivered := s.op.journal.lookupDelivered(desc.FileID(), canonicalPath); delivered {
		emitTrace(s.authority.diagnostics, traceEvent{
			component:    "file",
			operation:    "delivery_receipt",
			decision:     "reconciled",
			operationID:  s.intent.OperationID(),
			intentDigest: s.intent.Digest(),
			sessionID:    s.sessionID,
			fileID:       desc.FileID(),
		})
		settlement, err := transfer.NewPreviouslyPublishedFileSettlement(binding)
		if err != nil {
			return transfer.FileStart{}, err
		}
		return transfer.NewFileSettlementStart(settlement)
	}

	// Resolve physical filename inside parent.dir (reusing any persisted mapping).
	physLeaf, err := s.names.bindFileLeaf(canonicalPath, parent.dir)
	if err != nil {
		return transfer.FileStart{}, wrapFileFault(ctx, transferfault.OutputNamespaceUnsafe, "map file name", err)
	}

	// 2. Reconcile an interrupted publish (renamed or pending rename before receipt).
	if pub, hasPub := s.op.journal.lookupPublishing(canonicalPath); hasPub {
		return s.reconcilePublishingFileLocked(ctx, file, binding, parent, physLeaf, pub)
	}

	// 3. Check existing partial progress in the journal.
	if partial, hasPartial := s.op.journal.lookupPartial(canonicalPath); hasPartial {
		return s.resumePartialFileLocked(ctx, file, binding, parent, physLeaf, partial)
	}

	// 4. Fresh file: preflight collision before allocating a .part file.
	return s.beginFreshFileLocked(ctx, file, binding, parent, physLeaf)
}

func (s *Session) beginFreshFileLocked(
	ctx context.Context,
	file transfer.MaterializationFile,
	binding transfer.MaterializedFileBinding,
	parent fileParent,
	physLeaf string,
) (transfer.FileStart, error) {
	desc := file.Descriptor()
	target := file.Target()
	canonicalPath := file.MaterializationRelativePath().String()

	occupied, statErr := destinationOccupied(ctx, parent.dir, physLeaf)
	if statErr != nil {
		return transfer.FileStart{}, statErr
	}
	if occupied {
		return s.settleCollisionLocked(canonicalPath, target)
	}

	partName := partFileName(desc.FileID(), target.Locator().Digest())
	_ = removeChild(s.op.opDir, partName, false)
	partFile, err := openChildFile(s.op.opDir, partName, os.O_RDWR|os.O_CREATE|os.O_EXCL, publicFileMode)
	if err != nil {
		return transfer.FileStart{}, wrapFileFault(ctx, transferfault.OutputStateIO, "create staged file", err)
	}
	emptyRanges, _ := content.NewRangeSet(nil)
	durable, err := transfer.VerifyRestartRanges(binding, 0, emptyRanges)
	if err != nil {
		_ = partFile.Close()
		_ = removeChild(s.op.opDir, partName, false)
		return transfer.FileStart{}, err
	}
	tx := newFileTransaction(s, binding, canonicalPath, parent, physLeaf, partName, partFile, emptyRanges, 0)
	start, err := transfer.NewFileTransactionStart(tx, durable)
	if err != nil {
		_ = partFile.Close()
		_ = removeChild(s.op.opDir, partName, false)
		return transfer.FileStart{}, err
	}
	s.registerActiveFileLocked(canonicalPath, tx)
	return start, nil
}

func destinationOccupied(ctx context.Context, parentDir *Dir, physLeaf string) (bool, error) {
	kind, _, statErr := lstatChild(parentDir, physLeaf)
	if statErr != nil {
		return false, wrapFileFault(ctx, transferfault.OutputStateIO, "inspect destination file", statErr)
	}
	return kind != EntryAbsent, nil
}

func (s *Session) settleCollisionLocked(canonicalPath string, target transfer.FileMaterializationTarget) (transfer.FileStart, error) {
	settlement, err := transfer.NewCollisionFileSettlement(target)
	if err != nil {
		return transfer.FileStart{}, err
	}
	s.recordSettledOutcomeLocked(canonicalPath, nil, settledFileOutcome{
		fileID:   target.FileID(),
		revision: target.FileRevision(),
		kind:     transfer.FileCollision,
	})
	return transfer.NewFileSettlementStart(settlement)
}

func (s *Session) tryReconcilePublishingByPath(ctx context.Context, pub publishingEntry) (bool, error) {
	phys, err := s.names.resolve(pub.Path)
	if err != nil || phys == "" {
		return false, nil
	}
	parentRel, physLeaf := splitLogicalPath(phys)
	parentDir, owned, err := s.openRelativeDir(parentRel)
	if err != nil {
		return false, nil
	}
	if owned {
		defer parentDir.Close()
	}
	reconciled, _, err := s.tryReconcilePublished(ctx, pub, parentDir, physLeaf)
	return reconciled, err
}

// tryReconcilePublished verifies whether a publishing entry whose staged .part
// file already moved into the destination matches this task's pre-publish
// fingerprint. Matching files receive a durable delivery receipt; non-matching
// destination entries are reported as collisions and never overwritten.
func (s *Session) tryReconcilePublished(
	ctx context.Context,
	pub publishingEntry,
	parentDir *Dir,
	physLeaf string,
) (reconciled bool, collided bool, err error) {
	partKind, _, partErr := lstatChild(s.op.opDir, pub.PartName)
	if partErr != nil {
		return false, false, wrapFileFault(ctx, transferfault.OutputStateIO, "inspect staged file", partErr)
	}
	if partKind != EntryAbsent {
		return false, false, nil
	}
	kind, size, statErr := lstatChild(parentDir, physLeaf)
	if statErr != nil {
		return false, false, wrapFileFault(ctx, transferfault.OutputStateIO, "inspect published file", statErr)
	}
	if kind == EntryAbsent {
		return false, false, nil
	}
	if kind != EntryRegularFile || size != pub.ExactSize {
		return false, true, nil
	}
	destFile, openErr := openChildFile(parentDir, physLeaf, os.O_RDONLY, 0)
	if errors.Is(openErr, fs.ErrNotExist) {
		return false, false, nil
	}
	if errors.Is(openErr, ErrUnsafeNamespace) {
		return false, true, nil
	}
	if openErr != nil {
		return false, false, wrapFileFault(ctx, transferfault.OutputStateIO, "open published file", openErr)
	}
	actualFP, fpErr := samplePublicationFingerprint(destFile, pub.ExactSize)
	_ = destFile.Close()
	if fpErr != nil {
		return false, false, wrapFileFault(ctx, transferfault.OutputStateIO, "verify published file", fpErr)
	}
	if actualFP != pub.Fingerprint {
		return false, true, nil
	}
	if err := s.op.journal.recordDelivered(deliveredEntry{
		FileID:    pub.FileID,
		Path:      pub.Path,
		Revision:  pub.Revision,
		ExactSize: pub.ExactSize,
	}, true); err != nil {
		return false, false, DiagnoseFailure(FailureDurability, wrapFileFault(ctx, transferfault.OutputStateIO, "reconcile delivery receipt", err))
	}
	emitTrace(s.authority.diagnostics, traceEvent{
		component:          "file",
		operation:          "delivery_receipt",
		decision:           "reconciled",
		checkpointDecision: "publishing_verified",
		operationID:        s.intent.OperationID(),
		intentDigest:       s.intent.Digest(),
		sessionID:          s.sessionID,
		fileID:             pub.FileID,
	})
	return true, false, nil
}

func (s *Session) tracePublicationMismatch(fileID catalog.FileID) {
	emitTrace(s.authority.diagnostics, traceEvent{
		component:          "file",
		operation:          "recover_file",
		decision:           "collision",
		checkpointDecision: "publication_mismatch",
		operationID:        s.intent.OperationID(),
		intentDigest:       s.intent.Digest(),
		sessionID:          s.sessionID,
		fileID:             fileID,
	})
}

// preflightRecoveredStageCollision rejects an already-occupied destination
// before resuming or publishing a verified stage so no extra network transfer
// or redundant journal writes occur while the staged .part stays intact.
func (s *Session) preflightRecoveredStageCollisionLocked(
	ctx context.Context,
	file transfer.MaterializationFile,
	parentDir *Dir,
	physLeaf string,
	partFile *os.File,
	prevRev content.FileRevision,
	reason content.RevisionComparisonReason,
	retainedBytes uint64,
) (transfer.FileStart, bool, error) {
	occupied, statErr := destinationOccupied(ctx, parentDir, physLeaf)
	if statErr != nil {
		_ = partFile.Close()
		return transfer.FileStart{}, true, statErr
	}
	if !occupied {
		return transfer.FileStart{}, false, nil
	}
	_ = partFile.Close()
	desc := file.Descriptor()
	emitTrace(s.authority.diagnostics, traceEvent{
		component:          "file",
		operation:          "recover_file",
		decision:           "collision",
		checkpointDecision: "destination_collision",
		operationID:        s.intent.OperationID(),
		intentDigest:       s.intent.Digest(),
		sessionID:          s.sessionID,
		fileID:             desc.FileID(),
		previousRevision:   prevRev,
		currentRevision:    desc.FileRevision(),
		recoveryReason:     reason,
		retainedBytes:      retainedBytes,
	})
	start, err := s.settleCollisionLocked(file.MaterializationRelativePath().String(), file.Target())
	return start, true, err
}

func (s *Session) admitRecoveredTransactionLocked(
	file transfer.MaterializationFile,
	binding transfer.MaterializedFileBinding,
	parent fileParent,
	physLeaf string,
	partName string,
	partFile *os.File,
	persisted content.RangeSet,
	checkpointGen uint64,
	prevRev content.FileRevision,
	reason content.RevisionComparisonReason,
	reusedBytes uint64,
) (transfer.FileStart, error) {
	desc := file.Descriptor()
	canonicalPath := file.MaterializationRelativePath().String()
	emitTrace(s.authority.diagnostics, traceEvent{
		component:          "file",
		operation:          "recover_file",
		decision:           "reconciled",
		checkpointDecision: "exact",
		operationID:        s.intent.OperationID(),
		intentDigest:       s.intent.Digest(),
		sessionID:          s.sessionID,
		fileID:             desc.FileID(),
		previousRevision:   prevRev,
		currentRevision:    desc.FileRevision(),
		recoveryReason:     reason,
		reusedBytes:        reusedBytes,
	})
	durable, err := transfer.VerifyRestartRanges(binding, transfer.CheckpointGeneration(checkpointGen), persisted)
	if err != nil {
		_ = partFile.Close()
		return transfer.FileStart{}, err
	}
	tx := newFileTransaction(s, binding, canonicalPath, parent, physLeaf, partName, partFile, persisted, checkpointGen)
	start, err := transfer.NewFileTransactionStart(tx, durable)
	if err != nil {
		_ = partFile.Close()
		return transfer.FileStart{}, err
	}
	s.registerActiveFileLocked(canonicalPath, tx)
	return start, nil
}

func (s *Session) reconcilePublishingFileLocked(
	ctx context.Context,
	file transfer.MaterializationFile,
	binding transfer.MaterializedFileBinding,
	parent fileParent,
	physLeaf string,
	pub publishingEntry,
) (transfer.FileStart, error) {
	desc := file.Descriptor()
	target := file.Target()
	canonicalPath := file.MaterializationRelativePath().String()

	if pub.FileID != desc.FileID() {
		_ = removeChild(s.op.opDir, pub.PartName, false)
		_ = s.op.journal.recordRetired(pub.FileID, canonicalPath)
		return s.beginFreshFileLocked(ctx, file, binding, parent, physLeaf)
	}

	partKind, _, partErr := lstatChild(s.op.opDir, pub.PartName)
	if partErr != nil {
		return transfer.FileStart{}, wrapFileFault(ctx, transferfault.OutputStateIO, "inspect staged file", partErr)
	}
	if partKind == EntryAbsent && pub.BlockedReason == 0 {
		return s.reconcilePublishedWithoutStageLocked(ctx, file, binding, parent, physLeaf, pub)
	}

	// Stage still exists (interrupted before rename completed), or the publishing
	// entry was previously blocked before rename: check source lineage before
	// resuming publication with zero re-download.
	prevEvidence := content.RevisionSourceEvidence{
		FileRevision:   pub.Revision,
		ExactSize:      pub.ExactSize,
		SourceMetadata: pub.Metadata,
	}
	comparison := content.CompareRevisionMetadata(prevEvidence, desc.SourceEvidence())
	if !comparison.AllowsReuse() {
		return s.handleRevisionMismatchLocked(file, prevEvidence, comparison, pub.ExactSize, func() {
			pub.BlockedReason = transfer.ItemBlockRevisionConflict
			_ = s.op.journal.recordPublishing(pub, true)
		})
	}

	partFile, validStage, err := s.openValidatedPublishingStage(ctx, pub)
	if err != nil {
		return transfer.FileStart{}, err
	}
	if !validStage {
		occupied, statErr := destinationOccupied(ctx, parent.dir, physLeaf)
		if statErr != nil {
			return transfer.FileStart{}, statErr
		}
		if occupied {
			return s.settleCollisionLocked(canonicalPath, target)
		}
		_ = removeChild(s.op.opDir, pub.PartName, false)
		_ = s.op.journal.recordRetired(pub.FileID, canonicalPath)
		return s.beginFreshFileLocked(ctx, file, binding, parent, physLeaf)
	}

	if start, settled, err := s.preflightRecoveredStageCollisionLocked(
		ctx, file, parent.dir, physLeaf, partFile, prevEvidence.FileRevision, comparison.Reason, pub.ExactSize,
	); settled {
		return start, err
	}

	if pub.Revision != desc.FileRevision() || pub.BlockedReason != 0 {
		pub.Revision = desc.FileRevision()
		pub.Metadata = desc.SourceMetadata()
		pub.BlockedReason = 0
		_ = s.op.journal.recordPublishing(pub, true)
	}
	fullSet, err := fullRangeSet(pub.ExactSize)
	if err != nil {
		_ = partFile.Close()
		return transfer.FileStart{}, err
	}
	return s.admitRecoveredTransactionLocked(
		file, binding, parent, physLeaf, pub.PartName, partFile, fullSet,
		pub.CheckpointGen, prevEvidence.FileRevision, comparison.Reason, pub.ExactSize,
	)
}

func (s *Session) openValidatedPublishingStage(ctx context.Context, pub publishingEntry) (*os.File, bool, error) {
	partFile, err := openChildFile(s.op.opDir, pub.PartName, os.O_RDWR, publicFileMode)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, wrapFileFault(ctx, transferfault.OutputStateIO, "reopen publishing staged file", err)
	}
	info, statErr := partFile.Stat()
	if statErr == nil && info.Mode().IsRegular() && uint64(max(info.Size(), 0)) == pub.ExactSize {
		if fp, fpErr := samplePublicationFingerprint(partFile, pub.ExactSize); fpErr == nil && fp == pub.Fingerprint {
			return partFile, true, nil
		}
	}
	_ = partFile.Close()
	return nil, false, nil
}

func (s *Session) handleRevisionMismatchLocked(
	file transfer.MaterializationFile,
	prevEvidence content.RevisionSourceEvidence,
	comparison content.RevisionComparisonEvidence,
	retainedBytes uint64,
	persistBlocked func(),
) (transfer.FileStart, error) {
	desc := file.Descriptor()
	target := file.Target()
	canonicalPath := file.MaterializationRelativePath().String()

	if comparison.Comparison == content.RevisionComparisonUnavailable {
		emitTrace(s.authority.diagnostics, traceEvent{
			component:        "file",
			operation:        "recover_file",
			decision:         "isolated_failure",
			operationID:      s.intent.OperationID(),
			intentDigest:     s.intent.Digest(),
			sessionID:        s.sessionID,
			fileID:           desc.FileID(),
			previousRevision: prevEvidence.FileRevision,
			currentRevision:  desc.FileRevision(),
			recoveryReason:   comparison.Reason,
			retainedBytes:    retainedBytes,
		})
		srcFault, _ := transferfault.NewSource(transferfault.ScopeFileLocal, transferfault.SourceUnavailable)
		return transfer.FileStart{}, transferfault.Wrap(srcFault, errors.New("revision comparison evidence unavailable"))
	}
	persistBlocked()
	emitTrace(s.authority.diagnostics, traceEvent{
		component:          "file",
		operation:          "recover_file",
		decision:           "rejected",
		checkpointDecision: "revision_conflict",
		operationID:        s.intent.OperationID(),
		intentDigest:       s.intent.Digest(),
		sessionID:          s.sessionID,
		fileID:             desc.FileID(),
		previousRevision:   prevEvidence.FileRevision,
		currentRevision:    desc.FileRevision(),
		recoveryReason:     comparison.Reason,
		retainedBytes:      retainedBytes,
	})
	stateRef, err := transfer.NewMaterializationStateRef(s.sessionID, target.Locator().Digest())
	if err != nil {
		return transfer.FileStart{}, err
	}
	settlement, err := transfer.NewImmediateItemBlockedFileSettlement(target, stateRef, transfer.ItemBlockRevisionConflict)
	if err != nil {
		return transfer.FileStart{}, err
	}
	s.recordSettledOutcomeLocked(canonicalPath, nil, settledFileOutcome{
		fileID:          target.FileID(),
		revision:        target.FileRevision(),
		kind:            transfer.FileItemBlocked,
		itemBlockReason: transfer.ItemBlockRevisionConflict,
	})
	return transfer.NewFileSettlementStart(settlement)
}

func (s *Session) reconcilePublishedWithoutStageLocked(
	ctx context.Context,
	file transfer.MaterializationFile,
	binding transfer.MaterializedFileBinding,
	parent fileParent,
	physLeaf string,
	pub publishingEntry,
) (transfer.FileStart, error) {
	canonicalPath := file.MaterializationRelativePath().String()
	reconciled, collided, err := s.tryReconcilePublished(ctx, pub, parent.dir, physLeaf)
	if err != nil {
		return transfer.FileStart{}, err
	}
	if reconciled {
		settlement, err := transfer.NewPreviouslyPublishedFileSettlement(binding)
		if err != nil {
			return transfer.FileStart{}, err
		}
		return transfer.NewFileSettlementStart(settlement)
	}
	if collided {
		s.tracePublicationMismatch(pub.FileID)
		return s.settleCollisionLocked(canonicalPath, file.Target())
	}
	_ = s.op.journal.recordRetired(pub.FileID, canonicalPath)
	return s.beginFreshFileLocked(ctx, file, binding, parent, physLeaf)
}

func validatePartialStage(partFile *os.File, exactSize uint64, ranges []content.Range, fingerprint [stageFingerprintBytes]byte) bool {
	info, statErr := partFile.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Size() < 0 {
		return false
	}
	size := uint64(info.Size())
	if size > exactSize || size < maxRangeEnd(ranges) {
		return false
	}
	if len(ranges) == 0 {
		return size == 0
	}
	if fingerprint == ([stageFingerprintBytes]byte{}) {
		return false
	}
	actualFP, fpErr := sampleStageRangesFingerprint(partFile, ranges)
	return fpErr == nil && actualFP == fingerprint
}

func (s *Session) restartInvalidPartialStageLocked(
	ctx context.Context,
	file transfer.MaterializationFile,
	binding transfer.MaterializedFileBinding,
	parent fileParent,
	physLeaf string,
	partial partialEntry,
	stageMissing bool,
) (transfer.FileStart, error) {
	canonicalPath := file.MaterializationRelativePath().String()
	if !stageMissing {
		emitTrace(s.authority.diagnostics, traceEvent{
			component:          "file",
			operation:          "recover_file",
			decision:           "rejected",
			checkpointDecision: "invalid",
			operationID:        s.intent.OperationID(),
			intentDigest:       s.intent.Digest(),
			sessionID:          s.sessionID,
			fileID:             file.Descriptor().FileID(),
		})
	}
	// Preflight destination collision before discarding or recreating .part so
	// an already-occupied destination path never triggers a wasted download.
	occupied, statErr := destinationOccupied(ctx, parent.dir, physLeaf)
	if statErr != nil {
		return transfer.FileStart{}, statErr
	}
	if occupied {
		if stageMissing {
			emitTrace(s.authority.diagnostics, traceEvent{
				component:          "file",
				operation:          "recover_file",
				decision:           "collision",
				checkpointDecision: "missing_stage_collision",
				operationID:        s.intent.OperationID(),
				intentDigest:       s.intent.Digest(),
				sessionID:          s.sessionID,
				fileID:             file.Descriptor().FileID(),
			})
		}
		return s.settleCollisionLocked(canonicalPath, file.Target())
	}
	if !stageMissing {
		if err := removeChild(s.op.opDir, partial.PartName, false); err != nil {
			return transfer.FileStart{}, wrapFileFault(ctx, transferfault.OutputStateIO, "remove invalid staged file", err)
		}
	}
	_ = s.op.journal.recordRetired(partial.FileID, canonicalPath)
	return s.beginFreshFileLocked(ctx, file, binding, parent, physLeaf)
}

func (s *Session) resumePartialFileLocked(
	ctx context.Context,
	file transfer.MaterializationFile,
	binding transfer.MaterializedFileBinding,
	parent fileParent,
	physLeaf string,
	partial partialEntry,
) (transfer.FileStart, error) {
	desc := file.Descriptor()
	canonicalPath := file.MaterializationRelativePath().String()

	if partial.FileID != desc.FileID() {
		_ = removeChild(s.op.opDir, partial.PartName, false)
		_ = s.op.journal.recordRetired(partial.FileID, canonicalPath)
		return s.beginFreshFileLocked(ctx, file, binding, parent, physLeaf)
	}

	prevEvidence := content.RevisionSourceEvidence{
		FileRevision:   partial.Revision,
		ExactSize:      partial.ExactSize,
		SourceMetadata: partial.Metadata,
	}
	comparison := content.CompareRevisionMetadata(prevEvidence, desc.SourceEvidence())
	if !comparison.AllowsReuse() {
		return s.handleRevisionMismatchLocked(file, prevEvidence, comparison, rangeTotalBytes(partial.Ranges), func() {
			partial.BlockedReason = transfer.ItemBlockRevisionConflict
			_ = s.op.journal.recordPartial(partial, true)
		})
	}

	persistedSet, rangeErr := content.NewRangeSet(partial.Ranges)
	partFile, openErr := openChildFile(s.op.opDir, partial.PartName, os.O_RDWR, publicFileMode)
	if openErr != nil && !errors.Is(openErr, fs.ErrNotExist) {
		return transfer.FileStart{}, wrapFileFault(ctx, transferfault.OutputStateIO, "open staged file", openErr)
	}
	if openErr != nil || rangeErr != nil || !validatePartialStage(partFile, desc.ExactSize(), persistedSet.Ranges(), partial.Fingerprint) {
		stageMissing := errors.Is(openErr, fs.ErrNotExist)
		if partFile != nil {
			_ = partFile.Close()
		}
		return s.restartInvalidPartialStageLocked(ctx, file, binding, parent, physLeaf, partial, stageMissing)
	}

	reusedBytes := rangeTotalBytes(persistedSet.Ranges())
	if start, settled, err := s.preflightRecoveredStageCollisionLocked(
		ctx, file, parent.dir, physLeaf, partFile, prevEvidence.FileRevision, comparison.Reason, reusedBytes,
	); settled {
		return start, err
	}

	// If metadata agreement bridged across a reopened revision, bind the new
	// revision in the journal so subsequent checkpoints carry current lineage.
	if partial.Revision != desc.FileRevision() || partial.BlockedReason != 0 {
		partial.Revision = desc.FileRevision()
		partial.Metadata = desc.SourceMetadata()
		partial.BlockedReason = 0
		partial.Ranges = persistedSet.Ranges()
		_ = s.op.journal.recordPartial(partial, true)
	}

	return s.admitRecoveredTransactionLocked(
		file, binding, parent, physLeaf, partial.PartName, partFile, persistedSet,
		partial.CheckpointGen, prevEvidence.FileRevision, comparison.Reason, reusedBytes,
	)
}

func (s *Session) resolveFileParentLocked(file transfer.MaterializationFile) (fileParent, error) {
	parent := file.Parent()
	if parent.Kind() == transfer.MaterializationFileParentReference {
		return fileParent{dir: s.op.resultDir}, nil
	}
	state, ok := s.dirsByLogical[parent.Admission().Path()]
	if !ok || !state.admission.Equal(parent.Admission()) || !state.materialized {
		return fileParent{}, transfer.ErrInvalidOutputBinding
	}
	if state.dir != nil {
		return fileParent{state: state, dir: state.dir}, nil
	}
	// Parent directory was already finalized or evicted; reopen from resultDir.
	reopened, err := s.reopenLogicalDirLocked(state.logicalPath)
	if err != nil {
		return fileParent{}, err
	}
	state.dir = reopened
	return fileParent{state: state, dir: reopened}, nil
}

func (s *Session) reopenLogicalDirLocked(logicalPath string) (*Dir, error) {
	phys, err := s.names.resolve(logicalPath)
	if err != nil {
		return nil, err
	}
	dir, _, err := s.openRelativeDir(phys)
	return dir, err
}

func (s *Session) openRelativeDir(physRel string) (*Dir, bool, error) {
	if physRel == "" {
		return s.op.resultDir, false, nil
	}
	cur := s.op.resultDir
	for seg := range strings.SplitSeq(physRel, "/") {
		next, err := openChildDir(cur, seg)
		if cur != s.op.resultDir {
			_ = cur.Close()
		}
		if err != nil {
			return nil, false, err
		}
		cur = next
	}
	return cur, true, nil
}

func fullRangeSet(size uint64) (content.RangeSet, error) {
	if size == 0 {
		return content.NewRangeSet(nil)
	}
	return content.NewRangeSet([]content.Range{{Offset: 0, End: size}})
}

func ownedObjectIDFor(opID receivecontract.OperationID, locator transfer.MaterializationLocatorDigest) transfer.OwnedObjectID {
	h := sha256.New()
	_, _ = h.Write([]byte("windshare/owned-object/v1\x00"))
	_, _ = h.Write(opID.Bytes())
	_, _ = h.Write(locator[:])
	var id transfer.OwnedObjectID
	copy(id[:], h.Sum(nil))
	return id
}

func rangeTotalBytes(ranges []content.Range) uint64 {
	var total uint64
	for _, r := range ranges {
		if r.End > r.Offset {
			total += r.End - r.Offset
		}
	}
	return total
}

func maxRangeEnd(ranges []content.Range) uint64 {
	var maxEnd uint64
	for _, r := range ranges {
		if r.End > maxEnd {
			maxEnd = r.End
		}
	}
	return maxEnd
}
