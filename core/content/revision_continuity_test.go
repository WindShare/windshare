package content

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
)

func TestOpenHandleRevisionRetainsResidentIdentityAndReopensWithFreshID(t *testing.T) {
	modified, _ := catalog.NewModifiedTime(100, 0, catalog.TimePrecisionSeconds)
	file, record := fileRecordWithModifiedTime(t, 1, 1, modified)
	source := &testRevisionSource{files: []*testStableFile{
		{data: []byte{1}, modified: modified}, {data: []byte{1}, modified: modified},
	}}
	store := newRevisionStore(t, source, &testClock{now: time.Unix(100, 0)}, nil, file, record)
	session := generousSession(t, store, "continuity")

	first, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	resident, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	if resident.Descriptor().FileRevision() != first.Descriptor().FileRevision() || source.Calls() != 1 {
		t.Fatal("a resident stable handle lost its shared revision")
	}
	if err := store.EndLease(first.ID(), LeaseRelinquished); err != nil {
		t.Fatal(err)
	}
	if err := store.EndLease(resident.ID(), LeaseRelinquished); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Descriptor().FileRevision() == first.Descriptor().FileRevision() {
		t.Fatal("reopen reused a released handle's FileRevision")
	}
	comparison := CompareRevisions(first.Descriptor(), reopened.Descriptor())
	if first.Descriptor().SourceMetadata().IsZero() || !comparison.AllowsReuse() ||
		comparison.Comparison != RevisionComparisonMetadataAgreement {
		t.Fatalf("reopen lost best-effort source applicability: %+v", comparison)
	}
	if source.Calls() != 2 || source.files[0].closed.Load() != 1 {
		t.Fatal("reopen did not release and reacquire exactly one stable handle")
	}
	old, err := NewBlockRef(file, first.Descriptor().FileRevision(), 0, first.Descriptor().Geometry())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadBlock(context.Background(), reopened.ID(), old); err == nil {
		t.Fatal("reopened handle accepted a block ref from the previous revision")
	}
	if err := store.EndLease(reopened.ID(), LeaseRelinquished); err != nil {
		t.Fatal(err)
	}
	if got := store.CapacitySnapshot().Process().Used(); got != (CapacityUsage{}) {
		t.Fatalf("reopening leaked capacity: %+v", got)
	}
}

func TestDetachedLeasePreservesResidentRevisionDuringResumeGrace(t *testing.T) {
	file, record := fileRecord(t, 1)
	source := &testRevisionSource{files: []*testStableFile{{data: []byte{1}}}}
	store := newRevisionStore(t, source, &testClock{now: time.Unix(100, 0)}, nil, file, record)
	traces := make(chan RevisionTrace, 8)
	store.tracer = RevisionTracerFunc(func(event RevisionTrace) { traces <- event })
	session := generousSession(t, store, "reconnect")

	first, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	if event := <-traces; event.Stage() != RevisionTraceStageOpenHandleBound {
		t.Fatalf("open scope trace=%v", event.Stage())
	}
	if err := store.EndLease(first.ID(), LeaseDetached); err != nil {
		t.Fatal(err)
	}
	second, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	if first.Descriptor().FileRevision() != second.Descriptor().FileRevision() || source.Calls() != 1 {
		t.Fatal("network reconnect discarded a still-resident revision during grace")
	}
}

func TestRepeatedInvalidationsAndStaleOpensNeverStopFreshAdmission(t *testing.T) {
	const churnCount = 4_200
	parent := catalogID[catalog.DirectoryID](8)
	locator, _ := catalog.NewSourceReference([]byte("object:churn"))
	identity, _ := catalog.NewSourceIdentity([]byte("identity"))
	candidate, _ := catalog.NewVersionCandidate([]byte("candidate"))
	records := make(map[catalog.NodeID]catalog.NodeRecord, churnCount+1)
	staleFiles := make([]catalog.FileID, churnCount)
	for index := range churnCount {
		var file catalog.FileID
		binary.BigEndian.PutUint64(file[8:], uint64(index+1))
		record, err := catalog.NewFileNodeRecord(file, parent, "churn.bin", locator, identity, candidate, 1, catalog.ModifiedTime{})
		if err != nil {
			t.Fatal(err)
		}
		records[file.NodeID()] = record
		staleFiles[index] = file
	}
	healthyFile, healthyRecord := fileRecordWithModifiedTime(t, 0xfe, 1, catalog.ModifiedTime{})
	records[healthyFile.NodeID()] = healthyRecord

	source := revisionSourceFunc(func(_ context.Context, record catalog.NodeRecord) (StableFile, error) {
		file, _ := record.FileID()
		if file == healthyFile {
			return &testStableFile{data: []byte{42}}, nil
		}
		if binary.BigEndian.Uint64(file[8:])%2 == 0 {
			// Mismatch during open (size differs from catalog).
			return &testStableFile{data: []byte{1, 2}}, nil
		}
		// Succeeds on open, then drifts during ReadBlock.
		return &testStableFile{data: []byte{1}}, nil
	})
	pool, err := NewHandlePool(HandlePoolConfig{
		Limits:     CapacityLimits{StableHandles: 100, ActiveLeases: 100},
		RetryAfter: DefaultCapacityRetryAfter,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	store, err := NewRevisionStore(RevisionStoreConfig{
		ShareInstance: catalogID[catalog.ShareInstance](1), ChunkSize: catalog.MinChunkSize,
		Catalog: testCatalog{records: records}, Source: source, Clock: &testClock{now: time.Unix(100, 0)},
		HandlePool:  pool,
		ShareLimits: CapacityLimits{StableHandles: 100, ActiveLeases: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	session := generousSession(t, store, "churn-session")

	for index, file := range staleFiles {
		if (index+1)%2 == 0 {
			if _, err := store.OpenRevision(context.Background(), file, session); !errors.Is(err, ErrRevisionStale) {
				t.Fatalf("stale open %d = %v", index, err)
			}
			continue
		}
		lease, err := store.OpenRevision(context.Background(), file, session)
		if err != nil {
			t.Fatalf("open before drift %d = %v", index, err)
		}
		store.pool.mu.Lock()
		stable := store.revisions[file].source.(*testStableFile)
		store.pool.mu.Unlock()
		stable.drifted.Store(true)
		ref, _ := NewBlockRef(file, lease.Descriptor().FileRevision(), 0, lease.Descriptor().Geometry())
		if _, err := store.ReadBlock(context.Background(), lease.ID(), ref); !errors.Is(err, ErrRevisionDrift) {
			t.Fatalf("drift read %d = %v", index, err)
		}
	}

	healthyLease, err := store.OpenRevision(context.Background(), healthyFile, session)
	if err != nil {
		t.Fatalf("healthy file blocked after %d invalidations/stale opens: %v", churnCount, err)
	}
	ref, _ := NewBlockRef(healthyFile, healthyLease.Descriptor().FileRevision(), 0, healthyLease.Descriptor().Geometry())
	data, err := store.ReadBlock(context.Background(), healthyLease.ID(), ref)
	if err != nil || len(data) != 1 || data[0] != 42 {
		t.Fatalf("healthy file read = %v, %v", data, err)
	}
	if err := store.EndLease(healthyLease.ID(), LeaseRelinquished); err != nil {
		t.Fatal(err)
	}
	if got := store.CapacitySnapshot().Process().Used(); got != (CapacityUsage{}) {
		t.Fatalf("churn leaked capacity: %+v", got)
	}
}
