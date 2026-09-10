package v2route

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"

	"github.com/windshare/windshare/core/link"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

var errInjectedTombstone = errors.New("injected STOP index failure")

func openTestTombstones(t *testing.T, path string) *FileTombstoneStore {
	t.Helper()
	store, err := NewFileTombstoneStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func TestFileTombstoneStorePersistsAndRejectsCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay", "stopped.bin")
	store := openTestTombstones(t, path)
	record := validFileTombstone(t)
	if outcome, err := store.Commit(context.Background(), record); outcome != CommitCommitted || err != nil {
		t.Fatalf("commit = %v, %v", outcome, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestTombstones(t, path)
	if got, found, err := store.Lookup(context.Background(), record.ShareID); err != nil || !found || got != record {
		t.Fatalf("restored lookup = %+v, %t, %v", got, found, err)
	}
	if _, found, err := store.Lookup(context.Background(), v2.ShareID{1}); err != nil || found {
		t.Fatalf("unknown lookup = %t, %v", found, err)
	}
	if err := store.db.Update(func(tx *bolt.Tx) error {
		value := encodeTombstoneRecord(record)
		value[len(value)-1] ^= 1
		return tx.Bucket(tombstoneBucket).Put(record.ShareID[:], value)
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Lookup(context.Background(), record.ShareID); !errors.Is(err, ErrTombstoneFile) {
		t.Fatalf("corrupt lookup = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := NewFileTombstoneStore(path); err == nil {
		_ = reopened.Close()
		t.Fatal("corrupt history accepted at startup")
	}
}

func TestFileTombstoneStoreHonorsContextAndInputValidation(t *testing.T) {
	store := openTestTombstones(t, filepath.Join(t.TempDir(), "stopped.bin"))
	record := validFileTombstone(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := store.Lookup(ctx, record.ShareID); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled lookup = %v", err)
	}
	if outcome, err := store.Commit(ctx, record); outcome != CommitNotCommitted || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled commit = %v, %v", outcome, err)
	}
	if _, err := store.Commit(context.Background(), Tombstone{}); !errors.Is(err, ErrTombstoneFile) {
		t.Fatalf("invalid record = %v", err)
	}
	var absent *FileTombstoneStore
	if _, _, err := absent.Lookup(ctx, record.ShareID); !errors.Is(err, ErrTombstoneFile) {
		t.Fatalf("nil lookup = %v", err)
	}
	if _, err := absent.Commit(ctx, record); !errors.Is(err, ErrTombstoneFile) {
		t.Fatalf("nil commit = %v", err)
	}
	if err := absent.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Lookup(context.Background(), record.ShareID); !errors.Is(err, errTombstoneClosed) {
		t.Fatalf("closed lookup = %v", err)
	}
	if _, err := store.Commit(context.Background(), record); !errors.Is(err, errTombstoneClosed) {
		t.Fatalf("closed commit = %v", err)
	}
	if _, err := NewFileTombstoneStore(""); !errors.Is(err, ErrTombstoneFile) {
		t.Fatalf("empty path = %v", err)
	}
}

func TestFileTombstoneCommitIsIdempotentAndRejectsShareConflict(t *testing.T) {
	store := openTestTombstones(t, filepath.Join(t.TempDir(), "stopped.bin"))
	record := validFileTombstone(t)
	for range 2 {
		if outcome, err := store.Commit(context.Background(), record); outcome != CommitCommitted || err != nil {
			t.Fatalf("commit = %v, %v", outcome, err)
		}
	}
	conflict := record
	conflict.StopID[0] ^= 0x80
	if outcome, err := store.Commit(context.Background(), conflict); outcome != CommitUnknown || !errors.Is(err, ErrTombstoneConflict) {
		t.Fatalf("conflict = %v, %v", outcome, err)
	}
	if got, found, err := store.Lookup(context.Background(), record.ShareID); got != record || !found || err != nil {
		t.Fatalf("conflict changed revocation: %+v, %t, %v", got, found, err)
	}
}

type faultTombstoneDatabase struct {
	*bolt.DB
	afterCommit bool
	syncFailure bool
}

func (db *faultTombstoneDatabase) Update(fn func(*bolt.Tx) error) error {
	err := db.DB.Update(func(tx *bolt.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		if !db.afterCommit {
			return errInjectedTombstone
		}
		return nil
	})
	if err != nil {
		return err
	}
	return errInjectedTombstone
}

func (db *faultTombstoneDatabase) Sync() error {
	if db.syncFailure {
		return errInjectedTombstone
	}
	return db.DB.Sync()
}

func TestFileTombstoneAmbiguousCommitFencesAbsenceUntilRecovery(t *testing.T) {
	for _, persisted := range []bool{false, true} {
		t.Run(map[bool]string{false: "before commit", true: "after commit"}[persisted], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "stopped.bin")
			store := openTestTombstones(t, path)
			record := validFileTombstone(t)
			store.db = &faultTombstoneDatabase{DB: store.db.(*bolt.DB), afterCommit: persisted}
			if outcome, err := store.Commit(context.Background(), record); outcome != CommitUnknown || !errors.Is(err, ErrCommitUncertain) {
				t.Fatalf("ambiguous commit = %v, %v", outcome, err)
			}
			if _, _, err := store.Lookup(context.Background(), record.ShareID); !errors.Is(err, ErrCommitUncertain) {
				t.Fatalf("uncertain index reported an authoritative lookup: %v", err)
			}
			store.open = func(string) (tombstoneDatabase, error) { return nil, errInjectedTombstone }
			if outcome, err := store.Commit(context.Background(), record); outcome != CommitUnknown || !errors.Is(err, ErrCommitUncertain) {
				t.Fatalf("failed recovery = %v, %v", outcome, err)
			}
			store.open = openTombstoneDatabase
			if outcome, err := store.Commit(context.Background(), record); outcome != CommitCommitted || err != nil {
				t.Fatalf("retry = %v, %v", outcome, err)
			}
			if got, found, err := store.Lookup(context.Background(), record.ShareID); got != record || !found || err != nil {
				t.Fatalf("recovered lookup = %+v, %t, %v", got, found, err)
			}
		})
	}
}

func TestFileTombstoneExactRetryRequiresCleanResync(t *testing.T) {
	store := openTestTombstones(t, filepath.Join(t.TempDir(), "stopped.bin"))
	record := validFileTombstone(t)
	if _, err := store.Commit(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	store.db = &faultTombstoneDatabase{DB: store.db.(*bolt.DB), syncFailure: true}
	if outcome, err := store.Commit(context.Background(), record); outcome != CommitUnknown || !errors.Is(err, ErrCommitUncertain) {
		t.Fatalf("failed exact retry = %v, %v", outcome, err)
	}
	if outcome, err := store.Commit(context.Background(), record); outcome != CommitCommitted || err != nil {
		t.Fatalf("recovered exact retry = %v, %v", outcome, err)
	}
}

func TestTombstoneRecordValidation(t *testing.T) {
	record := validFileTombstone(t)
	encoded := encodeTombstoneRecord(record)
	invalid := record
	invalid.ShareInstance = v2.ShareInstance{}
	for _, test := range []struct {
		name  string
		key   []byte
		value []byte
	}{
		{name: "short", key: record.ShareID[:], value: encoded[:len(encoded)-1]},
		{name: "wrong key", key: []byte("wrong"), value: encoded},
		{name: "invalid authority", key: invalid.ShareID[:], value: encodeTombstoneRecord(invalid)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeTombstoneRecord(test.key, test.value); !errors.Is(err, ErrTombstoneFile) {
				t.Fatalf("invalid record accepted: %v", err)
			}
		})
	}
}

func TestFileTombstoneStoreRejectsInvalidExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stopped.bin")
	if err := os.WriteFile(path, []byte("WSR2STOP legacy or corrupt index"), 0o600); err != nil {
		t.Fatal(err)
	}
	if store, err := NewFileTombstoneStore(path); err == nil {
		_ = store.Close()
		t.Fatal("invalid index silently replaced")
	}
}

func validFileTombstone(t *testing.T) Tombstone {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pkHashRaw, err := link.SenderKeyHash(public)
	if err != nil {
		t.Fatal(err)
	}
	shareIDText, err := link.ShareIDForSenderKeyHash(pkHashRaw[:])
	if err != nil {
		t.Fatal(err)
	}
	shareIDRaw, err := base64.RawURLEncoding.Strict().DecodeString(shareIDText)
	if err != nil {
		t.Fatal(err)
	}
	shareID, _ := v2.ShareIDFromBytes(shareIDRaw)
	pkHash, _ := v2.PKHashFromBytes(pkHashRaw[:])
	return Tombstone{
		ShareID: shareID, ShareInstance: v2.ShareInstance{1}, PKHash: pkHash, StopID: v2.StopID{1},
	}
}
