package checkpointstore

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/transfer"
)

func TestReconciledFileIndexPrefersOnlyExactOwnedReadyCandidate(t *testing.T) {
	_, registry, lease, repository, ownership, operation := openRepositoryFixture(t, 0xb1)
	defer registry.Close()
	defer lease.Close()
	defer repository.Close()
	store, err := NewFreshFileExecutionStore(&repository)
	if err != nil {
		t.Fatal(err)
	}
	current := checkpointRecordFixture(t, ownership, operation, 0xb2)
	currentFile, observation, err := store.CreateOwnedFile(
		context.Background(), nil, current.OwnedObjectID(), current.ExactSize(),
	)
	if err != nil || observation.Condition() != fileexecution.OwnedReady {
		t.Fatalf("current object = (%d, %v)", observation.Condition(), err)
	}
	if err := currentFile.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Store(context.Background(), nil, current); err != nil {
		t.Fatal(err)
	}

	missingObject, err := checkpointmodel.ObjectIDFromBytes(bytes.Repeat(
		[]byte{0xb3}, transfer.OwnedObjectIdentityBytes,
	))
	if err != nil {
		t.Fatal(err)
	}
	missing := sameCheckpointCoordinateWithObject(t, current, missingObject)
	if err := store.indexReconciledRecord(missing); err != nil {
		t.Fatalf("ready current did not retain missing sibling: %v", err)
	}
	if retained := store.retained[missing.RecordID()]; retained.RecordID() != missing.RecordID() {
		t.Fatal("missing duplicate was not retained as inert evidence")
	}

	readyObject, err := checkpointmodel.ObjectIDFromBytes(bytes.Repeat(
		[]byte{0xb6}, transfer.OwnedObjectIdentityBytes,
	))
	if err != nil {
		t.Fatal(err)
	}
	readyFile, observation, err := store.CreateOwnedFile(
		context.Background(), nil, readyObject, current.ExactSize(),
	)
	if err != nil || observation.Condition() != fileexecution.OwnedReady {
		t.Fatalf("replacement object = (%d, %v)", observation.Condition(), err)
	}
	if err := readyFile.Close(); err != nil {
		t.Fatal(err)
	}
	ready := sameCheckpointCoordinateWithObject(t, current, readyObject)
	key := checkpointRecordLookupKey(current)
	selector := &FileExecutionStore{
		repository: &repository,
		records:    map[[32]byte]checkpointmodel.Record{key: missing},
		retained:   make(map[checkpointmodel.RecordID]checkpointmodel.Record),
	}
	if err := selector.indexReconciledRecord(ready); err != nil {
		t.Fatalf("ready sibling did not replace missing current: %v", err)
	}
	if selected := selector.records[key]; selected.RecordID() != ready.RecordID() ||
		selector.retained[missing.RecordID()].RecordID() != missing.RecordID() {
		t.Fatal("reconciled selection lost exact ready/missing distinction")
	}

	conflict := &FileExecutionStore{
		repository: &repository,
		records:    map[[32]byte]checkpointmodel.Record{key: current},
		retained:   make(map[checkpointmodel.RecordID]checkpointmodel.Record),
	}
	if err := conflict.indexReconciledRecord(ready); !errors.Is(err, checkpointmodel.ErrRecordBinding) {
		t.Fatalf("two ready siblings did not require attention: %v", err)
	}
	if ready, err := conflict.recordOwnedReady(current); err != nil || !ready {
		t.Fatalf("ready object observation = (%t, %v)", ready, err)
	}
	if ready, err := conflict.recordOwnedReady(missing); err != nil || ready {
		t.Fatalf("missing object observation = (%t, %v)", ready, err)
	}
}

func sameCheckpointCoordinateWithObject(
	t *testing.T,
	record checkpointmodel.Record,
	object checkpointmodel.ObjectID,
) checkpointmodel.Record {
	t.Helper()
	next, err := checkpointmodel.NewRecord(checkpointmodel.RecordSpec{
		OperationID:                  record.OperationID(),
		ReceiveIntentDigest:          record.ReceiveIntentDigest(),
		MaterializationBindingDigest: record.MaterializationBindingDigest(),
		FileID:                       record.FileID(),
		FileRevision:                 record.FileRevision(),
		CanonicalPath:                record.CanonicalPath(),
		ExactSize:                    record.ExactSize(),
		MaterializerKind:             record.MaterializerKind(),
		AuthorityRef:                 record.AuthorityRef().Bytes(),
		OwnedObjectID:                object.Bytes(),
		StateGeneration:              record.StateGeneration(),
		CheckpointGeneration:         record.CheckpointGeneration(),
		VerifiedRanges:               record.VerifiedRanges(),
		Phase:                        record.Phase(),
		CommitState:                  record.CommitState(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return next
}
