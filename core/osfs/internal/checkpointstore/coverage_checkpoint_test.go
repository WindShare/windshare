package checkpointstore

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io/fs"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer"
)

func TestCertifiedRestartOpensExistingRepositoryWithoutCreatingState(t *testing.T) {
	root := newMemoryDirectory()
	config, intent := certifiedFixture(t, root, checkpointmodel.AuthorityCreatedRoot, 0xa1)
	namespace, err := Initialize(config)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := namespace.AcquireIntent(intent)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := lease.OpenOrCreateRepository()
	if err != nil {
		t.Fatal(err)
	}
	candidate := checkpointRecordFixture(t, config.Ownership, intent, 0xa3)
	record, err := checkpointmodel.PromoteInitialCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Create(record); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(repository.Close(), lease.Close(), namespace.Close()); err != nil {
		t.Fatal(err)
	}

	namespace, err = OpenNamespace(config)
	if err != nil {
		t.Fatal(err)
	}
	lease, err = namespace.AcquireIntent(intent)
	if err != nil {
		t.Fatal(err)
	}
	repository, err = lease.OpenExistingRepository()
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := repository.Reopen(record.RecordID())
	if err != nil || !bytes.Equal(reopened.CanonicalBytes(), record.CanonicalBytes()) {
		t.Fatalf("reopened certified record = %v, %v", reopened.RecordID(), err)
	}

	contender, err := OpenNamespace(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contender.AcquireIntent(intent); errorCode(err) != ErrorBusy {
		t.Fatalf("restart lease contention error = %v", err)
	}
	if err := contender.Close(); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(repository.Close(), lease.Close()); err != nil {
		t.Fatal(err)
	}

	absentIntent, err := transfer.TransferIntentDigestFromBytes(bytes.Repeat([]byte{0xb1}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	absentLease, err := namespace.AcquireIntent(absentIntent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := absentLease.OpenExistingRepository(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("absent restart repository error = %v", err)
	}
	checkpointRoot := root.dirs[ControlDirectory].dirs[CheckpointDirectory]
	intents := checkpointRoot.dirs[IntentsDirectory]
	intents.mu.Lock()
	_, created := intents.dirs[intentNamespaceName(absentIntent)]
	intents.mu.Unlock()
	if created {
		t.Fatal("read-only restart opener created an absent intent subtree")
	}
	if err := errors.Join(absentLease.Close(), namespace.Close()); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryReconcileSeparatesStaleCandidatesFromAmbiguousCrashState(t *testing.T) {
	_, namespace, lease, repository, ownership, intent := openRepositoryFixture(t, 0xb2)
	defer namespace.Close()
	defer lease.Close()
	defer repository.Close()

	initial := checkpointRecordFixture(t, ownership, intent, 0xb3)
	stable, err := checkpointmodel.PromoteInitialCandidate(initial)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Create(stable); err != nil {
		t.Fatal(err)
	}
	staleShard, staleName := installRecoveryCandidate(t, &repository, initial, 0)

	orphanInitial := checkpointRecordFixture(t, ownership, intent, 0xb4)
	orphan, err := checkpointmodel.PromoteInitialCandidate(orphanInitial)
	if err != nil {
		t.Fatal(err)
	}
	orphanShard, orphanName := installRecoveryCandidate(t, &repository, orphan, 0)

	conflict := checkpointRecordFixture(t, ownership, intent, 0xb5)
	conflictShard, conflictName := installRecoveryCandidate(t, &repository, conflict, 0)
	conflictShardName, conflictRecordName := recordLocation(conflict.RecordID())
	conflictRecordShard, err := OpenShard(repository.records, conflictShardName, true)
	if err != nil {
		t.Fatal(err)
	}
	writeMemoryFile(t, conflictRecordShard, conflictRecordName, []byte("corrupt canonical image"))
	if err := conflictRecordShard.Close(); err != nil {
		t.Fatal(err)
	}

	witnessCalls := 0
	snapshot, err := repository.Reconcile(func(checkpointmodel.Record) (bool, error) {
		witnessCalls++
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if witnessCalls != 0 {
		t.Fatalf("ambiguous temporary state requested %d initial witnesses", witnessCalls)
	}
	if len(snapshot.Records()) != 1 || snapshot.Records()[0].RecordID() != stable.RecordID() {
		t.Fatalf("reconciled records = %+v", snapshot.Records())
	}
	codes := make(map[AttentionCode]bool)
	for _, attention := range snapshot.Attention() {
		codes[attention.Code()] = true
	}
	for _, code := range []AttentionCode{
		AttentionCorruptRecord,
		AttentionOrphanedCandidate,
		AttentionConflictingCandidate,
	} {
		if !codes[code] {
			t.Fatalf("missing %q in crash reconciliation attention: %+v", code, snapshot.Attention())
		}
	}
	if _, err := ReadFile(staleShard, staleName); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("superseded candidate survived reconciliation: %v", err)
	}
	if _, err := ReadFile(orphanShard, orphanName); err != nil {
		t.Fatalf("orphaned committed candidate was mutated: %v", err)
	}
	if _, err := ReadFile(conflictShard, conflictName); err != nil {
		t.Fatalf("candidate beside corrupt canonical state was mutated: %v", err)
	}
}

func TestRepositoryRemoveRequiresTheExactCommittedImage(t *testing.T) {
	_, namespace, lease, repository, ownership, intent := openRepositoryFixture(t, 0xe1)
	defer namespace.Close()
	defer lease.Close()
	defer repository.Close()

	initial := checkpointRecordFixture(t, ownership, intent, 0xe2)
	committed, err := checkpointmodel.PromoteInitialCandidate(initial)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Create(committed); err != nil {
		t.Fatal(err)
	}
	if err := repository.Remove(committed); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Reopen(committed.RecordID()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("removed checkpoint reopen error = %v", err)
	}

	if err := repository.Create(committed); err != nil {
		t.Fatal(err)
	}
	replacement, err := checkpointmodel.AdvanceGeneration(
		committed,
		[]checkpointmodel.Range{{Offset: 0, End: 16}},
		checkpointmodel.PhaseActive,
		checkpointmodel.CommitVerified,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Replace(committed, replacement); err != nil {
		t.Fatal(err)
	}
	if err := repository.Remove(committed); errorCode(err) != ErrorUnsafeInstall {
		t.Fatalf("stale checkpoint removal error = %v", err)
	}
	retained, err := repository.Reopen(replacement.RecordID())
	if err != nil || retained.Checksum() != replacement.Checksum() {
		t.Fatalf("replacement after stale removal = %v, %v", retained.Checksum(), err)
	}
	if err := repository.Remove(replacement); err != nil {
		t.Fatal(err)
	}
}

func installRecoveryCandidate(
	t *testing.T,
	repository *Repository,
	record checkpointmodel.Record,
	attempt int,
) (*memoryDirectory, string) {
	t.Helper()
	encoded, err := checkpointmodel.EncodeRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	shardName, recordName := recordLocation(record.RecordID())
	shard, err := OpenShard(repository.records, shardName, true)
	if err != nil {
		t.Fatal(err)
	}
	candidateName := TemporaryName(recordName, encoded, attempt)
	writeMemoryFile(t, shard, candidateName, encoded)
	if err := shard.Close(); err != nil {
		t.Fatal(err)
	}
	return repository.records.(*memoryDirectory).dirsForTest(t, shardName), candidateName
}
