package contentflow

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestCapacityFailureKeepsPoolDecisionForSenderTrace(t *testing.T) {
	limits := content.CapacityLimits{StableHandles: 1, ActiveLeases: 1}
	pool, err := content.NewHandlePool(content.HandlePoolConfig{
		Limits: limits, RetryAfter: content.DefaultCapacityRetryAfter,
	})
	if err != nil {
		t.Fatal(err)
	}
	share := flowID[catalog.ShareInstance](91)
	file := flowID[catalog.FileID](92)
	parent := flowID[catalog.DirectoryID](93)
	locator, _ := catalog.NewSourceReference([]byte("object:trace.bin"))
	sourceIdentity, _ := catalog.NewSourceIdentity([]byte("trace-source"))
	candidate, _ := catalog.NewVersionCandidate([]byte("trace-version"))
	data := make([]byte, catalog.MinChunkSize)
	record, err := catalog.NewFileNodeRecord(file, parent, "trace.bin", locator, sourceIdentity, candidate, uint64(len(data)), catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	store, err := content.NewRevisionStore(content.RevisionStoreConfig{
		ShareInstance: share, ChunkSize: catalog.MinChunkSize,
		Catalog:    runtimeCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}},
		Source:     runtimeSource{file: &runtimeStableFile{data: data}},
		HandlePool: pool, ShareLimits: limits, LeaseIDs: &runtimeIDs{},
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.RegisterSession(content.SessionConfig{SessionID: "trace-session", Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := session.OpenRevision(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	_, busyErr := session.OpenRevision(context.Background(), file)
	var busy *content.CapacityBusyError
	if !errors.As(busyErr, &busy) {
		t.Fatalf("capacity error = %v", busyErr)
	}
	want := busy.DecisionID()
	failure := classifyRevisionError(busyErr)
	if failure.CapacityDecisionID() != want {
		t.Fatalf("classified decision = %q, want %q", failure.CapacityDecisionID(), want)
	}
	failed, err := FailedOpen(catalog.FileID{1}, failure)
	if err != nil {
		t.Fatal(err)
	}
	results, err := NewOpenResults([]OpenResult{failed})
	if err != nil {
		t.Fatal(err)
	}
	var got SenderDecisionTrace
	handler := &SenderHandler{decisionTracer: SenderDecisionTraceFunc(func(event SenderDecisionTrace) { got = event })}
	operation := protocolsession.OperationID(1)
	handler.traceCapacityDecisions(operation, results)
	if got.Stage != SenderDecisionCapacityBusy || got.OperationID != operation ||
		got.RequestKind != protocolsession.MessageOpenRevisions || got.CapacityDecisionID != want {
		t.Fatalf("sender decision trace = %#v", got)
	}
	if err := session.EndLease(lease.ID(), content.LeaseRelinquished); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
}
