package output

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"sync"
	"time"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

const (
	// SmallFileThreshold separates single-shot small files (0 per-file fsync on
	// the hot path; batched journal sync) from large files that stream range
	// checkpoints to disk during transfer.
	SmallFileThreshold   uint64 = 8 << 20 // 8 MiB
	LargeCheckpointBytes uint64 = 8 << 20 // 8 MiB
	// LargeCheckpointInterval caps checkpoint lag during slow trickle writes
	// without spawning background idle timers; it is checked passively in Accept
	// while Pause and Commit flush any remaining bytes on stall, stop, or finish.
	LargeCheckpointInterval             = 2 * time.Second
	publicationFingerprintDomain        = "windshare/publication-samples/v1\x00"
	stageRangeFingerprintDomain         = "windshare/stage-range-samples/v1\x00"
	stageSampleBytes             uint64 = 4096
)

var errStageSizeMismatch = errors.New("osfs: staged file size does not match expected length")

// samplePublicationFingerprint hashes the file size together with up to three
// 4 KiB slices (head, middle, tail). It provides bounded O(1) reconciliation
// evidence for an interrupted publish, never authority to overwrite or delete
// an existing destination file.
func samplePublicationFingerprint(reader io.ReaderAt, size uint64) ([stageFingerprintBytes]byte, error) {
	h := sha256.New()
	_, _ = h.Write([]byte(publicationFingerprintDomain))
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], size)
	_, _ = h.Write(encoded[:])

	length := min(size, stageSampleBytes)
	if length > 0 {
		offsets := [3]uint64{0, (size - length) / 2, size - length}
		prev := ^uint64(0)
		buf := make([]byte, length)
		for _, off := range offsets {
			if off == prev {
				continue
			}
			prev = off
			n, err := reader.ReadAt(buf, int64(off))
			if n != len(buf) || (err != nil && !errors.Is(err, io.EOF)) {
				if err == nil {
					err = io.ErrUnexpectedEOF
				}
				return [stageFingerprintBytes]byte{}, err
			}
			binary.BigEndian.PutUint64(encoded[:], off)
			_, _ = h.Write(encoded[:])
			_, _ = h.Write(buf)
		}
	}
	var sum [stageFingerprintBytes]byte
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// stageRangeSampleWindows chooses up to three deduplicated <=4 KiB slices
// strictly inside ranges (head, covered midpoint, tail) so sampling never reads
// unwritten holes between non-contiguous ranges.
func stageRangeSampleWindows(ranges []content.Range) []content.Range {
	total := rangeTotalBytes(ranges)
	if total == 0 {
		return nil
	}
	var windows []content.Range
	appendUnique := func(candidate content.Range) {
		if candidate.End <= candidate.Offset {
			return
		}
		for _, existing := range windows {
			if existing == candidate {
				return
			}
		}
		windows = append(windows, candidate)
	}

	first := ranges[0]
	appendUnique(content.Range{
		Offset: first.Offset,
		End:    first.Offset + min(first.End-first.Offset, stageSampleBytes),
	})

	midPos := total / 2
	var acc uint64
	for _, r := range ranges {
		span := r.End - r.Offset
		if midPos < acc+span {
			length := min(span, stageSampleBytes)
			anchor := r.Offset + (midPos - acc)
			half := length / 2
			var start uint64
			switch {
			case anchor <= r.Offset+half:
				start = r.Offset
			case r.End-(anchor-half) < length:
				start = r.End - length
			default:
				start = anchor - half
			}
			appendUnique(content.Range{Offset: start, End: start + length})
			break
		}
		acc += span
	}

	last := ranges[len(ranges)-1]
	tailLen := min(last.End-last.Offset, stageSampleBytes)
	appendUnique(content.Range{
		Offset: last.End - tailLen,
		End:    last.End,
	})
	return windows
}

// sampleStageRangesFingerprint binds covered range geometry to up to three
// 4 KiB slices sampled strictly inside ranges. Omitting total file length and
// mtime preserves crash recovery after later in-place writes while catching
// restored, sync-replaced, or zeroed staging files in bounded O(1) I/O.
func sampleStageRangesFingerprint(reader io.ReaderAt, ranges []content.Range) ([stageFingerprintBytes]byte, error) {
	for _, r := range ranges {
		if r.Offset >= r.End {
			return [stageFingerprintBytes]byte{}, content.ErrNonCanonicalRange
		}
	}
	windows := stageRangeSampleWindows(ranges)
	if len(windows) == 0 {
		return [stageFingerprintBytes]byte{}, nil
	}
	h := sha256.New()
	_, _ = h.Write([]byte(stageRangeFingerprintDomain))
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(len(ranges)))
	_, _ = h.Write(encoded[:])
	for _, r := range ranges {
		binary.BigEndian.PutUint64(encoded[:], r.Offset)
		_, _ = h.Write(encoded[:])
		binary.BigEndian.PutUint64(encoded[:], r.End)
		_, _ = h.Write(encoded[:])
	}
	buf := make([]byte, stageSampleBytes)
	for _, w := range windows {
		slice := buf[:w.End-w.Offset]
		n, err := reader.ReadAt(slice, int64(w.Offset))
		if n != len(slice) || (err != nil && !errors.Is(err, io.EOF)) {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return [stageFingerprintBytes]byte{}, err
		}
		binary.BigEndian.PutUint64(encoded[:], w.Offset)
		_, _ = h.Write(encoded[:])
		binary.BigEndian.PutUint64(encoded[:], uint64(len(slice)))
		_, _ = h.Write(encoded[:])
		_, _ = h.Write(slice)
	}
	var sum [stageFingerprintBytes]byte
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

func verifyStagedFileSize(f *os.File, expected uint64) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || uint64(info.Size()) != expected {
		return errStageSizeMismatch
	}
	return nil
}

type fileTransaction struct {
	session       *Session
	binding       transfer.MaterializedFileBinding
	canonicalPath string
	parentState   *dirState
	parentDir     *Dir
	physLeaf      string
	partName      string
	exactSize     uint64

	mu               sync.Mutex
	file             *os.File
	accepted         content.RangeSet
	acceptedGen      uint64
	persisted        content.RangeSet
	persistedGen     uint64
	resumedBytes     uint64
	unpersistedBytes uint64
	lastCheckpoint   time.Time
	settled          bool
}

var _ transfer.FileTransaction = (*fileTransaction)(nil)

func newFileTransaction(
	session *Session,
	binding transfer.MaterializedFileBinding,
	canonicalPath string,
	parent fileParent,
	physLeaf string,
	partName string,
	file *os.File,
	persisted content.RangeSet,
	persistedGen uint64,
) *fileTransaction {
	return &fileTransaction{
		session:        session,
		binding:        binding,
		canonicalPath:  canonicalPath,
		parentState:    parent.state,
		parentDir:      parent.dir,
		physLeaf:       physLeaf,
		partName:       partName,
		exactSize:      binding.ExactSize(),
		file:           file,
		accepted:       persisted,
		acceptedGen:    persistedGen,
		persisted:      persisted,
		persistedGen:   persistedGen,
		resumedBytes:   rangeTotalBytes(persisted.Ranges()),
		lastCheckpoint: session.authority.now(),
	}
}

func (tx *fileTransaction) Binding() transfer.MaterializedFileBinding { return tx.binding }

func (tx *fileTransaction) WriteRange(ctx context.Context, offset uint64, data []byte) error {
	if ctx == nil {
		return transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.settled || tx.file == nil {
		return transfer.ErrInvalidOutputBinding
	}
	if offset > tx.exactSize || uint64(len(data)) > tx.exactSize-offset {
		return ErrOutOfRange
	}
	if len(data) == 0 {
		return nil
	}
	if _, err := tx.file.WriteAt(data, int64(offset)); err != nil {
		return wrapFileFault(ctx, transferfault.OutputStateIO, "write staged file range", err)
	}
	added, err := content.NewRangeSet([]content.Range{{Offset: offset, End: offset + uint64(len(data))}})
	if err != nil {
		return err
	}
	merged, err := transfer.MergeRanges(tx.accepted, added)
	if err != nil {
		return err
	}
	tx.accepted = merged
	tx.unpersistedBytes += uint64(len(data))
	return nil
}

func (tx *fileTransaction) Accept(ctx context.Context) (transfer.FileProgress, error) {
	if ctx == nil {
		return transfer.FileProgress{}, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return transfer.FileProgress{}, err
	}
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.settled || tx.file == nil {
		return transfer.FileProgress{}, transfer.ErrInvalidOutputBinding
	}

	// Only large files flush periodic checkpoints during transfer; small files
	// skip intermediate fsyncs and commit in one atomic rename.
	if tx.exactSize >= SmallFileThreshold && tx.unpersistedBytes > 0 {
		now := tx.session.authority.now()
		if tx.unpersistedBytes >= LargeCheckpointBytes || now.Sub(tx.lastCheckpoint) >= LargeCheckpointInterval {
			if err := tx.persistLocked(); err != nil {
				return transfer.FileProgress{}, DiagnoseFailure(FailureCheckpoint, wrapFileFault(ctx, transferfault.OutputStateIO, "checkpoint large file", err))
			}
		}
	}

	// In-process acknowledgements and restart-durable checkpoints advance on
	// independent monotonic clocks so small files can acknowledge ranges without
	// per-chunk fsync while still honoring the DurabilityProcessRestart contract.
	tx.acceptedGen++
	inProc, err := transfer.VerifyProcessRanges(tx.binding, transfer.CheckpointGeneration(tx.acceptedGen), tx.accepted)
	if err != nil {
		return transfer.FileProgress{}, err
	}
	durable, err := transfer.VerifyRestartRanges(tx.binding, transfer.CheckpointGeneration(tx.persistedGen), tx.persisted)
	if err != nil {
		return transfer.FileProgress{}, err
	}
	return transfer.NewFileProgress(inProc, durable)
}

func (tx *fileTransaction) Checkpoint(ctx context.Context) (transfer.VerifiedRanges, error) {
	if ctx == nil {
		return transfer.VerifiedRanges{}, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return transfer.VerifiedRanges{}, err
	}
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.settled || tx.file == nil {
		return transfer.VerifiedRanges{}, transfer.ErrInvalidOutputBinding
	}
	if tx.unpersistedBytes > 0 || tx.persistedGen == 0 {
		if err := tx.persistLocked(); err != nil {
			return transfer.VerifiedRanges{}, DiagnoseFailure(FailureCheckpoint, wrapFileFault(ctx, transferfault.OutputStateIO, "checkpoint file", err))
		}
	}
	return transfer.VerifyRestartRanges(tx.binding, transfer.CheckpointGeneration(tx.persistedGen), tx.persisted)
}

func (tx *fileTransaction) persistLocked() error {
	if err := tx.file.Sync(); err != nil {
		return err
	}
	ranges := tx.accepted.Ranges()
	info, err := tx.file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 ||
		uint64(info.Size()) < maxRangeEnd(ranges) || uint64(info.Size()) > tx.exactSize {
		return errStageSizeMismatch
	}
	fp, err := sampleStageRangesFingerprint(tx.file, ranges)
	if err != nil {
		return err
	}
	nextGen := tx.persistedGen + 1
	desc := tx.binding.Descriptor()
	if err := tx.session.op.journal.recordPartial(partialEntry{
		FileID:        desc.FileID(),
		Path:          tx.canonicalPath,
		Revision:      desc.FileRevision(),
		Metadata:      desc.SourceMetadata(),
		ExactSize:     tx.exactSize,
		PartName:      tx.partName,
		Ranges:        ranges,
		CheckpointGen: nextGen,
		Fingerprint:   fp,
	}, true); err != nil {
		return err
	}
	// Advance durability state only after the journal append and sync succeed so
	// a failed checkpoint never reports unpersisted progress or suppresses retry.
	tx.persistedGen = nextGen
	tx.persisted = tx.accepted
	tx.unpersistedBytes = 0
	tx.lastCheckpoint = tx.session.authority.now()
	return nil
}

func (tx *fileTransaction) Commit(ctx context.Context) (transfer.FileSettlement, error) {
	if ctx == nil {
		return transfer.FileSettlement{}, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return transfer.FileSettlement{}, err
	}
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.settled || tx.file == nil {
		return transfer.FileSettlement{}, transfer.ErrInvalidOutputBinding
	}
	if !transfer.RangesCoverFile(tx.exactSize, tx.accepted) {
		return transfer.FileSettlement{}, transfer.ErrIncompleteMaterializationFile
	}

	isLarge := tx.exactSize >= SmallFileThreshold
	// Only large files and files that already hold a durable partial entry pay
	// the pre-publish intent cost; one-shot small files keep the 0-fsync path.
	durablePublish := isLarge || tx.persistedGen > 0

	pubEntry, warnings, err := tx.prepareStageForPublishLocked(ctx, durablePublish)
	if err != nil {
		return transfer.FileSettlement{}, err
	}

	if durablePublish {
		// Persist the publishing intent and fingerprint before rename so a
		// journal write failure or crash after rename can reconcile the
		// published file instead of redownloading from zero and colliding.
		if err := tx.session.op.journal.recordPublishing(pubEntry, true); err != nil {
			return transfer.FileSettlement{}, DiagnoseFailure(FailureDurability, wrapFileFault(ctx, transferfault.OutputStateIO, "record publishing intent", err))
		}
	}

	renameErr := renameFileNoReplace(tx.session.op.opDir, tx.partName, tx.parentDir, tx.physLeaf, false)
	if errors.Is(renameErr, ErrNamespaceCollision) || errors.Is(renameErr, fs.ErrExist) {
		return tx.settleCommitCollisionLocked(durablePublish)
	}
	if renameErr != nil {
		return transfer.FileSettlement{}, wrapFileFault(ctx, transferfault.OutputStateIO, "publish file", renameErr)
	}

	desc := tx.binding.Descriptor()
	if err := tx.session.op.journal.recordDelivered(deliveredEntry{
		FileID:    desc.FileID(),
		Path:      tx.canonicalPath,
		Revision:  desc.FileRevision(),
		ExactSize: tx.exactSize,
	}, isLarge); err != nil {
		return transfer.FileSettlement{}, DiagnoseFailure(FailureDurability, wrapFileFault(ctx, transferfault.OutputStateIO, "record delivery receipt", err))
	}

	return tx.settlePublishedLocked(warnings)
}

func (tx *fileTransaction) prepareStageForPublishLocked(
	ctx context.Context,
	durablePublish bool,
) (publishingEntry, []transfer.FileMetadataWarning, error) {
	if err := verifyStagedFileSize(tx.file, tx.exactSize); err != nil {
		// Detach the corrupted stage so PauseTree does not checkpoint an
		// externally resized .part after Commit refuses to publish it.
		_ = tx.file.Close()
		tx.file = nil
		tx.settled = true
		tx.session.releaseActiveFile(tx.canonicalPath, tx)
		return publishingEntry{}, nil, DiagnoseFailure(FailureDurability, wrapFileFault(ctx, transferfault.OutputStateIO, "verify staged file size", err))
	}
	var pubEntry publishingEntry
	if durablePublish {
		fp, err := samplePublicationFingerprint(tx.file, tx.exactSize)
		if err != nil {
			return publishingEntry{}, nil, DiagnoseFailure(FailureDurability, wrapFileFault(ctx, transferfault.OutputStateIO, "fingerprint staged file", err))
		}
		desc := tx.binding.Descriptor()
		pubEntry = publishingEntry{
			FileID:        desc.FileID(),
			Path:          tx.canonicalPath,
			Revision:      desc.FileRevision(),
			Metadata:      desc.SourceMetadata(),
			ExactSize:     tx.exactSize,
			PartName:      tx.partName,
			CheckpointGen: tx.persistedGen + 1,
			Fingerprint:   fp,
		}
	}

	var warnings []transfer.FileMetadataWarning
	if mod := tx.binding.Descriptor().ModifiedTime(); mod.Present() {
		if err := setFileModifiedTimeCatalog(tx.file, mod); err != nil {
			warnings = append(warnings, transfer.FileMetadataModifiedTime)
		}
	}

	if durablePublish {
		if err := tx.file.Sync(); err != nil {
			return publishingEntry{}, nil, DiagnoseFailure(FailureDurability, wrapFileFault(ctx, transferfault.OutputStateIO, "sync staged file", err))
		}
	}

	// Close before atomic rename (required on Windows and clean on Unix).
	closeErr := tx.file.Close()
	tx.file = nil
	if closeErr != nil {
		return publishingEntry{}, nil, wrapFileFault(ctx, transferfault.OutputStateIO, "close staged file", closeErr)
	}
	return pubEntry, warnings, nil
}

func sampleCollisionStageFingerprint(
	opDir *Dir,
	partName string,
	exactSize uint64,
	ranges []content.Range,
) ([stageFingerprintBytes]byte, bool) {
	if len(ranges) == 0 {
		return [stageFingerprintBytes]byte{}, true
	}
	f, err := openChildFile(opDir, partName, os.O_RDONLY, 0)
	if err != nil {
		return [stageFingerprintBytes]byte{}, false
	}
	defer f.Close()
	if verifyStagedFileSize(f, exactSize) != nil {
		return [stageFingerprintBytes]byte{}, false
	}
	fp, err := sampleStageRangesFingerprint(f, ranges)
	if err != nil || fp == ([stageFingerprintBytes]byte{}) {
		return [stageFingerprintBytes]byte{}, false
	}
	return fp, true
}

func (tx *fileTransaction) settleCommitCollisionLocked(durablePublish bool) (transfer.FileSettlement, error) {
	// Preserve .part in .windshare-tmp/<opID>/ so retry or discard stays safe.
	// Durable-publish files already persisted a verified publishing record before
	// rename; keep it intact rather than reopening .part and risking a downgrade
	// to partial on a transient post-close I/O or AV lock error.
	if !durablePublish {
		ranges := tx.accepted.Ranges()
		if fp, ok := sampleCollisionStageFingerprint(tx.session.op.opDir, tx.partName, tx.exactSize, ranges); ok {
			nextGen := tx.persistedGen + 1
			desc := tx.binding.Descriptor()
			if err := tx.session.op.journal.recordPartial(partialEntry{
				FileID:        desc.FileID(),
				Path:          tx.canonicalPath,
				Revision:      desc.FileRevision(),
				Metadata:      desc.SourceMetadata(),
				ExactSize:     tx.exactSize,
				PartName:      tx.partName,
				Ranges:        ranges,
				CheckpointGen: nextGen,
				Fingerprint:   fp,
			}, true); err == nil {
				tx.persistedGen = nextGen
				tx.persisted = tx.accepted
				tx.unpersistedBytes = 0
			}
		}
	}
	settlement, err := transfer.NewTransactionCollisionFileSettlement(tx.binding)
	if err != nil {
		return transfer.FileSettlement{}, err
	}
	tx.settled = true
	tx.session.recordSettledOutcome(tx.canonicalPath, tx, settledFileOutcome{
		fileID:   tx.binding.FileID(),
		revision: tx.binding.FileRevision(),
		kind:     transfer.FileCollision,
		bound:    true,
	})
	return settlement, nil
}

func (tx *fileTransaction) settlePublishedLocked(warnings []transfer.FileMetadataWarning) (transfer.FileSettlement, error) {
	commitGen := tx.persistedGen + 1
	verified, err := transfer.VerifyRestartRanges(tx.binding, transfer.CheckpointGeneration(commitGen), tx.accepted)
	if err != nil {
		return transfer.FileSettlement{}, err
	}
	settlement, err := transfer.NewVerifiedFileSettlement(transfer.FilePublished, verified)
	if err != nil {
		return transfer.FileSettlement{}, err
	}
	provenance := transfer.FileDownloaded
	if tx.resumedBytes > 0 {
		provenance = transfer.FileResumed
	}
	settlement, err = settlement.WithPublicationProvenance(provenance)
	if err != nil {
		return transfer.FileSettlement{}, err
	}
	if len(warnings) > 0 {
		settlement, err = settlement.WithMetadataWarnings(warnings)
		if err != nil {
			return transfer.FileSettlement{}, err
		}
	}
	tx.settled = true
	// Delivery receipt is already recorded in op.journal; release the active
	// slot without retaining a per-file FileSettlement copy in Session.
	tx.session.releaseActiveFile(tx.canonicalPath, tx)
	return settlement, nil
}

func (tx *fileTransaction) persistAndCloseForPauseLocked() error {
	var persistErr error
	if tx.file != nil {
		if tx.unpersistedBytes > 0 || (tx.persistedGen == 0 && !tx.accepted.IsEmpty()) {
			persistErr = tx.persistLocked()
		}
		persistErr = errors.Join(persistErr, tx.file.Close())
		tx.file = nil
	}
	return persistErr
}

func (tx *fileTransaction) Pause(ctx context.Context) (transfer.FileSettlement, error) {
	if ctx == nil {
		return transfer.FileSettlement{}, transfer.ErrInvalidOutputBinding
	}
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.settled {
		return transfer.FileSettlement{}, transfer.ErrInvalidOutputBinding
	}
	if persistErr := tx.persistAndCloseForPauseLocked(); persistErr != nil {
		tx.settled = true
		// Detach the closed transaction so a subsequent PauseTree does not
		// re-enter Pause and turn a durability error into an invalid-binding fault.
		tx.session.releaseActiveFile(tx.canonicalPath, tx)
		return transfer.FileSettlement{}, DiagnoseFailure(FailureDurability, wrapFileFault(ctx, transferfault.OutputStateIO, "pause staged file", persistErr))
	}
	verified, err := transfer.VerifyRestartRanges(tx.binding, transfer.CheckpointGeneration(tx.persistedGen), tx.persisted)
	if err != nil {
		return transfer.FileSettlement{}, err
	}
	settlement, err := transfer.NewVerifiedFileSettlement(transfer.FilePaused, verified)
	if err != nil {
		return transfer.FileSettlement{}, err
	}
	tx.settled = true
	// Paused files remain resumable within the same session (e.g. revision reopen).
	tx.session.releaseActiveFile(tx.canonicalPath, tx)
	return settlement, nil
}

// pauseForSessionClose drains an active transaction snapshotted by Session.PauseTree.
// Because PauseTree releases s.mu before acquiring tx.mu (lock order: tx.mu -> s.mu),
// a concurrent Commit/Pause/Retire may settle and detach tx before this call runs;
// observing tx.settled under tx.mu is therefore an expected no-op rather than a
// caller contract violation.
func (tx *fileTransaction) pauseForSessionClose() error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.settled {
		return nil
	}
	persistErr := tx.persistAndCloseForPauseLocked()
	tx.settled = true
	tx.session.releaseActiveFile(tx.canonicalPath, tx)
	if persistErr != nil {
		return wrapOutputFaultError(transferfault.OutputStateIO, "pause staged file", persistErr)
	}
	return nil
}

func (tx *fileTransaction) Retire(ctx context.Context) (transfer.FileSettlement, error) {
	if ctx == nil {
		return transfer.FileSettlement{}, transfer.ErrInvalidOutputBinding
	}
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.settled {
		return transfer.FileSettlement{}, transfer.ErrInvalidOutputBinding
	}
	var err error
	if tx.file != nil {
		err = errors.Join(err, tx.file.Close())
		tx.file = nil
	}
	err = errors.Join(err, removeChild(tx.session.op.opDir, tx.partName, false))
	err = errors.Join(err, tx.session.op.journal.recordRetired(tx.binding.FileID(), tx.canonicalPath))
	if err != nil {
		return transfer.FileSettlement{}, wrapFileFault(ctx, transferfault.OutputStateIO, "retire staged file", err)
	}
	settlement, err := transfer.NewFailedFileSettlement(tx.binding)
	if err != nil {
		return transfer.FileSettlement{}, err
	}
	tx.settled = true
	tx.session.recordSettledOutcome(tx.canonicalPath, tx, settledFileOutcome{
		fileID:   tx.binding.FileID(),
		revision: tx.binding.FileRevision(),
		kind:     transfer.FileFailed,
		bound:    true,
	})
	return settlement, nil
}
