package revisioncapacity

import (
	"context"
	"testing"
)

func TestCrossStoreReclaimKeepsObservationOwnership(t *testing.T) {
	limits := CapacityLimits{StableHandles: 1, ActiveLeases: 4}
	owner := newTestOwner(t, limits)
	type observedStore struct {
		store   *StoreRegistration
		session *SessionRegistration
		events  []TraceEvent
	}
	register := func(id StoreID) *observedStore {
		observed := &observedStore{}
		var err error
		observed.store, err = owner.Coordinator().RegisterStore(StoreConfig{
			StoreID: id, ShareID: ShareID(id), Limits: limits,
			Tracer: TracerFunc(func(event TraceEvent) {
				// Inspection would deadlock if delivery still held the budget lock.
				_ = observed.store.Snapshot()
				observed.events = append(observed.events, event)
			}),
		}, reclaimTargetFunc(func(_ context.Context, claim ReclaimClaim) ReclaimResult { return ReclaimCompleted(claim, nil) }))
		if err != nil {
			t.Fatal(err)
		}
		observed.session, err = observed.store.RegisterSession(SessionConfig{SessionID: SessionID(id), Limits: limits})
		if err != nil {
			t.Fatal(err)
		}
		return observed
	}
	victim, requester := register("victim"), register("requester")
	charges := commitAdmission(t, victim.store, victim.session, AdmissionNewRevision, "old-revision")
	stable := releaseTransientCharges(t, charges)
	if err := victim.store.PublishIdle(IdleCandidate{
		Token: "old-idle", RevisionID: "old-revision", RecoveryUntil: testRecoveryEpoch, LifecycleGeneration: 1, StableHandle: stable,
	}); err != nil {
		t.Fatal(err)
	}
	admitted := commitAdmission(t, requester.store, requester.session, AdmissionNewRevision, "new-revision")
	if got := requester.store.Snapshot().Process().Used().StableHandles; got != 1 {
		t.Fatalf("aggregate handles=%d", got)
	}
	for _, observed := range []*observedStore{victim, requester} {
		for _, event := range observed.events {
			if event.StoreID() != observed.store.StoreID() || event.ShareID() != observed.store.ShareID() {
				t.Fatalf("cross-store event: owner=%s event=%+v", observed.store.StoreID(), event)
			}
		}
	}
	if len(victim.events) != 2 || victim.events[1].Stage() != TraceIdlePublished {
		t.Fatalf("victim publication=%+v", victim.events)
	}
	if len(requester.events) != 3 || requester.events[0].Stage() != TraceReclaimClaimed || requester.events[1].Stage() != TraceReclaimCompleted || requester.events[2].Stage() != TraceAdmissionGranted {
		t.Fatalf("requester publication=%+v", requester.events)
	}
	if err := admitted.Release(); err != nil {
		t.Fatal(err)
	}
	closeRegistrationTree(t, owner, victim.session, requester.session, victim.store, requester.store)
}

func TestStoreObserverPanicDoesNotChangeAdmissionOrProcessEvidence(t *testing.T) {
	limits := CapacityLimits{StableHandles: 1, ActiveLeases: 1}
	var processEvents []TraceEvent
	owner, err := NewProcessOwner(ProcessConfig{Limits: limits, RetryAfter: DefaultCapacityRetryAfter, Tracer: TracerFunc(func(event TraceEvent) { processEvents = append(processEvents, event) })})
	if err != nil {
		t.Fatal(err)
	}
	store, err := owner.Coordinator().RegisterStore(StoreConfig{StoreID: "store", ShareID: "share", Limits: limits,
		Tracer: TracerFunc(func(TraceEvent) { panic("observer failed") }),
	}, reclaimTargetFunc(decliningTarget))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.RegisterSession(SessionConfig{SessionID: "session", Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	charges := commitAdmission(t, store, session, AdmissionNewRevision, "revision")
	if len(processEvents) != 1 || processEvents[0].Stage() != TraceAdmissionGranted {
		t.Fatalf("process evidence=%+v", processEvents)
	}
	if err := charges.Release(); err != nil {
		t.Fatal(err)
	}
	closeRegistrationTree(t, owner, session, store)
}
