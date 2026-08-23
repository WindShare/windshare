package cli

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/capacitytrace"
	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/core/content/revisioncapacity"
)

type senderObservationReclaimTarget struct{}

func (senderObservationReclaimTarget) ReclaimIdle(context.Context, revisioncapacity.ReclaimClaim) revisioncapacity.ReclaimResult {
	return revisioncapacity.ReclaimResult{}
}

func TestProcessCapacityTraceRouterProjectsActiveShareDecisionCorrelation(t *testing.T) {
	emitter := &shareRecordingEmitter{detailed: true, trace: true}
	observations := newShareObservations(emitter)
	router := &capacitytrace.Router{}
	releaseTrace := router.Bind(observations.capacityTracer())

	limits := revisioncapacity.CapacityLimits{StableHandles: 1, ActiveLeases: 1}
	config := revisioncapacity.ProcessConfig{
		Limits: limits, RetryAfter: revisioncapacity.DefaultCapacityRetryAfter, Tracer: router,
	}
	owner, err := revisioncapacity.NewProcessOwner(config)
	if err != nil {
		t.Fatal(err)
	}
	store, err := owner.Coordinator().RegisterStore(revisioncapacity.StoreConfig{
		StoreID: "cli-trace-store", ShareID: "cli-trace-share", Limits: limits,
	}, senderObservationReclaimTarget{})
	if err != nil {
		t.Fatal(err)
	}
	rawSession := make([]byte, clievent.IdentityBytes)
	rawSession[0] = 0x51
	session, err := store.RegisterSession(revisioncapacity.SessionConfig{
		SessionID: revisioncapacity.SessionID(base64.RawURLEncoding.EncodeToString(rawSession)), Limits: limits,
	})
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
		t.Fatalf("capacity denial = %v", busyErr)
	}
	wantDecision, _ := clievent.NewCapacityDecisionID(string(busy.DecisionID()))
	var denied clievent.SenderCapacityObserved
	for _, event := range emitter.events {
		candidate, ok := event.(clievent.SenderCapacityObserved)
		if ok && candidate.Stage() == clievent.SenderCapacityAdmissionDenied {
			denied = candidate
		}
	}
	decision, decisionOK := denied.DecisionID()
	protocolSession, sessionOK := denied.ProtocolSessionID()
	sessionScope, scopeOK := denied.SessionSnapshot()
	if !decisionOK || decision != wantDecision || !sessionOK || protocolSession.Hex() != hex.EncodeToString(rawSession) ||
		!scopeOK || sessionScope.ActiveLeases != 1 || denied.ProcessSnapshot().ActiveLeases != 1 {
		t.Fatalf("projected capacity denial = %#v", denied)
	}

	beforeUnbind := len(emitter.events)
	releaseTrace()
	if err := charges.Release(); err != nil {
		t.Fatal(err)
	}
	grant, err = store.Admit(context.Background(), revisioncapacity.AdmissionRequest{
		Kind: revisioncapacity.AdmissionNewRevision, RevisionID: "revision-two", Session: session,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := grant.Abort(); err != nil {
		t.Fatal(err)
	}
	if len(emitter.events) != beforeUnbind {
		t.Fatal("unbound process tracer leaked capacity events into a completed share")
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
