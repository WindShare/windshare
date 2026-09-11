package v2route

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

const (
	tombstonePayloadSize = v2.ShareIDBytes + v2.ShareInstanceBytes + v2.PKHashBytes + v2.StopIDBytes
	tombstoneRecordSize  = tombstonePayloadSize + sha256.Size
	tombstoneOpenTimeout = time.Second
)

var (
	tombstoneBucket      = []byte("permanent-stops-v1")
	ErrTombstoneFile     = errors.New("relay v2 route: tombstone index is invalid")
	ErrTombstoneConflict = errors.New("relay v2 route: conflicting STOP tombstone")
	errTombstonePresent  = errors.New("relay v2 route: exact STOP already persisted")
	errTombstoneClosed   = errors.New("relay v2 route: tombstone index is closed")
)

type tombstoneDatabase interface {
	View(func(*bolt.Tx) error) error
	Update(func(*bolt.Tx) error) error
	Sync() error
	Close() error
}

type tombstoneDatabaseOpener func(string) (tombstoneDatabase, error)

// FileTombstoneStore uses a disk B+tree so history neither occupies route slots
// nor requires a heap-resident map or a full-history scan for each STOP.
// The single database owner must Close after all endpoint handlers have joined.
type FileTombstoneStore struct {
	mu        sync.RWMutex
	path      string
	db        tombstoneDatabase
	open      tombstoneDatabaseOpener
	uncertain error
	closed    bool
}

func NewFileTombstoneStore(path string) (*FileTombstoneStore, error) {
	if path == "" {
		return nil, ErrTombstoneFile
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve relay STOP index: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		return nil, fmt.Errorf("create relay state directory: %w", err)
	}
	db, err := openTombstoneDatabase(absolute)
	if err != nil {
		// Name the file: a rejection without a path is not actionable, and the
		// revocation history it guards must not be deleted blindly.
		return nil, fmt.Errorf("open STOP index %s: %w", absolute, err)
	}
	if err := syncDirectory(filepath.Dir(absolute)); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return &FileTombstoneStore{path: absolute, db: db, open: openTombstoneDatabase}, nil
}

func openTombstoneDatabase(path string) (tombstoneDatabase, error) {
	info, statErr := os.Stat(path)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	}
	fresh := errors.Is(statErr, os.ErrNotExist) || info.Size() == 0
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: tombstoneOpenTimeout})
	if err != nil {
		return nil, errors.Join(ErrTombstoneFile, err)
	}
	if fresh {
		err = db.Update(func(tx *bolt.Tx) error {
			_, createErr := tx.CreateBucket(tombstoneBucket)
			return createErr
		})
	}
	if err == nil {
		// Validate history without copying it into the registry or a growing map.
		// A damaged record must never silently become an absent revocation.
		err = db.View(func(tx *bolt.Tx) error {
			bucket := tx.Bucket(tombstoneBucket)
			if bucket == nil {
				return ErrTombstoneFile
			}
			return bucket.ForEach(func(key, value []byte) error {
				_, decodeErr := decodeTombstoneRecord(key, value)
				return decodeErr
			})
		})
	}
	if err != nil {
		return nil, errors.Join(ErrTombstoneFile, err, db.Close())
	}
	return db, nil
}

func (store *FileTombstoneStore) Lookup(ctx context.Context, shareID v2.ShareID) (Tombstone, bool, error) {
	if store == nil {
		return Tombstone{}, false, ErrTombstoneFile
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if err := store.readiness(ctx); err != nil {
		return Tombstone{}, false, err
	}
	var record Tombstone
	var found bool
	err := store.db.View(func(tx *bolt.Tx) error {
		value := tx.Bucket(tombstoneBucket).Get(shareID[:])
		if value == nil {
			return nil
		}
		var err error
		record, err = decodeTombstoneRecord(shareID[:], value)
		found = err == nil
		return err
	})
	return record, found, err
}

func (store *FileTombstoneStore) Commit(ctx context.Context, record Tombstone) (CommitOutcome, error) {
	if store == nil || !validTombstone(record) {
		return CommitNotCommitted, ErrTombstoneFile
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return CommitNotCommitted, err
	}
	if store.closed {
		return CommitNotCommitted, errTombstoneClosed
	}
	if store.uncertain != nil {
		if err := store.recover(); err != nil {
			return CommitUnknown, errors.Join(ErrCommitUncertain, err)
		}
	}
	commitAttempted := false
	err := store.db.Update(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		bucket := tx.Bucket(tombstoneBucket)
		if value := bucket.Get(record.ShareID[:]); value != nil {
			existing, err := decodeTombstoneRecord(record.ShareID[:], value)
			if err != nil {
				return err
			}
			if existing != record {
				return ErrTombstoneConflict
			}
			return errTombstonePresent
		}
		if err := bucket.Put(record.ShareID[:], encodeTombstoneRecord(record)); err != nil {
			return err
		}
		commitAttempted = true
		return nil
	})
	if errors.Is(err, errTombstonePresent) {
		// A same-ID retry acknowledges only after a clean durability boundary.
		err = store.db.Sync()
		commitAttempted = true
	}
	if err == nil {
		return CommitCommitted, nil
	}
	if commitAttempted {
		// A failed commit can have reached disk even if the current read view
		// has not advanced. No lookup may report absence until recovery.
		store.uncertain = err
		return CommitUnknown, errors.Join(ErrCommitUncertain, err)
	}
	if errors.Is(err, ErrTombstoneConflict) || errors.Is(err, ErrTombstoneFile) {
		return CommitUnknown, err
	}
	return CommitNotCommitted, err
}

func (store *FileTombstoneStore) readiness(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if store.closed || store.db == nil {
		return errTombstoneClosed
	}
	if store.uncertain != nil {
		return errors.Join(ErrCommitUncertain, store.uncertain)
	}
	return nil
}

func (store *FileTombstoneStore) recover() error {
	if store.db != nil {
		err := store.db.Close()
		store.db = nil
		if err != nil {
			return err
		}
	}
	db, err := store.open(store.path)
	if err != nil {
		return err
	}
	store.db = db
	if err := db.Sync(); err != nil {
		return err
	}
	store.uncertain = nil
	return nil
}

func (store *FileTombstoneStore) Close() error {
	if store == nil {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return nil
	}
	store.closed = true
	if store.db == nil {
		return nil
	}
	return store.db.Close()
}

func encodeTombstoneRecord(value Tombstone) []byte {
	result := make([]byte, 0, tombstoneRecordSize)
	result = append(result, value.ShareID[:]...)
	result = append(result, value.ShareInstance[:]...)
	result = append(result, value.PKHash[:]...)
	result = append(result, value.StopID[:]...)
	digest := sha256.Sum256(result)
	return append(result, digest[:]...)
}

func decodeTombstoneRecord(key, encoded []byte) (Tombstone, error) {
	if len(encoded) != tombstoneRecordSize {
		return Tombstone{}, ErrTombstoneFile
	}
	digest := sha256.Sum256(encoded[:tombstonePayloadSize])
	if !bytes.Equal(digest[:], encoded[tombstonePayloadSize:]) {
		return Tombstone{}, ErrTombstoneFile
	}
	var result Tombstone
	offset := 0
	offset += copy(result.ShareID[:], encoded[offset:offset+v2.ShareIDBytes])
	offset += copy(result.ShareInstance[:], encoded[offset:offset+v2.ShareInstanceBytes])
	offset += copy(result.PKHash[:], encoded[offset:offset+v2.PKHashBytes])
	copy(result.StopID[:], encoded[offset:offset+v2.StopIDBytes])
	if !bytes.Equal(key, result.ShareID[:]) || !validTombstone(result) {
		return Tombstone{}, ErrTombstoneFile
	}
	return result, nil
}
