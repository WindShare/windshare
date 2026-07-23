package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

const (
	failureMetaBodyBytes = 152
	failureMetaBytes     = failureMetaBodyBytes + sha256.Size
	failureMetaName      = "failure.meta"
	failureObjectName    = "failure.object"
	failureStorageSchema = uint16(1)

	failureDirectoryReadBatch = 128
)

var failureMagic = [4]byte{'W', 'S', 'F', '2'}

type fileFailureMeta struct {
	record     DirectoryFailureRecord
	commitment [sha256.Size]byte
	objectSize uint64
	spillBytes uint64
}

func encodeFileFailureMeta(meta fileFailureMeta) []byte {
	encoded := make([]byte, failureMetaBytes)
	copy(encoded[0:4], failureMagic[:])
	binary.BigEndian.PutUint16(encoded[4:6], failureStorageSchema)
	copy(encoded[8:24], meta.record.share[:])
	copy(encoded[24:40], meta.record.directory[:])
	copy(encoded[40:56], meta.record.attempt[:])
	copy(encoded[56:72], meta.record.generation[:])
	copy(encoded[72:88], meta.record.previousAttempt[:])
	encoded[88] = byte(meta.record.kind)
	if meta.record.Retryable() {
		encoded[89] = 1
		binary.BigEndian.PutUint64(encoded[96:104], uint64(meta.record.retryAfter/time.Millisecond))
	}
	binary.BigEndian.PutUint64(encoded[104:112], meta.objectSize)
	copy(encoded[112:144], meta.commitment[:])
	binary.BigEndian.PutUint64(encoded[144:152], meta.spillBytes)
	checksum := sha256.Sum256(encoded[:failureMetaBodyBytes])
	copy(encoded[failureMetaBodyBytes:], checksum[:])
	return encoded
}

func decodeFileFailureMeta(encoded []byte) (fileFailureMeta, error) {
	if len(encoded) != failureMetaBytes {
		return fileFailureMeta{}, ErrCorruptCatalogStorage
	}
	var storedChecksum [sha256.Size]byte
	copy(storedChecksum[:], encoded[failureMetaBodyBytes:])
	if sha256.Sum256(encoded[:failureMetaBodyBytes]) != storedChecksum ||
		string(encoded[0:4]) != string(failureMagic[:]) ||
		binary.BigEndian.Uint16(encoded[4:6]) != failureStorageSchema || encoded[6] != 0 || encoded[7] != 0 {
		return fileFailureMeta{}, ErrCorruptCatalogStorage
	}
	var meta fileFailureMeta
	copy(meta.record.share[:], encoded[8:24])
	copy(meta.record.directory[:], encoded[24:40])
	copy(meta.record.attempt[:], encoded[40:56])
	copy(meta.record.generation[:], encoded[56:72])
	copy(meta.record.previousAttempt[:], encoded[72:88])
	meta.record.kind = FailureKind(encoded[88])
	retryable := encoded[89]
	for _, reserved := range encoded[90:96] {
		if reserved != 0 {
			return fileFailureMeta{}, ErrCorruptCatalogStorage
		}
	}
	retryMilliseconds := binary.BigEndian.Uint64(encoded[96:104])
	if retryMilliseconds > uint64(MaxScanRetryCooldown/time.Millisecond) {
		return fileFailureMeta{}, ErrCorruptCatalogStorage
	}
	if retryable == 1 {
		meta.record.retryAfter = time.Duration(retryMilliseconds) * time.Millisecond
	} else if retryable != 0 || retryMilliseconds != 0 {
		return fileFailureMeta{}, ErrCorruptCatalogStorage
	}
	meta.objectSize = binary.BigEndian.Uint64(encoded[104:112])
	copy(meta.commitment[:], encoded[112:144])
	meta.spillBytes = binary.BigEndian.Uint64(encoded[144:152])
	if err := meta.record.valid(); err != nil || meta.objectSize == 0 ||
		meta.objectSize > MaxCatalogPageObjectBytes || meta.spillBytes != failureMetaBytes+meta.objectSize {
		return fileFailureMeta{}, ErrCorruptCatalogStorage
	}
	return meta, nil
}

func (b *FileCatalogBackend) failurePath(directory DirectoryID, attempt ScanAttemptID) string {
	return filepath.Join(b.failuresDir, hex.EncodeToString(directory[:]), hex.EncodeToString(attempt[:]))
}

func (b *FileCatalogBackend) CommitFailure(
	ctx context.Context,
	record DirectoryFailureRecord,
	object SealedFailureObject,
	meter ResourceMeter,
	prepare func(ResourceUsage) error,
) (FailurePreparation, error) {
	if err := ctx.Err(); err != nil {
		return FailurePreparation{}, err
	}
	if err := record.valid(); err != nil || object.IsZero() || meter == nil || prepare == nil || record.share != b.share {
		return FailurePreparation{}, errors.Join(errors.New("catalog failure commit is invalid"), err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return FailurePreparation{}, ErrCatalogClosed
	}
	existing, handled, err := b.prepareExistingFailureLocked(record, object)
	if err != nil {
		return FailurePreparation{}, err
	}
	if handled {
		return existing, nil
	}
	if err := b.requireFailurePredecessorLocked(record); err != nil {
		return FailurePreparation{}, err
	}
	transactionPath, meta, err := b.stageFailureLocked(record, object, meter)
	if err != nil {
		return FailurePreparation{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(transactionPath)
		}
	}()
	directoryPath, directoryCreated, err := b.ensureFailureDirectoryLocked(record.directory)
	if err != nil {
		return FailurePreparation{}, err
	}
	defer b.cleanUnpublishedFailureDirectory(directoryPath, directoryCreated, &committed)
	usage := ResourceUsage{SpillBytes: meta.spillBytes}
	if err := prepare(usage); err != nil {
		return FailurePreparation{}, err
	}
	if err := ctx.Err(); err != nil {
		return FailurePreparation{}, err
	}
	target := b.failurePath(record.directory, record.attempt)
	if err := b.publishFailureLocked(transactionPath, target, directoryPath); err != nil {
		return FailurePreparation{}, err
	}
	committed = true
	return FailurePreparation{Record: record, Usage: usage}, nil
}

func (b *FileCatalogBackend) prepareExistingFailureLocked(
	record DirectoryFailureRecord,
	object SealedFailureObject,
) (FailurePreparation, bool, error) {
	existing, found, err := b.loadFailureLocked(record.directory, record.attempt)
	if err != nil || !found {
		return FailurePreparation{}, false, err
	}
	if existing.record != record || existing.commitment != object.commitment {
		return FailurePreparation{}, false, ErrGenerationConflict
	}
	return FailurePreparation{Record: record, Existing: true}, true, nil
}

func (b *FileCatalogBackend) requireFailurePredecessorLocked(record DirectoryFailureRecord) error {
	records, err := b.failureRecordsLocked(record.directory)
	if err != nil {
		return err
	}
	if len(records) >= maxFailureAttemptsPerDirectory {
		return ErrBudgetExceeded
	}
	tail, err := validateFailureChain(records)
	if err != nil || tail != record.previousAttempt {
		return errors.Join(ErrGenerationConflict, err)
	}
	return nil
}

func (b *FileCatalogBackend) stageFailureLocked(
	record DirectoryFailureRecord,
	object SealedFailureObject,
	meter ResourceMeter,
) (path string, meta fileFailureMeta, resultErr error) {
	if b.faults != nil {
		if err := b.faults.Fail(FileFaultStageFailure); err != nil {
			return "", fileFailureMeta{}, err
		}
	}
	path, err := os.MkdirTemp(b.stagingDir, "failure-")
	if err != nil {
		return "", fileFailureMeta{}, err
	}
	cleanupPath := path
	defer func() {
		if resultErr != nil {
			_ = os.RemoveAll(cleanupPath)
		}
	}()
	meta = fileFailureMeta{
		record: record, commitment: object.commitment, objectSize: uint64(len(object.encoded)),
		spillBytes: failureMetaBytes + uint64(len(object.encoded)),
	}
	if err := stageFailureFile(filepath.Join(path, failureMetaName), encodeFileFailureMeta(meta), meter); err != nil {
		return "", fileFailureMeta{}, err
	}
	if err := stageFailureFile(filepath.Join(path, failureObjectName), object.Bytes(), meter); err != nil {
		return "", fileFailureMeta{}, err
	}
	if err := syncCatalogDirectory(path); err != nil {
		return "", fileFailureMeta{}, err
	}
	return path, meta, nil
}

func (b *FileCatalogBackend) ensureFailureDirectoryLocked(directory DirectoryID) (string, bool, error) {
	path := filepath.Join(b.failuresDir, hex.EncodeToString(directory[:]))
	if err := os.Mkdir(path, 0o700); err == nil {
		if err := syncCatalogDirectory(b.failuresDir); err != nil {
			_ = os.Remove(path)
			_ = syncCatalogDirectory(b.failuresDir)
			return "", false, err
		}
		return path, true, nil
	} else if !errors.Is(err, os.ErrExist) {
		return "", false, err
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", false, ErrCorruptCatalogStorage
	}
	return path, false, nil
}

func (b *FileCatalogBackend) cleanUnpublishedFailureDirectory(path string, created bool, committed *bool) {
	if !*committed && created {
		_ = os.Remove(path)
		_ = syncCatalogDirectory(b.failuresDir)
	}
}

func (b *FileCatalogBackend) publishFailureLocked(transactionPath, target, directoryPath string) error {
	if b.faults != nil {
		if err := b.faults.Fail(FileFaultPublishFailure); err != nil {
			return err
		}
	}
	if err := os.Rename(transactionPath, target); err != nil {
		return err
	}
	if err := syncCatalogDirectory(directoryPath); err != nil {
		if rollbackErr := os.Rename(target, transactionPath); rollbackErr == nil {
			_ = syncCatalogDirectory(directoryPath)
			return err
		}
	}
	return nil
}

func stageFailureFile(path string, encoded []byte, meter ResourceMeter) error {
	if err := meter.Consume(ResourceUsage{SpillBytes: uint64(len(encoded))}); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := writeFull(file, encoded); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func (b *FileCatalogBackend) LoadFailureObject(
	ctx context.Context,
	directory DirectoryID,
	attempt ScanAttemptID,
) (SealedFailureObject, bool, error) {
	if err := ctx.Err(); err != nil {
		return SealedFailureObject{}, false, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return SealedFailureObject{}, false, ErrCatalogClosed
	}
	meta, found, err := b.loadFailureLocked(directory, attempt)
	if err != nil || !found {
		return SealedFailureObject{}, found, err
	}
	encoded, err := readCatalogObject(filepath.Join(b.failurePath(directory, attempt), failureObjectName))
	if err != nil {
		return SealedFailureObject{}, false, err
	}
	object, err := NewSealedFailureObject(encoded)
	if err != nil || object.commitment != meta.commitment {
		return SealedFailureObject{}, false, ErrCorruptCatalogStorage
	}
	return object, true, nil
}

func (b *FileCatalogBackend) ReplayFailures(ctx context.Context, yield func(DirectoryFailureRecord, bool) error) error {
	if yield == nil {
		return errors.New("catalog failure replay requires a consumer")
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return ErrCatalogClosed
	}
	_, err := b.walkFailuresLocked(ctx, func(meta fileFailureMeta, active bool) error {
		return yield(meta.record, active)
	})
	return err
}

func (b *FileCatalogBackend) recoverFailures(ctx context.Context) (ResourceUsage, error) {
	if err := b.cleanEmptyFailureDirectoriesLocked(ctx); err != nil {
		return ResourceUsage{}, err
	}
	return b.walkFailuresLocked(ctx, nil)
}

func (b *FileCatalogBackend) failureRecordsLocked(
	directory DirectoryID,
) (records []DirectoryFailureRecord, resultErr error) {
	path := filepath.Join(b.failuresDir, hex.EncodeToString(directory[:]))
	handle, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, handle.Close())
	}()
	records = make([]DirectoryFailureRecord, 0, failureDirectoryReadBatch)
	for {
		entries, readErr := handle.ReadDir(failureDirectoryReadBatch)
		if len(records)+len(entries) > maxFailureAttemptsPerDirectory {
			return nil, ErrCorruptCatalogStorage
		}
		for _, entry := range entries {
			attempt, err := decodeFailurePathIdentity[ScanAttemptID](entry.Name(), entry.IsDir())
			if err != nil {
				return nil, err
			}
			meta, found, err := b.loadFailureLocked(directory, attempt)
			if err != nil || !found {
				return nil, ErrCorruptCatalogStorage
			}
			records = append(records, meta.record)
		}
		if errors.Is(readErr, io.EOF) {
			return records, nil
		}
		if readErr != nil {
			return nil, readErr
		}
	}
}

func (b *FileCatalogBackend) loadFailureLocked(
	directory DirectoryID,
	attempt ScanAttemptID,
) (fileFailureMeta, bool, error) {
	path := b.failurePath(directory, attempt)
	encoded, err := readCatalogObject(filepath.Join(path, failureMetaName))
	if errors.Is(err, os.ErrNotExist) {
		return fileFailureMeta{}, false, nil
	}
	if err != nil {
		return fileFailureMeta{}, false, err
	}
	meta, err := decodeFileFailureMeta(encoded)
	if err != nil || meta.record.share != b.share || meta.record.directory != directory || meta.record.attempt != attempt {
		return fileFailureMeta{}, false, ErrCorruptCatalogStorage
	}
	objectBytes, err := readCatalogObject(filepath.Join(path, failureObjectName))
	if err != nil || uint64(len(objectBytes)) != meta.objectSize || sha256.Sum256(objectBytes) != meta.commitment {
		return fileFailureMeta{}, false, ErrCorruptCatalogStorage
	}
	size, err := directoryTreeBytes(path)
	if err != nil || size != meta.spillBytes {
		return fileFailureMeta{}, false, ErrCorruptCatalogStorage
	}
	return meta, true, nil
}

func decodeFailurePathIdentity[T ~[IdentityBytes]byte](name string, isDirectory bool) (T, error) {
	var identity T
	decoded, err := hex.DecodeString(name)
	if err != nil || !isDirectory || len(decoded) != IdentityBytes {
		return identity, ErrCorruptCatalogStorage
	}
	copy(identity[:], decoded)
	if identity == (T{}) || name != hex.EncodeToString(identity[:]) {
		return T{}, ErrCorruptCatalogStorage
	}
	return identity, nil
}
