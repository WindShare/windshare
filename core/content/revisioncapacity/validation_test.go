package revisioncapacity

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRegistrationValidationAndTerminalCloseContracts(t *testing.T) {
	limits := CapacityLimits{StableHandles: 2, ActiveLeases: 2}
	if _, err := NewProcessOwner(ProcessConfig{}); err == nil {
		t.Fatal("zero process config was accepted")
	}
	if _, err := NewProcessOwner(ProcessConfig{Limits: limits, RetryAfter: time.Microsecond}); err == nil {
		t.Fatal("sub-millisecond retry delay was accepted")
	}
	owner := newTestOwner(t, limits)
	coordinator := owner.Coordinator()
	store, err := coordinator.RegisterStore(StoreConfig{
		StoreID: "store", ShareID: "share", Limits: limits,
	}, reclaimTargetFunc(decliningTarget))
	if err != nil {
		t.Fatalf("register store: %v", err)
	}
	if _, err := coordinator.RegisterStore(StoreConfig{
		StoreID: "store", ShareID: "other-share", Limits: limits,
	}, reclaimTargetFunc(decliningTarget)); err == nil {
		t.Fatal("duplicate store identity was accepted")
	}
	if _, err := coordinator.RegisterStore(StoreConfig{
		StoreID: "other-store", ShareID: "share", Limits: limits,
	}, reclaimTargetFunc(decliningTarget)); err == nil {
		t.Fatal("duplicate share authority was accepted")
	}
	session, err := store.RegisterSession(SessionConfig{SessionID: "session", Limits: limits})
	if err != nil {
		t.Fatalf("register session: %v", err)
	}
	if _, err := store.RegisterSession(SessionConfig{SessionID: "session", Limits: limits}); err == nil {
		t.Fatal("duplicate session identity was accepted")
	}
	charges := commitAdmission(t, store, session, AdmissionNewRevision, "revision")
	var liveSessions *LiveSessionRegistrationsError
	if err := store.Close(); !errors.As(err, &liveSessions) || liveSessions.Count() != 1 {
		t.Fatalf("store close with session = %v", err)
	}
	var liveOwnership *LiveCapacityOwnershipError
	if err := session.Close(); !errors.As(err, &liveOwnership) || liveOwnership.Scope() != CapacityScopeSession {
		t.Fatalf("session close with charges = %v", err)
	}
	if err := charges.Release(); err != nil {
		t.Fatalf("release charges: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("retry session close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("retry store close: %v", err)
	}
	if _, err := store.RegisterSession(SessionConfig{SessionID: "late", Limits: limits}); !errors.Is(err, ErrRegistrationClosing) {
		t.Fatalf("late session registration = %v", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatalf("close owner: %v", err)
	}
	if _, err := coordinator.RegisterStore(StoreConfig{
		StoreID: "late", ShareID: "late-share", Limits: limits,
	}, reclaimTargetFunc(decliningTarget)); !errors.Is(err, ErrCoordinatorClosed) {
		t.Fatalf("late store registration = %v", err)
	}
}

func TestAdmissionRejectsForeignSessionAndInvalidCandidateOwnership(t *testing.T) {
	limits := CapacityLimits{StableHandles: 4, ActiveLeases: 4}
	owner := newTestOwner(t, limits)
	storeA, sessionA := registerTestStore(t, owner, "a", limits, reclaimTargetFunc(decliningTarget))
	storeB, sessionB := registerTestStore(t, owner, "b", limits, reclaimTargetFunc(decliningTarget))
	if _, err := storeA.Admit(context.Background(), AdmissionRequest{
		Kind: AdmissionNewRevision, RevisionID: "revision", Session: sessionB,
	}); err == nil {
		t.Fatal("foreign session admission was accepted")
	}
	if _, err := storeA.Admit(context.Background(), AdmissionRequest{
		Kind: 255, RevisionID: "revision", Session: sessionA,
	}); err == nil {
		t.Fatal("invalid admission kind was accepted")
	}
	charges := commitAdmission(t, storeA, sessionA, AdmissionNewRevision, "revision")
	stable := releaseTransientCharges(t, charges)
	if err := storeB.PublishIdle(IdleCandidate{
		Token: "foreign", RevisionID: "revision", RecoveryUntil: testRecoveryEpoch.Add(5 * time.Minute),
		LifecycleGeneration: 1, StableHandle: stable,
	}); err == nil {
		t.Fatal("foreign stable-handle publication was accepted")
	}
	valid := IdleCandidate{
		Token: "valid", RevisionID: "revision", RecoveryUntil: testRecoveryEpoch.Add(5 * time.Minute),
		LifecycleGeneration: 1, StableHandle: stable,
	}
	if err := storeA.PublishIdle(valid); err != nil {
		t.Fatalf("publish valid candidate: %v", err)
	}
	if err := storeA.PublishIdle(valid); err != nil {
		t.Fatalf("idempotent publication: %v", err)
	}
	if !storeA.WithdrawIdle(valid.Token) || storeA.WithdrawIdle(valid.Token) {
		t.Fatal("idle withdrawal did not report exact indexed ownership")
	}
	if err := stable.Release(); err != nil {
		t.Fatalf("release withdrawn stable handle: %v", err)
	}
	closeRegistrationTree(t, owner, sessionA, sessionB, storeA, storeB)
}

func TestTracerPanicCannotChangeCapacityDecision(t *testing.T) {
	limits := CapacityLimits{StableHandles: 1, ActiveLeases: 1}
	owner, err := NewProcessOwner(ProcessConfig{
		Limits: limits, RetryAfter: DefaultCapacityRetryAfter,
		Tracer: TracerFunc(func(TraceEvent) { panic("observer") }),
	})
	if err != nil {
		t.Fatalf("new traced owner: %v", err)
	}
	store, session := registerTestStore(t, owner, "store", limits, reclaimTargetFunc(decliningTarget))
	grant, err := store.Admit(context.Background(), AdmissionRequest{
		Kind: AdmissionNewRevision, RevisionID: "revision", Session: session,
	})
	if err != nil {
		t.Fatalf("tracer panic changed admission: %v", err)
	}
	if grant.DecisionID() == "" {
		t.Fatal("grant omitted its capacity decision ID")
	}
	if err := grant.Abort(); err != nil {
		t.Fatalf("abort traced grant: %v", err)
	}
	closeRegistrationTree(t, owner, session, store)
}
