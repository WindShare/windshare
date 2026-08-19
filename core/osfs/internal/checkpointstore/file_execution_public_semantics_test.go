package checkpointstore

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
)

const (
	publicCheckpointFixtureFill = 0xd4
	publicCheckpointObjectFill  = 0xd5
	publicCheckpointOtherFill   = 0xd6
)

func TestFileExecutionStorePublicInstallAndLookupPreserveDurableAuthority(t *testing.T) {
	_, registry, lease, repository, ownership, operation := openRepositoryFixture(t, publicCheckpointFixtureFill)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = registry.Close()
	})
	store, err := NewFreshFileExecutionStore(&repository)
	if err != nil {
		t.Fatal(err)
	}
	file, session, destination := replacementMaterializationFixture(t, operation.intent)
	candidate := replacementRecordFixture(
		t, operation.intent, ownership, file, objectIDFixture(t, publicCheckpointObjectFill),
	)
	key := captureCheckpointKey(t, operation.intent, ownership, session, file, destination, store)

	absent, err := store.Lookup(context.Background(), key)
	if err != nil || absent.Decision() != checkpointmodel.CheckpointLineageDecisionAbsent {
		t.Fatalf("initial Lookup() = (%s, %v), want absent", absent.Decision(), err)
	}
	if record, present := absent.Record(); present || record.Valid() {
		t.Fatalf("absent Lookup() exposed record authority: present=%t record=%+v", present, record)
	}

	installed, err := store.InstallInitial(context.Background(), key, candidate)
	selected, exact := installed.Resolution().Record()
	if err != nil || !installed.Installed() || !exact ||
		!bytes.Equal(selected.CanonicalBytes(), candidate.CanonicalBytes()) {
		t.Fatalf("InstallInitial() = (installed=%t exact=%t record=%+v err=%v), want durable candidate",
			installed.Installed(), exact, selected, err)
	}
	durable, err := repository.Reopen(candidate.RecordID())
	if err != nil || !bytes.Equal(durable.CanonicalBytes(), candidate.CanonicalBytes()) {
		t.Fatalf("durable initial checkpoint = (%+v, %v), want candidate", durable, err)
	}

	// A retry observes the one durable authority without claiming that this call
	// installed it, so callers can distinguish recovery from fresh publication.
	retried, err := store.InstallInitial(context.Background(), key, candidate)
	retriedRecord, exact := retried.Resolution().Record()
	if err != nil || retried.Installed() || !exact ||
		!bytes.Equal(retriedRecord.CanonicalBytes(), candidate.CanonicalBytes()) {
		t.Fatalf("retried InstallInitial() = (installed=%t exact=%t record=%+v err=%v), want existing authority",
			retried.Installed(), exact, retriedRecord, err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if observation, installErr := store.InstallInitial(canceled, key, candidate); !errors.Is(installErr, context.Canceled) || observation.Installed() {
		t.Fatalf("canceled InstallInitial() = (installed=%t, %v)", observation.Installed(), installErr)
	}
	if observation, installErr := store.InstallInitial(context.Background(), key, checkpointmodel.Record{}); installErr == nil || observation.Installed() {
		t.Fatalf("invalid InstallInitial() = (installed=%t, %v)", observation.Installed(), installErr)
	}
}

func TestFileExecutionStoreRejectsConflictedAndUnsettledReplacementAuthority(t *testing.T) {
	_, registry, lease, repository, ownership, operation := openRepositoryFixture(t, publicCheckpointOtherFill)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = registry.Close()
	})
	file, session, destination := replacementMaterializationFixture(t, operation.intent)
	candidate := replacementRecordFixture(
		t, operation.intent, ownership, file, objectIDFixture(t, publicCheckpointObjectFill),
	)
	if err := repository.Create(candidate); err != nil {
		t.Fatal(err)
	}
	previous, err := checkpointmodel.PromoteInitialCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Replace(candidate, previous); err != nil {
		t.Fatal(err)
	}
	store, err := NewFileExecutionStore(&repository)
	if err != nil {
		t.Fatal(err)
	}
	key := captureCheckpointKey(t, operation.intent, ownership, session, file, destination, store)

	wrongRevision := previous.FileRevision()
	wrongRevision[0] ^= 0xff
	wrongPrevious := checkpointRecordVariant(t, previous, recordVariant{revision: &wrongRevision})
	wrongNext, err := checkpointmodel.AdvanceGeneration(
		wrongPrevious,
		[]checkpointmodel.Range{{Offset: 0, End: 8}},
		checkpointmodel.PhaseActive,
		checkpointmodel.CommitCandidate,
	)
	if err != nil {
		t.Fatal(err)
	}
	observation, replaceErr := store.Replace(context.Background(), wrongPrevious, wrongNext)
	record, present := observation.Record()
	if ErrorCodeFor(replaceErr) != ErrorUnsafeInstall || present || record.Valid() {
		t.Fatalf("conflicted Replace() = (present=%t record=%+v, %v), want unsafe install",
			present, record, replaceErr)
	}

	next, err := checkpointmodel.AdvanceGeneration(
		previous,
		[]checkpointmodel.Range{{Offset: 0, End: 8}},
		checkpointmodel.PhaseActive,
		checkpointmodel.CommitCandidate,
	)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, replaceErr := store.Replace(canceled, previous, next); !errors.Is(replaceErr, context.Canceled) {
		t.Fatalf("canceled Replace() = %v", replaceErr)
	}

	// An ambiguous durability result freezes the shared operation authority. No
	// adapter may classify or replace through it until restart reconciliation.
	store.authority.markUnsettled()
	if _, lookupErr := store.Lookup(context.Background(), key); ErrorCodeFor(lookupErr) != ErrorStateIO {
		t.Fatalf("unsettled Lookup() = %v, want state I/O", lookupErr)
	}
	if _, replaceErr := store.Replace(context.Background(), previous, next); ErrorCodeFor(replaceErr) != ErrorStateIO {
		t.Fatalf("unsettled Replace() = %v, want state I/O", replaceErr)
	}
	durable, err := repository.Reopen(previous.RecordID())
	if err != nil || !bytes.Equal(durable.CanonicalBytes(), previous.CanonicalBytes()) {
		t.Fatalf("rejected replacements changed durable authority: record=%+v err=%v", durable, err)
	}
}

func TestFileExecutionAuthorityRetriesAndRemovalKeepIndexesAtomic(t *testing.T) {
	_, registry, lease, repository, ownership, operation := openRepositoryFixture(t, publicCheckpointFixtureFill)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = registry.Close()
	})
	candidate := checkpointRecordFixture(t, ownership, operation, publicCheckpointObjectFill)
	previous, err := checkpointmodel.PromoteInitialCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	next, err := checkpointmodel.AdvanceGeneration(
		previous,
		[]checkpointmodel.Range{{Offset: 0, End: 8}},
		checkpointmodel.PhaseActive,
		checkpointmodel.CommitCandidate,
	)
	if err != nil {
		t.Fatal(err)
	}
	authority := newFileExecutionAuthority()
	if err := authority.rebuild([]checkpointmodel.Record{previous}, nil); err != nil {
		t.Fatal(err)
	}
	if err := authority.replace(previous, next); err != nil {
		t.Fatal(err)
	}
	if err := authority.replace(previous, next); err != nil {
		t.Fatalf("idempotent replacement retry = %v", err)
	}
	if err := authority.remove(previous); !errors.Is(err, checkpointmodel.ErrRecordBinding) {
		t.Fatalf("stale-generation removal = %v", err)
	}
	if err := authority.remove(next); err != nil {
		t.Fatal(err)
	}
	if err := authority.remove(next); !errors.Is(err, checkpointmodel.ErrRecordBinding) {
		t.Fatalf("repeated removal = %v", err)
	}
	if len(authority.physical) != 0 || len(authority.lineages) != 0 || len(authority.objects) != 0 {
		t.Fatalf("exact removal retained indexes: physical=%d lineages=%d objects=%d",
			len(authority.physical), len(authority.lineages), len(authority.objects))
	}

	var nilAuthority *fileExecutionAuthority
	if err := nilAuthority.requireSettled(); !errors.Is(err, checkpointmodel.ErrInvalidRecord) {
		t.Fatalf("nil settlement authority = %v", err)
	}
	if err := nilAuthority.admitInitial(candidate); !errors.Is(err, checkpointmodel.ErrInvalidRecord) {
		t.Fatalf("nil initial authority = %v", err)
	}
	if err := authority.admitInitial(previous); !errors.Is(err, checkpointmodel.ErrInvalidRecord) {
		t.Fatalf("non-initial admission = %v", err)
	}
}
