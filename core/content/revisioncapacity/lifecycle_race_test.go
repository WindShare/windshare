package revisioncapacity

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

func TestCancellationAfterClaimBeforeTargetPreservesPublishedCandidate(t *testing.T) {
	processLimits := CapacityLimits{StableHandles: 1, ActiveLeases: 8}
	shareLimits := CapacityLimits{StableHandles: 4, ActiveLeases: 8}
	var cancel context.CancelFunc
	owner, err := NewProcessOwner(ProcessConfig{
		Limits: processLimits, RetryAfter: DefaultCapacityRetryAfter,
		Tracer: TracerFunc(func(event TraceEvent) {
			if event.Stage() == TraceReclaimClaimed && cancel != nil {
				cancel()
			}
		}),
	})
	if err != nil {
		t.Fatalf("new process owner: %v", err)
	}
	targetCalled := false
	victim, victimSession := registerTestStore(t, owner, "victim", shareLimits, reclaimTargetFunc(func(_ context.Context, claim ReclaimClaim) ReclaimResult {
		targetCalled = true
		return ReclaimCompleted(claim, nil)
	}))
	requester, requesterSession := registerTestStore(t, owner, "requester", shareLimits, reclaimTargetFunc(decliningTarget))
	victimStable := publishSingleIdle(t, victim, victimSession, "cancel-before-target")
	ctx, cancelAdmission := context.WithCancel(context.Background())
	cancel = cancelAdmission
	_, err = requester.Admit(ctx, AdmissionRequest{
		Kind: AdmissionNewRevision, RevisionID: "requester-revision", Session: requesterSession,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled admission error = %v", err)
	}
	if targetCalled {
		t.Fatal("reclaim target ran after claim-time cancellation")
	}
	snapshot := victim.Snapshot()
	if snapshot.Process().ReclaimableStableHandles() != 1 || snapshot.Process().Used().StableHandles != 1 {
		t.Fatalf("abandoned claim snapshot used=%+v reclaimable=%d", snapshot.Process().Used(), snapshot.Process().ReclaimableStableHandles())
	}
	if err := victimStable.Release(); err != nil {
		t.Fatalf("release abandoned candidate: %v", err)
	}
	closeRegistrationTree(t, owner, victimSession, requesterSession, victim, requester)
}

func TestCancellationBeforeDetachRetainsVictimOwnership(t *testing.T) {
	processLimits := CapacityLimits{StableHandles: 1, ActiveLeases: 8}
	shareLimits := CapacityLimits{StableHandles: 4, ActiveLeases: 8}
	owner := newTestOwner(t, processLimits)
	entered := make(chan struct{})
	target := reclaimTargetFunc(func(ctx context.Context, claim ReclaimClaim) ReclaimResult {
		close(entered)
		<-ctx.Done()
		return ReclaimDeclined(claim)
	})
	victim, victimSession := registerTestStore(t, owner, "victim", shareLimits, target)
	requester, requesterSession := registerTestStore(t, owner, "requester", shareLimits, reclaimTargetFunc(decliningTarget))
	victimStable := publishSingleIdle(t, victim, victimSession, "cancel-before-detach")

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := requester.Admit(ctx, AdmissionRequest{
			Kind: AdmissionNewRevision, RevisionID: "requester-revision", Session: requesterSession,
		})
		result <- err
	}()
	<-entered
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled admission error = %v", err)
	}
	snapshot := victim.Snapshot()
	if got := snapshot.Process().Used(); got != (CapacityUsage{StableHandles: 1}) {
		t.Fatalf("process usage after pre-detach cancellation = %+v", got)
	}
	if snapshot.Process().ReclaimableStableHandles() != 0 {
		t.Fatalf("declined candidate remained published after its generation was rejected")
	}
	if err := victimStable.Release(); err != nil {
		t.Fatalf("release retained victim: %v", err)
	}
	closeRegistrationTree(t, owner, victimSession, requesterSession, victim, requester)
}

func TestCancellationDuringCloseReleasesTerminalVictimWithoutTransfer(t *testing.T) {
	processLimits := CapacityLimits{StableHandles: 1, ActiveLeases: 8}
	shareLimits := CapacityLimits{StableHandles: 4, ActiveLeases: 8}
	owner := newTestOwner(t, processLimits)
	entered := make(chan struct{})
	finishClose := make(chan struct{})
	target := reclaimTargetFunc(func(_ context.Context, claim ReclaimClaim) ReclaimResult {
		close(entered)
		<-finishClose
		return ReclaimCompleted(claim, nil)
	})
	victim, victimSession := registerTestStore(t, owner, "victim", shareLimits, target)
	requester, requesterSession := registerTestStore(t, owner, "requester", shareLimits, reclaimTargetFunc(decliningTarget))
	victimStable := publishSingleIdle(t, victim, victimSession, "cancel-during-close")

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := requester.Admit(ctx, AdmissionRequest{
			Kind: AdmissionNewRevision, RevisionID: "requester-revision", Session: requesterSession,
		})
		result <- err
	}()
	<-entered
	cancel()
	close(finishClose)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled admission error = %v", err)
	}
	if got := victim.Snapshot().Process().Used(); got != (CapacityUsage{}) {
		t.Fatalf("terminal victim charge after cancellation = %+v, want zero", got)
	}
	if got := requester.Snapshot().Share().Used(); got != (CapacityUsage{}) {
		t.Fatalf("requester charge after cancellation = %+v, want zero", got)
	}
	if err := victimStable.Release(); !errors.Is(err, ErrOwnershipResolved) {
		t.Fatalf("terminal victim release = %v, want ErrOwnershipResolved", err)
	}
	closeRegistrationTree(t, owner, victimSession, requesterSession, victim, requester)
}

func TestStoreCloseWaitsForReclaimCallbackBeforeUnregistering(t *testing.T) {
	processLimits := CapacityLimits{StableHandles: 1, ActiveLeases: 8}
	shareLimits := CapacityLimits{StableHandles: 4, ActiveLeases: 8}
	owner := newTestOwner(t, processLimits)
	entered := make(chan struct{})
	finishClose := make(chan struct{})
	target := reclaimTargetFunc(func(_ context.Context, claim ReclaimClaim) ReclaimResult {
		close(entered)
		<-finishClose
		return ReclaimCompleted(claim, nil)
	})
	victim, victimSession := registerTestStore(t, owner, "victim", shareLimits, target)
	requester, requesterSession := registerTestStore(t, owner, "requester", shareLimits, reclaimTargetFunc(decliningTarget))
	victimStable := publishSingleIdle(t, victim, victimSession, "close-race")
	if err := victimSession.Close(); err != nil {
		t.Fatalf("close idle victim session: %v", err)
	}

	admissionResult := make(chan AdmissionGrant, 1)
	admissionErr := make(chan error, 1)
	go func() {
		grant, err := requester.Admit(context.Background(), AdmissionRequest{
			Kind: AdmissionNewRevision, RevisionID: "requester-revision", Session: requesterSession,
		})
		admissionResult <- grant
		admissionErr <- err
	}()
	<-entered
	storeCloseResult := make(chan error, 1)
	go func() { storeCloseResult <- victim.Close() }()
	waitForStoreClosing(t, victim)
	select {
	case err := <-storeCloseResult:
		t.Fatalf("store unregistered before reclaim callback completed: %v", err)
	default:
	}
	close(finishClose)
	if err := <-admissionErr; err != nil {
		t.Fatalf("reclaim admission: %v", err)
	}
	grant := <-admissionResult
	if err := <-storeCloseResult; err != nil {
		t.Fatalf("victim close after callback: %v", err)
	}
	charges, err := grant.Commit()
	if err != nil {
		t.Fatalf("commit requester: %v", err)
	}
	if err := victimStable.Release(); !errors.Is(err, ErrOwnershipResolved) {
		t.Fatalf("victim release after close race = %v", err)
	}
	if err := charges.Release(); err != nil {
		t.Fatalf("release requester: %v", err)
	}
	closeRegistrationTree(t, owner, requesterSession, requester)
}

func TestOwnershipContractFailureQuarantinesWithoutGrant(t *testing.T) {
	tests := []struct {
		name   string
		target ReclaimTarget
	}{
		{name: "invalid result", target: reclaimTargetFunc(func(context.Context, ReclaimClaim) ReclaimResult {
			return ReclaimResult{}
		})},
		{name: "panic", target: reclaimTargetFunc(func(context.Context, ReclaimClaim) ReclaimResult {
			panic("close ownership unknown")
		})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			processLimits := CapacityLimits{StableHandles: 1, ActiveLeases: 8}
			shareLimits := CapacityLimits{StableHandles: 4, ActiveLeases: 8}
			owner := newTestOwner(t, processLimits)
			victim, victimSession := registerTestStore(t, owner, "victim", shareLimits, test.target)
			requester, requesterSession := registerTestStore(t, owner, "requester", shareLimits, reclaimTargetFunc(decliningTarget))
			victimStable := publishSingleIdle(t, victim, victimSession, "uncertain")

			_, err := requester.Admit(context.Background(), AdmissionRequest{
				Kind: AdmissionNewRevision, RevisionID: "requester-revision", Session: requesterSession,
			})
			var ownershipErr *ReclaimOwnershipError
			if !errors.As(err, &ownershipErr) {
				t.Fatalf("admission error = %v, want ReclaimOwnershipError", err)
			}
			if ownershipErr.DecisionID() == "" || ownershipErr.ClaimID() == "" || ownershipErr.CandidateToken() != "uncertain" {
				t.Fatalf("ownership error lacks correlation: %#v", ownershipErr)
			}
			victimSnapshot := victim.Snapshot()
			if got := victimSnapshot.Process().Used(); got != (CapacityUsage{StableHandles: 1}) {
				t.Fatalf("quarantined process usage = %+v", got)
			}
			if victimSnapshot.Process().QuarantinedStableHandles() != 1 || victimSnapshot.Share().QuarantinedStableHandles() != 1 {
				t.Fatalf("quarantine counts process=%d share=%d", victimSnapshot.Process().QuarantinedStableHandles(), victimSnapshot.Share().QuarantinedStableHandles())
			}
			if got := requester.Snapshot().Share().Used(); got != (CapacityUsage{}) {
				t.Fatalf("failed claimant usage = %+v, want zero", got)
			}
			if err := victimStable.Release(); !errors.Is(err, ErrOwnershipQuarantined) {
				t.Fatalf("quarantined release = %v, want ErrOwnershipQuarantined", err)
			}
			closeRegistrationTree(t, owner, victimSession, requesterSession, victim, requester)
		})
	}
}

func TestOwnerCloseReportsLiveRegistrationAndWinsRegisterRaceAtomically(t *testing.T) {
	limits := CapacityLimits{StableHandles: 4, ActiveLeases: 4}
	owner := newTestOwner(t, limits)
	store, session := registerTestStore(t, owner, "live", limits, reclaimTargetFunc(decliningTarget))
	var live *LiveStoreRegistrationsError
	if err := owner.Close(); !errors.As(err, &live) || live.Count() != 1 {
		t.Fatalf("owner close with registration = %v", err)
	}
	closeRegistrationTree(t, owner, session, store)

	for range 32 {
		tracingOwner := newTestOwner(t, limits)
		start := make(chan struct{})
		registered := make(chan *StoreRegistration, 1)
		registerErr := make(chan error, 1)
		closeErr := make(chan error, 1)
		go func() {
			<-start
			registration, err := tracingOwner.Coordinator().RegisterStore(StoreConfig{
				StoreID: "racing", ShareID: "racing-share", Limits: limits,
			}, reclaimTargetFunc(decliningTarget))
			registered <- registration
			registerErr <- err
		}()
		go func() {
			<-start
			closeErr <- tracingOwner.Close()
		}()
		close(start)
		registration, registrationErr := <-registered, <-registerErr
		ownerErr := <-closeErr
		switch {
		case registrationErr == nil:
			var liveStores *LiveStoreRegistrationsError
			if !errors.As(ownerErr, &liveStores) || liveStores.Count() != 1 {
				t.Fatalf("register won but owner close = %v", ownerErr)
			}
			if err := registration.Close(); err != nil {
				t.Fatalf("close racing registration: %v", err)
			}
			if err := tracingOwner.Close(); err != nil {
				t.Fatalf("close owner after racing registration: %v", err)
			}
		case errors.Is(registrationErr, ErrCoordinatorClosed):
			if ownerErr != nil {
				t.Fatalf("owner won race but close = %v", ownerErr)
			}
		default:
			t.Fatalf("unexpected register/close race result: registration=%v owner=%v", registrationErr, ownerErr)
		}
	}
}

func publishSingleIdle(t *testing.T, store *StoreRegistration, session *SessionRegistration, token CandidateToken) StableHandleCharge {
	t.Helper()
	charges := commitAdmission(t, store, session, AdmissionNewRevision, RevisionID(token+"-revision"))
	stable := releaseTransientCharges(t, charges)
	if err := store.PublishIdle(IdleCandidate{
		Token: token, RevisionID: RevisionID(token + "-revision"), RecoveryUntil: testRecoveryEpoch.Add(4 * time.Minute),
		LifecycleGeneration: 1, StableHandle: stable,
	}); err != nil {
		t.Fatalf("publish %q: %v", token, err)
	}
	return stable
}

func waitForStoreClosing(t *testing.T, store *StoreRegistration) {
	t.Helper()
	for range 10_000 {
		store.coordinator.mu.Lock()
		closing := store.state.closing
		store.coordinator.mu.Unlock()
		if closing {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("store close did not enter closing state")
}
