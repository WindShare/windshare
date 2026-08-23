package revisioncapacity

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestReclaimSelectsOldestCandidateByRecoveryStoreAndRevision(t *testing.T) {
	processLimits := CapacityLimits{StableHandles: 4, ActiveLeases: 32}
	shareLimits := CapacityLimits{StableHandles: 8, ActiveLeases: 32}
	owner := newTestOwner(t, processLimits)

	var selectedMu sync.Mutex
	selected := make([]CandidateToken, 0, 4)
	target := reclaimTargetFunc(func(_ context.Context, claim ReclaimClaim) ReclaimResult {
		selectedMu.Lock()
		selected = append(selected, claim.CandidateToken())
		selectedMu.Unlock()
		return ReclaimCompleted(claim, nil)
	})
	storeZ, sessionZ := registerTestStore(t, owner, "z-store", shareLimits, target)
	storeA, sessionA := registerTestStore(t, owner, "a-store", shareLimits, target)
	storeB, sessionB := registerTestStore(t, owner, "b-store", shareLimits, target)
	requester, requesterSession := registerTestStore(t, owner, "requester", shareLimits, reclaimTargetFunc(decliningTarget))

	base := testRecoveryEpoch
	type published struct {
		store      *StoreRegistration
		session    *SessionRegistration
		token      CandidateToken
		revision   RevisionID
		recovery   time.Time
		generation uint64
	}
	publications := []published{
		{store: storeZ, session: sessionZ, token: "token-z-oldest", revision: "revision-z", recovery: base.Add(-time.Second), generation: 1},
		{store: storeA, session: sessionA, token: "token-a-b", revision: "revision-b", recovery: base, generation: 2},
		{store: storeA, session: sessionA, token: "token-a-a", revision: "revision-a", recovery: base, generation: 3},
		{store: storeB, session: sessionB, token: "token-b-a", revision: "revision-a", recovery: base, generation: 4},
	}
	victimHandles := make([]StableHandleCharge, 0, len(publications))
	for _, publication := range publications {
		charges := commitAdmission(t, publication.store, publication.session, AdmissionNewRevision, publication.revision)
		stable := releaseTransientCharges(t, charges)
		if err := publication.store.PublishIdle(IdleCandidate{
			Token: publication.token, RevisionID: publication.revision, RecoveryUntil: publication.recovery,
			LifecycleGeneration: publication.generation, StableHandle: stable,
		}); err != nil {
			t.Fatalf("publish %q: %v", publication.token, err)
		}
		victimHandles = append(victimHandles, stable)
	}

	requesterCharges := make([]AdmissionCharges, 0, len(publications))
	for index := range publications {
		requesterCharges = append(requesterCharges, commitAdmission(
			t, requester, requesterSession, AdmissionNewRevision, RevisionID("requester-"+string(rune('a'+index))),
		))
	}
	wantOrder := []CandidateToken{"token-z-oldest", "token-a-a", "token-a-b", "token-b-a"}
	selectedMu.Lock()
	if len(selected) != len(wantOrder) {
		t.Fatalf("selected %v, want %v", selected, wantOrder)
	}
	for index := range wantOrder {
		if selected[index] != wantOrder[index] {
			t.Fatalf("selection order = %v, want %v", selected, wantOrder)
		}
	}
	selectedMu.Unlock()
	if got := requester.Snapshot().Process().Used().StableHandles; got != processLimits.StableHandles {
		t.Fatalf("process stable handles after handoffs = %d, want %d", got, processLimits.StableHandles)
	}
	for _, victim := range victimHandles {
		if err := victim.Release(); !errors.Is(err, ErrOwnershipResolved) {
			t.Fatalf("transferred victim release = %v, want ErrOwnershipResolved", err)
		}
	}
	for _, charges := range requesterCharges {
		if err := charges.Release(); err != nil {
			t.Fatalf("release requester charges: %v", err)
		}
	}
	closeRegistrationTree(t, owner,
		sessionZ, sessionA, sessionB, requesterSession,
		storeZ, storeA, storeB, requester,
	)
}

func TestShareBoundaryNeverReclaimsAnotherShare(t *testing.T) {
	processLimits := CapacityLimits{StableHandles: 8, ActiveLeases: 8}
	shareLimits := CapacityLimits{StableHandles: 1, ActiveLeases: 8}
	owner := newTestOwner(t, processLimits)

	called := false
	victim, victimSession := registerTestStore(t, owner, "victim", shareLimits, reclaimTargetFunc(func(_ context.Context, claim ReclaimClaim) ReclaimResult {
		called = true
		return ReclaimCompleted(claim, nil)
	}))
	requester, requesterSession := registerTestStoreWithSessionLimits(
		t, owner, "requester", shareLimits, processLimits, reclaimTargetFunc(decliningTarget),
	)
	victimCharges := commitAdmission(t, victim, victimSession, AdmissionNewRevision, "victim-revision")
	victimStable := releaseTransientCharges(t, victimCharges)
	if err := victim.PublishIdle(IdleCandidate{
		Token: "victim-idle", RevisionID: "victim-revision", RecoveryUntil: testRecoveryEpoch.Add(time.Minute),
		LifecycleGeneration: 1, StableHandle: victimStable,
	}); err != nil {
		t.Fatalf("publish victim: %v", err)
	}
	requesterCharges := commitAdmission(t, requester, requesterSession, AdmissionNewRevision, "requester-resident")

	_, err := requester.Admit(context.Background(), AdmissionRequest{
		Kind: AdmissionNewRevision, RevisionID: "requester-next", Session: requesterSession,
	})
	var busy *CapacityBusyError
	if !errors.As(err, &busy) || busy.Scope() != CapacityScopeShare || busy.Resource() != CapacityResourceStableHandle {
		t.Fatalf("share-blocked admission error = %v", err)
	}
	if called {
		t.Fatal("share boundary invoked a target owned by another share")
	}
	if err := requesterCharges.Release(); err != nil {
		t.Fatalf("release requester: %v", err)
	}
	victim.WithdrawIdle("victim-idle")
	if err := victimStable.Release(); err != nil {
		t.Fatalf("release victim stable handle: %v", err)
	}
	closeRegistrationTree(t, owner, victimSession, requesterSession, victim, requester)
}

func TestClaimReactivationWithdrawsCandidateWithoutLosingItsCharge(t *testing.T) {
	processLimits := CapacityLimits{StableHandles: 1, ActiveLeases: 8}
	shareLimits := CapacityLimits{StableHandles: 4, ActiveLeases: 8}
	owner := newTestOwner(t, processLimits)

	var victim *StoreRegistration
	target := reclaimTargetFunc(func(_ context.Context, claim ReclaimClaim) ReclaimResult {
		if !victim.WithdrawIdle(claim.CandidateToken()) {
			t.Error("reactivation did not find the claimed candidate")
		}
		return ReclaimDeclined(claim)
	})
	victim, victimSession := registerTestStore(t, owner, "victim", shareLimits, target)
	requester, requesterSession := registerTestStore(t, owner, "requester", shareLimits, reclaimTargetFunc(decliningTarget))
	victimCharges := commitAdmission(t, victim, victimSession, AdmissionNewRevision, "victim-revision")
	victimStable := releaseTransientCharges(t, victimCharges)
	if err := victim.PublishIdle(IdleCandidate{
		Token: "reactivating", RevisionID: "victim-revision", RecoveryUntil: testRecoveryEpoch.Add(2 * time.Minute),
		LifecycleGeneration: 7, StableHandle: victimStable,
	}); err != nil {
		t.Fatalf("publish candidate: %v", err)
	}

	_, err := requester.Admit(context.Background(), AdmissionRequest{
		Kind: AdmissionNewRevision, RevisionID: "requester-revision", Session: requesterSession,
	})
	var busy *CapacityBusyError
	if !errors.As(err, &busy) || busy.Scope() != CapacityScopeProcess {
		t.Fatalf("post-reactivation admission error = %v, want process busy", err)
	}
	snapshot := victim.Snapshot()
	if snapshot.Process().Used().StableHandles != 1 || snapshot.Process().ReclaimableStableHandles() != 0 {
		t.Fatalf("reactivated snapshot used=%+v reclaimable=%d", snapshot.Process().Used(), snapshot.Process().ReclaimableStableHandles())
	}
	if err := victimStable.Release(); err != nil {
		t.Fatalf("reactivated stable handle release: %v", err)
	}
	closeRegistrationTree(t, owner, victimSession, requesterSession, victim, requester)
}

func TestAdmissionWaitsForTerminalCloseAndPreservesDiagnostic(t *testing.T) {
	processLimits := CapacityLimits{StableHandles: 1, ActiveLeases: 8}
	shareLimits := CapacityLimits{StableHandles: 4, ActiveLeases: 8}
	owner := newTestOwner(t, processLimits)
	entered := make(chan struct{})
	closeReturned := make(chan struct{})
	diagnostic := errors.New("stable close diagnostic")
	target := reclaimTargetFunc(func(_ context.Context, claim ReclaimClaim) ReclaimResult {
		close(entered)
		<-closeReturned
		return ReclaimCompleted(claim, diagnostic)
	})
	victim, victimSession := registerTestStore(t, owner, "victim", shareLimits, target)
	requester, requesterSession := registerTestStore(t, owner, "requester", shareLimits, reclaimTargetFunc(decliningTarget))
	victimCharges := commitAdmission(t, victim, victimSession, AdmissionNewRevision, "victim-revision")
	victimStable := releaseTransientCharges(t, victimCharges)
	if err := victim.PublishIdle(IdleCandidate{
		Token: "blocking-close", RevisionID: "victim-revision", RecoveryUntil: testRecoveryEpoch.Add(3 * time.Minute),
		LifecycleGeneration: 1, StableHandle: victimStable,
	}); err != nil {
		t.Fatalf("publish blocking candidate: %v", err)
	}
	type result struct {
		grant AdmissionGrant
		err   error
	}
	admitted := make(chan result, 1)
	go func() {
		grant, err := requester.Admit(context.Background(), AdmissionRequest{
			Kind: AdmissionNewRevision, RevisionID: "requester-revision", Session: requesterSession,
		})
		admitted <- result{grant: grant, err: err}
	}()
	<-entered
	select {
	case premature := <-admitted:
		t.Fatalf("admission completed before terminal Close returned: %v", premature.err)
	default:
	}
	close(closeReturned)
	outcome := <-admitted
	if outcome.err != nil {
		t.Fatalf("admission after terminal Close: %v", outcome.err)
	}
	if !errors.Is(outcome.grant.ReclaimDiagnostic(), diagnostic) {
		t.Fatalf("grant diagnostic = %v, want %v", outcome.grant.ReclaimDiagnostic(), diagnostic)
	}
	requesterCharges, err := outcome.grant.Commit()
	if err != nil {
		t.Fatalf("commit reclaimed admission: %v", err)
	}
	if err := victimStable.Release(); !errors.Is(err, ErrOwnershipResolved) {
		t.Fatalf("victim release after handoff = %v", err)
	}
	if err := requesterCharges.Release(); err != nil {
		t.Fatalf("release requester: %v", err)
	}
	closeRegistrationTree(t, owner, victimSession, requesterSession, victim, requester)
}
