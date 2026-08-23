package revisioncapacity

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestAdmissionChargesMatchEveryScopeAndSettleExactlyOnce(t *testing.T) {
	limits := CapacityLimits{StableHandles: 8, ActiveLeases: 8}
	owner := newTestOwner(t, limits)
	store, session := registerTestStore(t, owner, "store", limits, reclaimTargetFunc(decliningTarget))

	grant, err := store.Admit(context.Background(), AdmissionRequest{
		Kind: AdmissionNewRevision, RevisionID: "revision-new", Session: session,
	})
	if err != nil {
		t.Fatalf("admit new revision: %v", err)
	}
	beforeCommit := store.Snapshot()
	if got, want := beforeCommit.Process().Used(), (CapacityUsage{StableHandles: 1, ActiveLeases: 1}); got != want {
		t.Fatalf("provisional process usage = %+v, want %+v", got, want)
	}
	if got, want := beforeCommit.Share().Used(), (CapacityUsage{StableHandles: 1, ActiveLeases: 1}); got != want {
		t.Fatalf("provisional share usage = %+v, want %+v", got, want)
	}
	if got, want := beforeCommit.Sessions()[0].Used(), (CapacityUsage{StableHandles: 1, ActiveLeases: 1}); got != want {
		t.Fatalf("provisional session usage = %+v, want %+v", got, want)
	}
	if beforeCommit.Process().PendingAdmissions() != 1 || beforeCommit.Share().PendingAdmissions() != 1 {
		t.Fatalf("provisional pending counts = process %d share %d", beforeCommit.Process().PendingAdmissions(), beforeCommit.Share().PendingAdmissions())
	}

	newCharges, err := grant.Commit()
	if err != nil {
		t.Fatalf("commit new revision: %v", err)
	}
	if _, err := grant.Commit(); !errors.Is(err, ErrAdmissionGrantSettled) {
		t.Fatalf("second commit error = %v, want ErrAdmissionGrantSettled", err)
	}
	if err := grant.Abort(); !errors.Is(err, ErrAdmissionGrantSettled) {
		t.Fatalf("abort after commit error = %v, want ErrAdmissionGrantSettled", err)
	}

	firstCharges := commitAdmission(t, store, session, AdmissionFirstSessionLease, "revision-resident")
	additionalCharges := commitAdmission(t, store, session, AdmissionAdditionalSessionLease, "revision-resident")
	snapshot := store.Snapshot()
	if got, want := snapshot.Process().Used(), (CapacityUsage{StableHandles: 1, ActiveLeases: 3}); got != want {
		t.Fatalf("process usage = %+v, want %+v", got, want)
	}
	if got, want := snapshot.Share().Used(), (CapacityUsage{StableHandles: 1, ActiveLeases: 3}); got != want {
		t.Fatalf("share usage = %+v, want %+v", got, want)
	}
	if got, want := snapshot.Sessions()[0].Used(), (CapacityUsage{StableHandles: 2, ActiveLeases: 3}); got != want {
		t.Fatalf("session usage = %+v, want %+v", got, want)
	}

	if err := additionalCharges.Release(); err != nil {
		t.Fatalf("release additional lease: %v", err)
	}
	if err := firstCharges.Release(); err != nil {
		t.Fatalf("release first-session lease: %v", err)
	}
	stable, _ := newCharges.StableHandle()
	lease, _ := newCharges.ActiveLease()
	sessionHandle, _ := newCharges.SessionHandle()
	if err := lease.Release(); err != nil {
		t.Fatalf("release new lease: %v", err)
	}
	if err := sessionHandle.Release(); err != nil {
		t.Fatalf("release new session handle: %v", err)
	}
	if err := stable.Release(); err != nil {
		t.Fatalf("release new stable handle: %v", err)
	}
	if err := stable.Release(); !errors.Is(err, ErrOwnershipResolved) {
		t.Fatalf("second stable release = %v, want ErrOwnershipResolved", err)
	}
	if got := store.Snapshot().Process().Used(); got != (CapacityUsage{}) {
		t.Fatalf("final process usage = %+v, want zero", got)
	}
	closeRegistrationTree(t, owner, session, store)
}

func TestCapacityBusyIdentifiesExactNonReclaimableBoundary(t *testing.T) {
	limits := CapacityLimits{StableHandles: 1, ActiveLeases: 1}
	owner := newTestOwner(t, limits)
	store, session := registerTestStore(t, owner, "store", limits, reclaimTargetFunc(decliningTarget))
	charges := commitAdmission(t, store, session, AdmissionNewRevision, "revision-one")

	_, err := store.Admit(context.Background(), AdmissionRequest{
		Kind: AdmissionAdditionalSessionLease, RevisionID: "revision-one", Session: session,
	})
	var busy *CapacityBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("additional lease error = %v, want CapacityBusyError", err)
	}
	if busy.Resource() != CapacityResourceActiveLease || busy.Scope() != CapacityScopeSession {
		t.Fatalf("blocked at %s/%s, want active_lease/session", busy.Resource(), busy.Scope())
	}
	if busy.DecisionID() == "" || busy.RetryAfter() != DefaultCapacityRetryAfter {
		t.Fatalf("busy decision = %q retry = %v", busy.DecisionID(), busy.RetryAfter())
	}
	if got := busy.Snapshot().Process().Used(); got != (CapacityUsage{StableHandles: 1, ActiveLeases: 1}) {
		t.Fatalf("busy process snapshot = %+v", got)
	}

	lease, _ := charges.ActiveLease()
	if err := lease.Release(); err != nil {
		t.Fatalf("release lease: %v", err)
	}
	_, err = store.Admit(context.Background(), AdmissionRequest{
		Kind: AdmissionFirstSessionLease, RevisionID: "revision-two", Session: session,
	})
	busy = nil
	if !errors.As(err, &busy) || busy.Resource() != CapacityResourceStableHandle || busy.Scope() != CapacityScopeSession {
		t.Fatalf("session-handle admission error = %#v, want stable_handle/session busy", err)
	}
	if err := charges.Release(); !errors.Is(err, ErrOwnershipResolved) {
		// Release reports the already released lease while still releasing the
		// remaining tokens; this verifies aggregate cleanup is not short-circuited.
		t.Fatalf("aggregate release error = %v, want resolved lease diagnostic", err)
	}
	if got := store.Snapshot().Process().Used(); got != (CapacityUsage{}) {
		t.Fatalf("usage after aggregate release = %+v", got)
	}
	closeRegistrationTree(t, owner, session, store)
}

func TestConcurrentAdmissionsNeverOverbookAndAbortExactly(t *testing.T) {
	processLimits := CapacityLimits{StableHandles: 1, ActiveLeases: 64}
	shareLimits := CapacityLimits{StableHandles: 64, ActiveLeases: 64}
	owner := newTestOwner(t, processLimits)
	store, session := registerTestStore(t, owner, "store", shareLimits, reclaimTargetFunc(decliningTarget))

	const contenders = 32
	start := make(chan struct{})
	type outcome struct {
		grant AdmissionGrant
		err   error
	}
	outcomes := make(chan outcome, contenders)
	var workers sync.WaitGroup
	workers.Add(contenders)
	for index := range contenders {
		go func(index int) {
			defer workers.Done()
			<-start
			grant, err := store.Admit(context.Background(), AdmissionRequest{
				Kind: AdmissionNewRevision, RevisionID: RevisionID(string(rune('a' + index))), Session: session,
			})
			outcomes <- outcome{grant: grant, err: err}
		}(index)
	}
	close(start)
	workers.Wait()
	close(outcomes)

	granted := make([]AdmissionGrant, 0, 1)
	busyCount := 0
	for outcome := range outcomes {
		if outcome.err == nil {
			granted = append(granted, outcome.grant)
			continue
		}
		var busy *CapacityBusyError
		if !errors.As(outcome.err, &busy) || busy.Resource() != CapacityResourceStableHandle || busy.Scope() != CapacityScopeProcess {
			t.Fatalf("contender error = %v, want process stable-handle busy", outcome.err)
		}
		busyCount++
	}
	if len(granted) != 1 || busyCount != contenders-1 {
		t.Fatalf("granted=%d busy=%d, want 1/%d", len(granted), busyCount, contenders-1)
	}
	if got := store.Snapshot().Process().Used(); got != (CapacityUsage{StableHandles: 1, ActiveLeases: 1}) {
		t.Fatalf("concurrent process usage = %+v", got)
	}
	if err := granted[0].Abort(); err != nil {
		t.Fatalf("abort winning admission: %v", err)
	}
	if got := store.Snapshot().Process().Used(); got != (CapacityUsage{}) {
		t.Fatalf("process usage after abort = %+v", got)
	}
	closeRegistrationTree(t, owner, session, store)
}

func TestUncertainStableOwnershipRemainsUnavailableAfterAdmissionSettlement(t *testing.T) {
	limits := CapacityLimits{StableHandles: 1, ActiveLeases: 4}

	t.Run("provisional acquisition", func(t *testing.T) {
		owner := newTestOwner(t, limits)
		store, session := registerTestStore(t, owner, "provisional", limits, reclaimTargetFunc(decliningTarget))
		grant, err := store.Admit(context.Background(), AdmissionRequest{
			Kind: AdmissionNewRevision, RevisionID: "revision-provisional", Session: session,
		})
		if err != nil {
			t.Fatalf("admit provisional revision: %v", err)
		}
		if err := grant.QuarantineStableHandle(errors.New("stable close ownership is uncertain")); err != nil {
			t.Fatalf("quarantine provisional stable handle: %v", err)
		}
		assertQuarantinedStableCapacity(t, store)
		if _, err := grant.Commit(); !errors.Is(err, ErrAdmissionGrantSettled) {
			t.Fatalf("commit quarantined grant error = %v, want ErrAdmissionGrantSettled", err)
		}
		if err := grant.QuarantineStableHandle(nil); !errors.Is(err, ErrAdmissionGrantSettled) {
			t.Fatalf("second provisional quarantine error = %v, want ErrAdmissionGrantSettled", err)
		}
		assertQuarantineDeniesReplacement(t, store, session, "revision-after-provisional")
		closeRegistrationTree(t, owner, session, store)
	})

	t.Run("committed idle ownership", func(t *testing.T) {
		owner := newTestOwner(t, limits)
		store, session := registerTestStore(t, owner, "committed", limits, reclaimTargetFunc(decliningTarget))
		charges := commitAdmission(t, store, session, AdmissionNewRevision, "revision-committed")
		stable := releaseTransientCharges(t, charges)
		if err := store.PublishIdle(IdleCandidate{
			Token: "idle-before-quarantine", RevisionID: "revision-committed",
			RecoveryUntil: testRecoveryEpoch, LifecycleGeneration: 1, StableHandle: stable,
		}); err != nil {
			t.Fatalf("publish idle ownership: %v", err)
		}
		if err := stable.Quarantine(errors.New("stable ownership contract failed")); err != nil {
			t.Fatalf("quarantine committed stable handle: %v", err)
		}
		assertQuarantinedStableCapacity(t, store)
		if err := stable.Release(); !errors.Is(err, ErrOwnershipQuarantined) {
			t.Fatalf("release quarantined stable handle error = %v, want ErrOwnershipQuarantined", err)
		}
		if err := stable.Quarantine(nil); !errors.Is(err, ErrOwnershipQuarantined) {
			t.Fatalf("second committed quarantine error = %v, want ErrOwnershipQuarantined", err)
		}
		assertQuarantineDeniesReplacement(t, store, session, "revision-after-commit")
		closeRegistrationTree(t, owner, session, store)
	})
}

func assertQuarantinedStableCapacity(t *testing.T, store *StoreRegistration) {
	t.Helper()
	snapshot := store.Snapshot()
	wantUsed := CapacityUsage{StableHandles: 1}
	if got := snapshot.Process().Used(); got != wantUsed {
		t.Fatalf("quarantined process usage = %+v, want %+v", got, wantUsed)
	}
	if got := snapshot.Share().Used(); got != wantUsed {
		t.Fatalf("quarantined share usage = %+v, want %+v", got, wantUsed)
	}
	if snapshot.Process().QuarantinedStableHandles() != 1 ||
		snapshot.Share().QuarantinedStableHandles() != 1 {
		t.Fatalf(
			"quarantined counts = process %d share %d, want 1/1",
			snapshot.Process().QuarantinedStableHandles(),
			snapshot.Share().QuarantinedStableHandles(),
		)
	}
	if snapshot.Process().PendingAdmissions() != 0 || snapshot.Share().PendingAdmissions() != 0 ||
		snapshot.Process().ReclaimableStableHandles() != 0 || snapshot.Share().ReclaimableStableHandles() != 0 {
		t.Fatalf("quarantined ownership remained pending or reclaimable: %+v", snapshot)
	}
	if got := snapshot.Sessions()[0].Used(); got != (CapacityUsage{}) {
		t.Fatalf("quarantined session usage = %+v, want zero", got)
	}
}

func assertQuarantineDeniesReplacement(
	t *testing.T,
	store *StoreRegistration,
	session *SessionRegistration,
	revision RevisionID,
) {
	t.Helper()
	_, err := store.Admit(context.Background(), AdmissionRequest{
		Kind: AdmissionNewRevision, RevisionID: revision, Session: session,
	})
	var busy *CapacityBusyError
	if !errors.As(err, &busy) || busy.Resource() != CapacityResourceStableHandle ||
		busy.Scope() != CapacityScopeShare {
		t.Fatalf("replacement admission error = %#v, want share stable-handle capacity busy", err)
	}
}
