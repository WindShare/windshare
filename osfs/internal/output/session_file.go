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
	if s.closed {
		s.mu.Unlock()
		return transfer.FileStart{}, transfer.ErrDirectTreeSessionFatal
	}
	canonicalPath := file.MaterializationRelativePath().String()
	if outcome, exists := s.settledOutcomes[canonicalPath]; exists {
		s.mu.Unlock()
		return s.replaySettledOutcome(outcome, file.Target())
	}
	if _, active := s.activeFiles[canonicalPath]; active {
		s.mu.Unlock()
		return transfer.FileStart{}, transfer.ErrInvalidOutputBinding
	}
	parentDir, err := s.resolveFileParentLocked(file)
	if err != nil {
		s.mu.Unlock()
		return transfer.FileStart{}, err
	}
	s.mu.Unlock()

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

	// Resolve physical filename inside parentDir (reusing any persisted mapping).
	physLeaf, err := s.names.bindLeaf(canonicalPath, parentDir)
	if err != nil {
		return transfer.FileStart{}, wrapFileFault(ctx, transferfault.OutputNamespaceUnsafe, "map file name", err)
	}

	// 2. Reconcile an interrupted publish (renamed or pending rename before receipt).
	if pub, hasPub := s.op.journal.lookupPublishing(canonicalPath); hasPub {
		return s.reconcilePublishingFile(ctx, file, binding, parentDir, physLeaf, pub)
	}

	// 3. Check existing partial progress in the journal.
	if partial, hasPartial := s.op.journal.lookupPartial(canonicalPath); hasPartial {
		return s.resumePartialFile(ctx, file, binding, parentDir, physLeaf, partial)
	}

	// 4. Fresh file: preflight collision before allocating a .part file.
	return s.beginFreshFile(ctx, file, binding, parentDir, physLeaf)
}

func (s *Session) beginFreshFile(
	ctx context.Context,
	file transfer.MaterializationFile,
	binding transfer.MaterializedFileBinding,
	parentDir *Dir,
	physLeaf string,
) (transfer.FileStart, error) {
	desc := file.Descriptor()
	target := file.Target()
	canonicalPath := file.MaterializationRelativePath().String()

	kind, _, statErr := lstatChild(parentDir, physLeaf)
	if statErr != nil {
		return transfer.FileStart{}, wrapFileFault(ctx, transferfault.OutputStateIO, "inspect destination file", statErr)
	}
	if kind != EntryAbsent {
		return s.settleCollision(canonicalPath, target)
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
	tx := newFileTransaction(s, binding, canonicalPath, parentDir, physLeaf, partName, partFile, emptyRanges, 0)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = partFile.Close()
		_ = removeChild(s.op.opDir, partName, false)
		return transfer.FileStart{}, transfer.ErrDirectTreeSessionFatal
	}
	s.activeFiles[canonicalPath] = tx
	s.mu.Unlock()
	return transfer.NewFileTransactionStart(tx, durable)
}

func (s *Session) settleCollision(canonicalPath string, target transfer.FileMaterializationTarget) (transfer.FileStart, error) {
	settlement, err := transfer.NewCollisionFileSettlement(target)
	if err != nil {
		return transfer.FileStart{}, err
	}
	s.recordSettledOutcome(canonicalPath, nil, settledFileOutcome{
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

func (s *Session) reconcilePublishingFile(
	ctx context.Context,
	file transfer.MaterializationFile,
	binding transfer.MaterializedFileBinding,
	parentDir *Dir,
	physLeaf string,
	pub publishingEntry,
) (transfer.FileStart, error) {
	desc := file.Descriptor()
	target := file.Target()
	canonicalPath := file.MaterializationRelativePath().String()

	if pub.FileID != desc.FileID() {
		_ = removeChild(s.op.opDir, pub.PartName, false)
		_ = s.op.journal.recordRetired(pub.FileID, canonicalPath)
		return s.beginFreshFile(ctx, file, binding, parentDir, physLeaf)
	}

	partKind, _, partErr := lstatChild(s.op.opDir, pub.PartName)
	if partErr != nil {
		return transfer.FileStart{}, wrapFileFault(ctx, transferfault.OutputStateIO, "inspect staged file", partErr)
	}
	if partKind == EntryAbsent {
		return s.reconcilePublishedWithoutStage(ctx, file, binding, parentDir, physLeaf, pub)
	}

	// Stage still exists (interrupted before rename completed): check source
	// lineage before resuming publication with zero re-download.
	prevEvidence := content.RevisionSourceEvidence{
		FileRevision:   pub.Revision,
		ExactSize:      pub.ExactSize,
		SourceMetadata: pub.Metadata,
	}
	comparison := content.CompareRevisionMetadata(prevEvidence, desc.SourceEvidence())
	if !comparison.AllowsReuse() {
		fullSet, _ := fullRangeSet(pub.ExactSize)
		return s.handleRevisionMismatch(file, partialEntry{
			FileID:        pub.FileID,
			Path:          pub.Path,
			Revision:      pub.Revision,
			Metadata:      pub.Metadata,
			ExactSize:     pub.ExactSize,
			PartName:      pub.PartName,
			Ranges:        fullSet.Ranges(),
			CheckpointGen: pub.CheckpointGen,
		}, prevEvidence, comparison)
	}

	partFile, openErr := openChildFile(s.op.opDir, pub.PartName, os.O_RDWR, publicFileMode)
	if openErr != nil {
		return transfer.FileStart{}, wrapFileFault(ctx, transferfault.OutputStateIO, "reopen publishing staged file", openErr)
	}
	validStage := false
	if info, statErr := partFile.Stat(); statErr == nil && info.Mode().IsRegular() && uint64(max(info.Size(), 0)) == pub.ExactSize {
		if fp, fpErr := samplePublicationFingerprint(partFile, pub.ExactSize); fpErr == nil && fp == pub.Fingerprint {
			validStage = true
		}
	}
	if !validStage {
		_ = partFile.Close()
		kind, _, statErr := lstatChild(parentDir, physLeaf)
		if statErr != nil {
			return transfer.FileStart{}, wrapFileFault(ctx, transferfault.OutputStateIO, "inspect destination file", statErr)
		}
		if kind != EntryAbsent {
			return s.settleCollision(canonicalPath, target)
		}
		_ = removeChild(s.op.opDir, pub.PartName, false)
		_ = s.op.journal.recordRetired(pub.FileID, canonicalPath)
		return s.beginFreshFile(ctx, file, binding, parentDir, physLeaf)
	}

	if pub.Revision != desc.FileRevision() {
		pub.Revision = desc.FileRevision()
		pub.Metadata = desc.SourceMetadata()
		_ = s.op.journal.recordPublishing(pub, true)
	}
	fullSet, err := fullRangeSet(pub.ExactSize)
	if err != nil {
		_ = partFile.Close()
		return transfer.FileStart{}, err
	}
	emitTrace(s.authority.diagnostics, traceEvent{
		component:          "file",
		operation:          "recover_file",
		decision:           "reconciled",
		checkpointDecision: "exact",
		operationID:        s.intent.OperationID(),
		intentDigest:       s.intent.Digest(),
		sessionID:          s.sessionID,
		fileID:             desc.FileID(),
		previousRevision:   prevEvidence.FileRevision,
		currentRevision:    desc.FileRevision(),
		recoveryReason:     comparison.Reason,
		reusedBytes:        pub.ExactSize,
	})
	durable, err := transfer.VerifyRestartRanges(binding, transfer.CheckpointGeneration(pub.CheckpointGen), fullSet)
	if err != nil {
		_ = partFile.Close()
		return transfer.FileStart{}, err
	}
	tx := newFileTransaction(s, binding, canonicalPath, parentDir, physLeaf, pub.PartName, partFile, fullSet, pub.CheckpointGen)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = partFile.Close()
		return transfer.FileStart{}, transfer.ErrDirectTreeSessionFatal
	}
	s.activeFiles[canonicalPath] = tx
	s.mu.Unlock()
	return transfer.NewFileTransactionStart(tx, durable)
}

func (s *Session) handleRevisionMismatch(
	file transfer.MaterializationFile,
	partial partialEntry,
	prevEvidence content.RevisionSourceEvidence,
	comparison content.RevisionComparisonEvidence,
) (transfer.FileStart, error) {
	desc := file.Descriptor()
	target := file.Target()
	canonicalPath := file.MaterializationRelativePath().String()
	retainedBytes := rangeTotalBytes(partial.Ranges)

	if comparison.Comparison == content.RevisionComparisonUnavailable {
		emitTrace(s.authority.diagnostics, traceEvent{
			component:        "file",
			operation:        "recover_file",
			decision:         "isolated_failure",
			operationID:      s.intent.OperationID(),
			intentDigest:     s.intent.Digest(),
			sessionID:        s.sessionID,
			fileID:           desc.FileID(),
			previousRevision: partial.Revision,
			currentRevision:  desc.FileRevision(),
			recoveryReason:   comparison.Reason,
			retainedBytes:    retainedBytes,
		})
		srcFault, _ := transferfault.NewSource(transferfault.ScopeFileLocal, transferfault.SourceUnavailable)
		return transfer.FileStart{}, transferfault.Wrap(srcFault, errors.New("revision comparison evidence unavailable"))
	}
	partial.BlockedReason = transfer.ItemBlockRevisionConflict
	_ = s.op.journal.recordPartial(partial, true)
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
	s.recordSettledOutcome(canonicalPath, nil, settledFileOutcome{
		fileID:          target.FileID(),
		revision:        target.FileRevision(),
		kind:            transfer.FileItemBlocked,
		itemBlockReason: transfer.ItemBlockRevisionConflict,
	})
	return transfer.NewFileSettlementStart(settlement)
}

func (s *Session) reconcilePublishedWithoutStage(
	ctx context.Context,
	file transfer.MaterializationFile,
	binding transfer.MaterializedFileBinding,
	parentDir *Dir,
	physLeaf string,
	pub publishingEntry,
) (transfer.FileStart, error) {
	canonicalPath := file.MaterializationRelativePath().String()
	reconciled, collided, err := s.tryReconcilePublished(ctx, pub, parentDir, physLeaf)
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
		return s.settleCollision(canonicalPath, file.Target())
	}
	_ = s.op.journal.recordRetired(pub.FileID, canonicalPath)
	return s.beginFreshFile(ctx, file, binding, parentDir, physLeaf)
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

func (s *Session) restartInvalidPartialStage(
	ctx context.Context,
	file transfer.MaterializationFile,
	binding transfer.MaterializedFileBinding,
	parentDir *Dir,
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
	kind, _, statErr := lstatChild(parentDir, physLeaf)
	if statErr != nil {
		return transfer.FileStart{}, wrapFileFault(ctx, transferfault.OutputStateIO, "inspect destination file", statErr)
	}
	if kind != EntryAbsent {
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
		return s.settleCollision(canonicalPath, file.Target())
	}
	if !stageMissing {
		if err := removeChild(s.op.opDir, partial.PartName, false); err != nil {
			return transfer.FileStart{}, wrapFileFault(ctx, transferfault.OutputStateIO, "remove invalid staged file", err)
		}
	}
	_ = s.op.journal.recordRetired(partial.FileID, canonicalPath)
	return s.beginFreshFile(ctx, file, binding, parentDir, physLeaf)
}

func (s *Session) resumePartialFile(
	ctx context.Context,
	file transfer.MaterializationFile,
	binding transfer.MaterializedFileBinding,
	parentDir *Dir,
	physLeaf string,
	partial partialEntry,
) (transfer.FileStart, error) {
	desc := file.Descriptor()
	canonicalPath := file.MaterializationRelativePath().String()

	if partial.FileID != desc.FileID() {
		_ = removeChild(s.op.opDir, partial.PartName, false)
		_ = s.op.journal.recordRetired(partial.FileID, canonicalPath)
		return s.beginFreshFile(ctx, file, binding, parentDir, physLeaf)
	}

	prevEvidence := content.RevisionSourceEvidence{
		FileRevision:   partial.Revision,
		ExactSize:      partial.ExactSize,
		SourceMetadata: partial.Metadata,
	}
	comparison := content.CompareRevisionMetadata(prevEvidence, desc.SourceEvidence())
	if !comparison.AllowsReuse() {
		return s.handleRevisionMismatch(file, partial, prevEvidence, comparison)
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
		return s.restartInvalidPartialStage(ctx, file, binding, parentDir, physLeaf, partial, stageMissing)
	}

	reusedBytes := rangeTotalBytes(persistedSet.Ranges())

	// If metadata agreement bridged across a reopened revision, bind the new
	// revision in the journal so subsequent checkpoints carry current lineage.
	if partial.Revision != desc.FileRevision() || partial.BlockedReason != 0 {
		partial.Revision = desc.FileRevision()
		partial.Metadata = desc.SourceMetadata()
		partial.BlockedReason = 0
		partial.Ranges = persistedSet.Ranges()
		_ = s.op.journal.recordPartial(partial, true)
	}

	emitTrace(s.authority.diagnostics, traceEvent{
		component:          "file",
		operation:          "recover_file",
		decision:           "reconciled",
		checkpointDecision: "exact",
		operationID:        s.intent.OperationID(),
		intentDigest:       s.intent.Digest(),
		sessionID:          s.sessionID,
		fileID:             desc.FileID(),
		previousRevision:   prevEvidence.FileRevision,
		currentRevision:    desc.FileRevision(),
		recoveryReason:     comparison.Reason,
		reusedBytes:        reusedBytes,
	})

	durable, err := transfer.VerifyRestartRanges(binding, transfer.CheckpointGeneration(partial.CheckpointGen), persistedSet)
	if err != nil {
		_ = partFile.Close()
		return transfer.FileStart{}, err
	}
	tx := newFileTransaction(s, binding, canonicalPath, parentDir, physLeaf, partial.PartName, partFile, persistedSet, partial.CheckpointGen)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = partFile.Close()
		return transfer.FileStart{}, transfer.ErrDirectTreeSessionFatal
	}
	s.activeFiles[canonicalPath] = tx
	s.mu.Unlock()
	return transfer.NewFileTransactionStart(tx, durable)
}

func (s *Session) resolveFileParentLocked(file transfer.MaterializationFile) (*Dir, error) {
	parent := file.Parent()
	if parent.Kind() == transfer.MaterializationFileParentReference {
		return s.op.resultDir, nil
	}
	state, ok := s.dirsByLogical[parent.Admission().Path()]
	if !ok || !state.admission.Equal(parent.Admission()) || !state.materialized {
		return nil, transfer.ErrInvalidOutputBinding
	}
	if state.dir != nil {
		return state.dir, nil
	}
	// Parent directory was already finalized or evicted; reopen from resultDir.
	reopened, err := s.reopenLogicalDirLocked(state.logicalPath)
	if err != nil {
		return nil, err
	}
	state.dir = reopened
	return reopened, nil
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

func (s *Session) releaseActiveFile(canonicalPath string, tx *fileTransaction) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeFiles[canonicalPath] == tx {
		delete(s.activeFiles, canonicalPath)
	}
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
