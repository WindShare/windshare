package output

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"slices"
	"strings"

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
	if settled, exists := s.settledFiles[canonicalPath]; exists {
		s.mu.Unlock()
		return transfer.NewFileSettlementStart(settled)
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
	objectID := ownedObjectIDFor(s.intent.OperationID(), target.Locator().Digest())
	binding, err := transfer.BindFileMaterializationTarget(target, objectID)
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
		s.recordSettledFile(canonicalPath, settlement)
		return transfer.NewFileSettlementStart(settlement)
	}

	// Resolve physical filename inside parentDir (reusing any persisted mapping).
	physLeaf, err := s.names.bindLeaf(canonicalPath, parentDir)
	if err != nil {
		return transfer.FileStart{}, wrapFileFault(ctx, transferfault.OutputNamespaceUnsafe, "map file name", err)
	}

	// 2. Check existing partial progress in the journal.
	if partial, hasPartial := s.op.journal.lookupPartial(canonicalPath); hasPartial {
		return s.resumePartialFile(ctx, file, binding, parentDir, physLeaf, partial)
	}

	// 3. Fresh file: preflight collision before allocating a .part file.
	kind, _, statErr := lstatChild(parentDir, physLeaf)
	if statErr != nil {
		return transfer.FileStart{}, wrapFileFault(ctx, transferfault.OutputStateIO, "inspect destination file", statErr)
	}
	if kind != EntryAbsent {
		settlement, err := transfer.NewCollisionFileSettlement(target)
		if err != nil {
			return transfer.FileStart{}, err
		}
		s.recordSettledFile(canonicalPath, settlement)
		return transfer.NewFileSettlementStart(settlement)
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

func (s *Session) resumePartialFile(
	ctx context.Context,
	file transfer.MaterializationFile,
	binding transfer.MaterializedFileBinding,
	parentDir *Dir,
	physLeaf string,
	partial partialEntry,
) (transfer.FileStart, error) {
	desc := file.Descriptor()
	target := file.Target()
	canonicalPath := file.MaterializationRelativePath().String()

	prevEvidence := content.RevisionSourceEvidence{
		FileRevision:   partial.Revision,
		ExactSize:      partial.ExactSize,
		SourceMetadata: partial.Metadata,
	}
	comparison := content.CompareRevisionMetadata(prevEvidence, desc.SourceEvidence())
	retainedBytes := rangeTotalBytes(partial.Ranges)

	if !comparison.AllowsReuse() {
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
		// Observed source revision changed: block item while preserving partial data.
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
			previousRevision:   partial.Revision,
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
		s.recordSettledFile(canonicalPath, settlement)
		return transfer.NewFileSettlementStart(settlement)
	}

	// Open the staged .part file and validate its size covers persisted ranges.
	partFile, openErr := openChildFile(s.op.opDir, partial.PartName, os.O_RDWR, publicFileMode)
	persistedSlice := slices.Clone(partial.Ranges)
	if openErr != nil {
		// Staged part vanished (e.g., manual cleanup inside .windshare-tmp): recreate fresh.
		partFile, openErr = openChildFile(s.op.opDir, partial.PartName, os.O_RDWR|os.O_CREATE|os.O_EXCL, publicFileMode)
		if openErr != nil {
			return transfer.FileStart{}, wrapFileFault(ctx, transferfault.OutputStateIO, "recreate staged file", openErr)
		}
		persistedSlice = nil
	} else if info, statErr := partFile.Stat(); statErr != nil || !info.Mode().IsRegular() || uint64(max(info.Size(), 0)) < maxRangeEnd(persistedSlice) {
		persistedSlice = nil
	}

	persistedSet, err := content.NewRangeSet(persistedSlice)
	if err != nil {
		persistedSet, _ = content.NewRangeSet(nil)
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
	state, ok := s.dirsByToken[keyForAdmission(parent.Admission())]
	if !ok || !state.materialized {
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
	phys, err := s.names.resolveLocked(logicalPath)
	if err != nil {
		return nil, err
	}
	if phys == "" {
		return s.op.resultDir, nil
	}
	cur := s.op.resultDir
	var toClose []*Dir
	for _, seg := range strings.Split(phys, "/") {
		next, err := openChildDir(cur, seg)
		if err != nil {
			for _, d := range toClose {
				_ = d.Close()
			}
			return nil, err
		}
		if cur != s.op.resultDir {
			toClose = append(toClose, cur)
		}
		cur = next
	}
	for _, d := range toClose {
		_ = d.Close()
	}
	return cur, nil
}

func (s *Session) recordSettledFile(canonicalPath string, settlement transfer.FileSettlement) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.activeFiles, canonicalPath)
	// Paused files remain resumable within the same session (e.g. revision reopen).
	if settlement.Kind() != transfer.FilePaused {
		s.settledFiles[canonicalPath] = settlement
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
