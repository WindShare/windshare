package contentflow

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content/revisioncapacity"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type decisionTraceReclaimTarget struct{}

func (decisionTraceReclaimTarget) ReclaimIdle(context.Context, revisioncapacity.ReclaimClaim) revisioncapacity.ReclaimResult {
	return revisioncapacity.ReclaimResult{}
}

func TestCapacityFailureKeepsCoordinatorDecisionForSenderTrace(t *testing.T) {
	limits := revisioncapacity.CapacityLimits{StableHandles: 1, ActiveLeases: 1}
	owner, err := revisioncapacity.NewProcessOwner(revisioncapacity.ProcessConfig{
		Limits: limits, RetryAfter: revisioncapacity.DefaultCapacityRetryAfter,
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := owner.Coordinator().RegisterStore(revisioncapacity.StoreConfig{
		StoreID: "trace-store", ShareID: "trace-share", Limits: limits,
	}, decisionTraceReclaimTarget{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.RegisterSession(revisioncapacity.SessionConfig{SessionID: "trace-session", Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := store.Admit(context.Background(), revisioncapacity.AdmissionRequest{
		Kind: revisioncapacity.AdmissionNewRevision, RevisionID: "revision-one", Session: session,
	})
	if err != nil {
		t.Fatal(err)
	}
	charges, err := grant.Commit()
	if err != nil {
		t.Fatal(err)
	}
	_, busyErr := store.Admit(context.Background(), revisioncapacity.AdmissionRequest{
		Kind: revisioncapacity.AdmissionAdditionalSessionLease, RevisionID: "revision-one", Session: session,
	})
	var busy *revisioncapacity.CapacityBusyError
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
	operation := protocolsession.OperationID{1}
	handler.traceCapacityDecisions(operation, results)
	if got.Stage != SenderDecisionCapacityBusy || got.OperationID != operation ||
		got.RequestKind != protocolsession.MessageOpenRevisions || got.CapacityDecisionID != want {
		t.Fatalf("sender decision trace = %#v", got)
	}
	if err := charges.Release(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
}
