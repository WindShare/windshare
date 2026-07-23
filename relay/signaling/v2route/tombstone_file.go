package v2route

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

const (
	tombstoneFileVersion = byte(1)
	tombstonePayloadSize = v2.ShareIDBytes + v2.ShareInstanceBytes + v2.PKHashBytes + v2.StopIDBytes
	tombstoneRecordSize  = tombstonePayloadSize + sha256.Size
)

var (
	tombstoneFileMagic     = [8]byte{'W', 'S', 'R', '2', 'S', 'T', 'O', 'P'}
	ErrTombstoneFile       = errors.New("relay v2 route: tombstone file is invalid")
	ErrTombstoneConflict   = errors.New("relay v2 route: conflicting STOP tombstone")
	openProductionStopFile = func(path string, flag int, perm os.FileMode) (stopFile, error) {
		return os.OpenFile(path, flag, perm)
	}
)

type stopFile interface {
	io.Reader
	io.Writer
	io.Seeker
	Sync() error
	Truncate(int64) error
	Close() error
}

type stopFileOpener func(string, int, os.FileMode) (stopFile, error)

// FileTombstoneStore is the production durability boundary for explicit STOP.
// The append-only format keeps an acknowledged tombstone recoverable even when
// a later record is interrupted; corruption is rejected at startup instead of
// silently resurrecting a stopped share.
type FileTombstoneStore struct {
	path string
	mu   sync.Mutex
	open stopFileOpener
}

func NewFileTombstoneStore(path string) (*FileTombstoneStore, error) {
	if path == "" {
		return nil, ErrTombstoneFile
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve relay tombstone file: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		return nil, fmt.Errorf("create relay state directory: %w", err)
	}
	file, err := os.OpenFile(absolute, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open relay tombstone file: %w", err)
	}
	info, statErr := file.Stat()
	if statErr == nil && info.Size() == 0 {
		header := append(tombstoneFileMagic[:], tombstoneFileVersion)
		_, err = file.Write(header)
		if err == nil {
			err = file.Sync()
		}
	}
	closeErr := file.Close()
	if err = errors.Join(statErr, err, closeErr); err != nil {
		return nil, fmt.Errorf("initialize relay tombstone file: %w", err)
	}
	if err := syncDirectory(filepath.Dir(absolute)); err != nil {
		return nil, fmt.Errorf("persist relay state directory: %w", err)
	}
	store := &FileTombstoneStore{path: absolute, open: openProductionStopFile}
	if err := store.recoverPartialTail(); err != nil {
		return nil, err
	}
	return store, nil
}

func (store *FileTombstoneStore) Load(ctx context.Context) ([]Tombstone, error) {
	if store == nil || store.path == "" {
		return nil, ErrTombstoneFile
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	encoded, err := os.ReadFile(store.path)
	if err != nil {
		return nil, fmt.Errorf("read relay tombstone file: %w", err)
	}
	return decodeTombstoneRecords(ctx, encoded)
}

func (store *FileTombstoneStore) Commit(
	ctx context.Context,
	tombstone Tombstone,
) (CommitOutcome, error) {
	if store == nil || store.path == "" || !validTombstone(tombstone) {
		return CommitNotCommitted, ErrTombstoneFile
	}
	if err := ctx.Err(); err != nil {
		return CommitNotCommitted, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	file, err := store.openFile(os.O_RDWR)
	if err != nil {
		return CommitUnknown, fmt.Errorf("open relay tombstone file for commit: %w", err)
	}
	encoded, readErr := io.ReadAll(file)
	if readErr != nil {
		return CommitUnknown, errors.Join(fmt.Errorf("read relay tombstones before commit: %w", readErr), file.Close())
	}
	records, decodeErr := decodeTombstoneRecords(ctx, encoded)
	if decodeErr != nil {
		return CommitUnknown, errors.Join(decodeErr, file.Close())
	}
	for _, existing := range records {
		if existing.ShareID != tombstone.ShareID {
			continue
		}
		if existing != tombstone {
			return CommitUnknown, errors.Join(ErrTombstoneConflict, file.Close())
		}
		// A retry resolves an earlier ambiguous result only after the exact record
		// is validated and synchronized again on a clean handle lifecycle.
		return committedAfterSync(file)
	}
	originalSize := int64(len(encoded))
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return CommitUnknown, errors.Join(fmt.Errorf("seek relay tombstone append: %w", err), file.Close())
	}
	payload := encodeTombstone(tombstone)
	digest := sha256.Sum256(payload)
	record := make([]byte, tombstoneRecordSize)
	copy(record, payload)
	copy(record[tombstonePayloadSize:], digest[:])
	_, writeErr := io.Copy(file, bytes.NewReader(record))
	syncErr := error(nil)
	if writeErr == nil {
		syncErr = file.Sync()
	}
	closeErr := file.Close()
	commitErr := errors.Join(writeErr, syncErr, closeErr)
	if commitErr == nil {
		return CommitCommitted, nil
	}
	rollbackErr := store.rollback(originalSize)
	if rollbackErr != nil {
		return CommitUnknown, errors.Join(ErrCommitUncertain, fmt.Errorf("persist relay tombstone: %w", commitErr), rollbackErr)
	}
	return CommitNotCommitted, fmt.Errorf("persist relay tombstone: %w", commitErr)
}

func (store *FileTombstoneStore) openFile(flag int) (stopFile, error) {
	open := store.open
	if open == nil {
		open = openProductionStopFile
	}
	return open(store.path, flag, 0o600)
}

func committedAfterSync(file stopFile) (CommitOutcome, error) {
	err := errors.Join(file.Sync(), file.Close())
	if err != nil {
		return CommitUnknown, errors.Join(ErrCommitUncertain, fmt.Errorf("confirm relay tombstone durability: %w", err))
	}
	return CommitCommitted, nil
}

func (store *FileTombstoneStore) rollback(size int64) error {
	file, err := store.openFile(os.O_RDWR)
	if err != nil {
		return fmt.Errorf("open relay tombstone rollback: %w", err)
	}
	truncateErr := file.Truncate(size)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(truncateErr, syncErr, closeErr); err != nil {
		return fmt.Errorf("rollback relay tombstone append: %w", err)
	}
	return nil
}

func (store *FileTombstoneStore) recoverPartialTail() error {
	file, err := store.openFile(os.O_RDWR)
	if err != nil {
		return fmt.Errorf("open relay tombstone recovery: %w", err)
	}
	encoded, readErr := io.ReadAll(file)
	if readErr != nil {
		return errors.Join(fmt.Errorf("read relay tombstone recovery: %w", readErr), file.Close())
	}
	_, completeBytes, decodeErr := decodeTombstonePrefix(context.Background(), encoded)
	if decodeErr != nil {
		return errors.Join(decodeErr, file.Close())
	}
	if completeBytes == len(encoded) {
		if err := file.Close(); err != nil {
			return fmt.Errorf("close relay tombstone recovery: %w", err)
		}
		return nil
	}
	// Only an incomplete final fixed record is unambiguously unacknowledged:
	// every prior acknowledged record crossed file.Sync, so a torn complete
	// record or any earlier checksum failure must remain startup-fatal.
	truncateErr := file.Truncate(int64(completeBytes))
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(truncateErr, syncErr, closeErr); err != nil {
		return fmt.Errorf("truncate partial relay tombstone tail: %w", err)
	}
	return nil
}

func decodeTombstoneRecords(ctx context.Context, encoded []byte) ([]Tombstone, error) {
	records, completeBytes, err := decodeTombstonePrefix(ctx, encoded)
	if err != nil {
		return nil, err
	}
	if completeBytes != len(encoded) {
		return nil, ErrTombstoneFile
	}
	return records, nil
}

func decodeTombstonePrefix(ctx context.Context, encoded []byte) ([]Tombstone, int, error) {
	headerSize := len(tombstoneFileMagic) + 1
	if len(encoded) < headerSize || !bytes.Equal(encoded[:len(tombstoneFileMagic)], tombstoneFileMagic[:]) ||
		encoded[len(tombstoneFileMagic)] != tombstoneFileVersion {
		return nil, 0, ErrTombstoneFile
	}
	completeBytes := headerSize + ((len(encoded)-headerSize)/tombstoneRecordSize)*tombstoneRecordSize
	result := make([]Tombstone, 0, (completeBytes-headerSize)/tombstoneRecordSize)
	for offset := headerSize; offset < completeBytes; offset += tombstoneRecordSize {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		record := encoded[offset : offset+tombstoneRecordSize]
		payload := record[:tombstonePayloadSize]
		digest := sha256.Sum256(payload)
		if !bytes.Equal(digest[:], record[tombstonePayloadSize:]) {
			return nil, 0, ErrTombstoneFile
		}
		result = append(result, decodeTombstone(payload))
	}
	return result, completeBytes, nil
}

func encodeTombstone(value Tombstone) []byte {
	result := make([]byte, 0, tombstonePayloadSize)
	result = append(result, value.ShareID[:]...)
	result = append(result, value.ShareInstance[:]...)
	result = append(result, value.PKHash[:]...)
	return append(result, value.StopID[:]...)
}

func decodeTombstone(encoded []byte) Tombstone {
	var result Tombstone
	offset := 0
	offset += copy(result.ShareID[:], encoded[offset:offset+v2.ShareIDBytes])
	offset += copy(result.ShareInstance[:], encoded[offset:offset+v2.ShareInstanceBytes])
	offset += copy(result.PKHash[:], encoded[offset:offset+v2.PKHashBytes])
	copy(result.StopID[:], encoded[offset:offset+v2.StopIDBytes])
	return result
}
