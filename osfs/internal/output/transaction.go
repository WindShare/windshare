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
	SmallFileThreshold           uint64 = 8 << 20 // 8 MiB
	LargeCheckpointBytes         uint64 = 8 << 20 // 8 MiB
	LargeCheckpointInterval             = 2 * time.Second
	publicationFingerprintDomain        = "windshare/publication-samples/v1\x00"
	publicationSampleBytes       uint64 = 4096
)

// samplePublicationFingerprint hashes the file size together with up to three
// 4 KiB slices (head, middle, tail). It provides bounded O(1) reconciliation
// evidence for an interrupted publish, never authority to overwrite or delete
// an existing destination file.
func samplePublicationFingerprint(reader io.ReaderAt, size uint64) ([publicationFingerprintBytes]byte, error) {
	h := sha256.New()
	_, _ = h.Write([]byte(publicationFingerprintDomain))
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], size)
	_, _ = h.Write(encoded[:])

	length := min(size, publicationSampleBytes)
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
			if err != nil || n != len(buf) {
				if err == nil {
					err = io.ErrUnexpectedEOF
				}
				return [publicationFingerprintBytes]byte{}, err
			}
			binary.BigEndian.PutUint64(encoded[:], off)
			_, _ = h.Write(encoded[:])
			_, _ = h.Write(buf)
		}
	}
	var sum [publicationFingerprintBytes]byte
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

type fileTransaction struct {
	session       *Session
	binding       transfer.MaterializedFileBinding
	canonicalPath string
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
	parentDir *Dir,
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
		parentDir:      parentDir,
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
	tx.persistedGen++
	tx.persisted = tx.accepted
	tx.unpersistedBytes = 0
	tx.lastCheckpoint = tx.session.authority.now()
	desc := tx.binding.Descriptor()
	return tx.session.op.journal.recordPartial(partialEntry{
		FileID:        desc.FileID(),
		Path:          tx.canonicalPath,
		Revision:      desc.FileRevision(),
		Metadata:      desc.SourceMetadata(),
		ExactSize:     tx.exactSize,
		PartName:      tx.partName,
		Ranges:        tx.persisted.Ranges(),
		CheckpointGen: tx.persistedGen,
	}, true)
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
		return tx.settleCommitCollisionLocked()
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

func (tx *fileTransaction) settleCommitCollisionLocked() (transfer.FileSettlement, error) {
	// Preserve .part in .windshare-tmp/<opID>/ so retry or discard stays safe.
	tx.persistedGen++
	tx.persisted = tx.accepted
	desc := tx.binding.Descriptor()
	_ = tx.session.op.journal.recordPartial(partialEntry{
		FileID:        desc.FileID(),
		Path:          tx.canonicalPath,
		Revision:      desc.FileRevision(),
		Metadata:      desc.SourceMetadata(),
		ExactSize:     tx.exactSize,
		PartName:      tx.partName,
		Ranges:        tx.persisted.Ranges(),
		CheckpointGen: tx.persistedGen,
	}, true)
	settlement, err := transfer.NewTransactionCollisionFileSettlement(tx.binding)
	if err != nil {
		return transfer.FileSettlement{}, err
	}
	tx.settled = true
	tx.session.recordSettledFile(tx.canonicalPath, settlement)
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
	tx.session.recordSettledFile(tx.canonicalPath, settlement)
	return settlement, nil
}

func (tx *fileTransaction) Pause(ctx context.Context, _ transfer.FilePauseReason) (transfer.FileSettlement, error) {
	if ctx == nil {
		return transfer.FileSettlement{}, transfer.ErrInvalidOutputBinding
	}
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.settled {
		return transfer.FileSettlement{}, transfer.ErrInvalidOutputBinding
	}
	var persistErr error
	if tx.file != nil {
		if tx.unpersistedBytes > 0 || (tx.persistedGen == 0 && !tx.accepted.IsEmpty()) {
			persistErr = tx.persistLocked()
		}
		persistErr = errors.Join(persistErr, tx.file.Close())
		tx.file = nil
	}
	if persistErr != nil {
		tx.settled = true
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
	tx.session.recordSettledFile(tx.canonicalPath, settlement)
	return settlement, nil
}

func (tx *fileTransaction) Retire(ctx context.Context, _ transfer.FileRetireReason) (transfer.FileSettlement, error) {
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
	tx.session.recordSettledFile(tx.canonicalPath, settlement)
	return settlement, nil
}
