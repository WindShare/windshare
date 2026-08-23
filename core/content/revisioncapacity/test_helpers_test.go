package revisioncapacity

import (
	"context"
	"testing"
	"time"
)

var testRecoveryEpoch = time.Date(2100, time.January, 1, 0, 0, 0, 0, time.UTC)

type reclaimTargetFunc func(context.Context, ReclaimClaim) ReclaimResult

func (f reclaimTargetFunc) ReclaimIdle(ctx context.Context, claim ReclaimClaim) ReclaimResult {
	return f(ctx, claim)
}

func decliningTarget(_ context.Context, claim ReclaimClaim) ReclaimResult {
	return ReclaimDeclined(claim)
}

func newTestOwner(t *testing.T, limits CapacityLimits) *ProcessOwner {
	t.Helper()
	owner, err := NewProcessOwner(ProcessConfig{Limits: limits, RetryAfter: DefaultCapacityRetryAfter})
	if err != nil {
		t.Fatalf("new process owner: %v", err)
	}
	return owner
}

func registerTestStore(t *testing.T, owner *ProcessOwner, storeID StoreID, limits CapacityLimits, target ReclaimTarget) (*StoreRegistration, *SessionRegistration) {
	return registerTestStoreWithSessionLimits(t, owner, storeID, limits, limits, target)
}

func registerTestStoreWithSessionLimits(t *testing.T, owner *ProcessOwner, storeID StoreID, storeLimits, sessionLimits CapacityLimits, target ReclaimTarget) (*StoreRegistration, *SessionRegistration) {
	t.Helper()
	store, err := owner.Coordinator().RegisterStore(StoreConfig{
		StoreID: storeID, ShareID: ShareID("share-" + string(storeID)), Limits: storeLimits,
	}, target)
	if err != nil {
		t.Fatalf("register store %q: %v", storeID, err)
	}
	session, err := store.RegisterSession(SessionConfig{
		SessionID: SessionID("session-" + string(storeID)), Limits: sessionLimits,
	})
	if err != nil {
		t.Fatalf("register session for %q: %v", storeID, err)
	}
	return store, session
}

func commitAdmission(t *testing.T, store *StoreRegistration, session *SessionRegistration, kind AdmissionKind, revision RevisionID) AdmissionCharges {
	t.Helper()
	grant, err := store.Admit(context.Background(), AdmissionRequest{Kind: kind, RevisionID: revision, Session: session})
	if err != nil {
		t.Fatalf("admit %q: %v", revision, err)
	}
	charges, err := grant.Commit()
	if err != nil {
		t.Fatalf("commit %q: %v", revision, err)
	}
	return charges
}

func releaseTransientCharges(t *testing.T, charges AdmissionCharges) StableHandleCharge {
	t.Helper()
	lease, ok := charges.ActiveLease()
	if !ok {
		t.Fatal("admission did not return an active-lease charge")
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("release active lease: %v", err)
	}
	sessionHandle, ok := charges.SessionHandle()
	if !ok {
		t.Fatal("admission did not return a session-handle charge")
	}
	if err := sessionHandle.Release(); err != nil {
		t.Fatalf("release session handle: %v", err)
	}
	stable, ok := charges.StableHandle()
	if !ok {
		t.Fatal("admission did not return a stable-handle charge")
	}
	return stable
}

func closeRegistrationTree(t *testing.T, owner *ProcessOwner, registrations ...any) {
	t.Helper()
	for _, registration := range registrations {
		switch value := registration.(type) {
		case *SessionRegistration:
			if err := value.Close(); err != nil {
				t.Fatalf("close session %q: %v", value.SessionID(), err)
			}
		case *StoreRegistration:
			if err := value.Close(); err != nil {
				t.Fatalf("close store %q: %v", value.StoreID(), err)
			}
		}
	}
	if err := owner.Close(); err != nil {
		t.Fatalf("close process owner: %v", err)
	}
}
