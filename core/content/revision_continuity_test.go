package content

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
)

type continuityRevisionSource struct {
	*testRevisionSource
	continuity RevisionContinuity
	err        error
}

func (s *continuityRevisionSource) RevisionContinuity(catalog.NodeRecord) (RevisionContinuity, error) {
	return s.continuity, s.err
}

func TestRevisionContinuityRetainsResidentProgressAndSeparatesWeakReopen(t *testing.T) {
	for _, continuity := range []RevisionContinuity{CatalogRevisionContinuity, OpenHandleRevisionContinuity} {
		t.Run(map[RevisionContinuity]string{CatalogRevisionContinuity: "catalog", OpenHandleRevisionContinuity: "open-handle"}[continuity], func(t *testing.T) {
			file, record := fileRecord(t, 1)
			source := &continuityRevisionSource{
				testRevisionSource: &testRevisionSource{files: []*testStableFile{
					{data: []byte{1}}, {data: []byte{1}},
				}},
				continuity: continuity,
			}
			store, process, _ := newRevisionStore(t, source, &testClock{now: time.Unix(100, 0)}, nil, file, record)
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
				t.Fatal("a resident write-excluding handle lost its shared revision")
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
			same := reopened.Descriptor().FileRevision() == first.Descriptor().FileRevision()
			if same != (continuity == CatalogRevisionContinuity) {
				t.Fatalf("reopened revision continuity=%v same=%v", continuity, same)
			}
			if source.Calls() != 2 || source.files[0].closed.Load() != 1 {
				t.Fatal("reopen did not release and reacquire exactly one stable handle")
			}
			if continuity == OpenHandleRevisionContinuity {
				old, err := NewBlockRef(file, first.Descriptor().FileRevision(), 0, first.Descriptor().Geometry())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.ReadBlock(context.Background(), reopened.ID(), old); err == nil {
					t.Fatal("reopened handle accepted a range from the previous revision")
				}
			}
			if err := store.EndLease(reopened.ID(), LeaseRelinquished); err != nil {
				t.Fatal(err)
			}
			if got := process.Snapshot().Used; got != (QuotaUsage{}) {
				t.Fatalf("reopening leaked capacity: %+v", got)
			}
		})
	}
}

func TestOpenHandleContinuityPreservesDetachedLeaseDuringResumeGrace(t *testing.T) {
	file, record := fileRecord(t, 1)
	source := &continuityRevisionSource{
		testRevisionSource: &testRevisionSource{files: []*testStableFile{{data: []byte{1}}}},
		continuity:         OpenHandleRevisionContinuity,
	}
	store, _, _ := newRevisionStore(t, source, &testClock{now: time.Unix(100, 0)}, nil, file, record)
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
		t.Fatal("network reconnect discarded a still-proven revision during grace")
	}
}

func TestRevisionContinuityFailureDoesNotAcquireSourceOrCapacity(t *testing.T) {
	failure := errors.New("source metadata capability unavailable")
	for _, test := range []struct {
		name       string
		continuity RevisionContinuity
		err        error
	}{
		{name: "unknown"},
		{name: "provider failure", continuity: CatalogRevisionContinuity, err: failure},
	} {
		t.Run(test.name, func(t *testing.T) {
			file, record := fileRecord(t, 1)
			source := &continuityRevisionSource{
				testRevisionSource: &testRevisionSource{files: []*testStableFile{{data: []byte{1}}}},
				continuity:         test.continuity, err: test.err,
			}
			store, process, _ := newRevisionStore(t, source, &testClock{now: time.Unix(100, 0)}, nil, file, record)
			session := generousSession(t, store, "rejected")
			_, err := store.OpenRevision(context.Background(), file, session)
			if err == nil || (test.err != nil && !errors.Is(err, test.err)) {
				t.Fatalf("continuity error=%v", err)
			}
			if source.Calls() != 0 || process.Snapshot().Used != (QuotaUsage{}) {
				t.Fatal("failed continuity selection acquired file resources")
			}
		})
	}
}

func TestRejectedOpenHandleAttemptsDoNotExhaustShareInvalidationBudget(t *testing.T) {
	file, record := fileRecord(t, 1)
	source := &continuityRevisionSource{
		testRevisionSource: &testRevisionSource{files: []*testStableFile{
			{data: []byte{1, 2}}, {data: []byte{1, 2}}, {data: []byte{1, 2}}, {data: []byte{1}},
		}},
		continuity: OpenHandleRevisionContinuity,
	}
	invalidator := &recordingInvalidator{}
	store, process, _ := newRevisionStore(t, source, &testClock{now: time.Unix(100, 0)}, invalidator, file, record)
	store.metadataBudget = testRevisionMetadataBudget(t, 1)
	session := generousSession(t, store, "stale-retry")
	for range 3 {
		if _, err := store.OpenRevision(context.Background(), file, session); !errors.Is(err, ErrRevisionStale) {
			t.Fatalf("stale weak candidate error=%v", err)
		}
	}
	if store.metadataBudget.Snapshot().Used != 0 || len(invalidator.revisions) != 0 ||
		process.Snapshot().Used != (QuotaUsage{}) {
		t.Fatal("unpublished one-use identities retained invalidation authority or resources")
	}
	if _, err := store.OpenRevision(context.Background(), file, session); err != nil {
		t.Fatalf("prior rejected opens stopped fresh valid content: %v", err)
	}
}

func TestOpenRevisionEvidenceSeparatesInstancesAndCatalogDomain(t *testing.T) {
	evidence := knownRevisionEvidence(t)
	deriver := testRevisionDeriver(t)
	t.Cleanup(deriver.Destroy)
	catalogID, err := deriver.DeriveRevision(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Continuity() != CatalogRevisionContinuity || evidence.OpenInstance() != [sha256.Size]byte{} {
		t.Fatal("catalog evidence claimed a live instance")
	}
	first := evidence
	first.openInstance[0] = 1
	second := first
	second.openInstance[0] = 2
	firstID, err := deriver.DeriveRevision(first)
	if err != nil {
		t.Fatal(err)
	}
	repeatedID, err := deriver.DeriveRevision(first)
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := deriver.DeriveRevision(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstID == catalogID || secondID == catalogID || firstID == secondID || repeatedID != firstID {
		t.Fatal("revision derivation failed to bind the open instance independently")
	}
	if first.Continuity() != OpenHandleRevisionContinuity || first.OpenInstance() == [sha256.Size]byte{} {
		t.Fatal("open evidence lost its lifetime")
	}
	projected := first.OpenInstance()
	projected[0]++
	if first.OpenInstance() == projected {
		t.Fatal("projected nonce mutated evidence")
	}
}
