package revisioncapacity

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDefaultAndZeroValueCapacityContracts(t *testing.T) {
	if got, want := DefaultProcessLimits(), (CapacityLimits{
		StableHandles: DefaultProcessStableHandles,
		ActiveLeases:  DefaultProcessActiveLeases,
	}); got != want {
		t.Fatalf("default process limits = %+v, want %+v", got, want)
	}
	if got, want := DefaultShareLimits(), (CapacityLimits{
		StableHandles: DefaultShareStableHandles,
		ActiveLeases:  DefaultShareActiveLeases,
	}); got != want {
		t.Fatalf("default share limits = %+v, want %+v", got, want)
	}
	if got, want := DefaultSessionLimits(), (CapacityLimits{
		StableHandles: DefaultSessionStableHandles,
		ActiveLeases:  DefaultSessionActiveLeases,
	}); got != want {
		t.Fatalf("default session limits = %+v, want %+v", got, want)
	}
	if got, want := DefaultProcessConfig(), (ProcessConfig{
		Limits: DefaultProcessLimits(), RetryAfter: DefaultCapacityRetryAfter,
	}); got != want {
		t.Fatalf("default process config = %+v, want %+v", got, want)
	}

	resources := map[CapacityResource]string{
		CapacityResourceStableHandle: "stable_handle",
		CapacityResourceActiveLease:  "active_lease",
		CapacityResource(255):        "unknown",
	}
	for resource, want := range resources {
		if got := resource.String(); got != want {
			t.Errorf("capacity resource %d = %q, want %q", resource, got, want)
		}
	}
	scopes := map[CapacityScope]string{
		CapacityScopeProcess: "process",
		CapacityScopeShare:   "share",
		CapacityScopeSession: "session",
		CapacityScope(255):   "unknown",
	}
	for scope, want := range scopes {
		if got := scope.String(); got != want {
			t.Errorf("capacity scope %d = %q, want %q", scope, got, want)
		}
	}

	// These handles cross several lifecycle layers, so their zero values must be
	// safe to inspect and reject mutation without a panic.
	var owner *ProcessOwner
	if owner.Coordinator() != nil || owner.Close() != nil {
		t.Fatal("zero process owner was not inert")
	}
	var store StoreRegistration
	zeroSnapshot := store.Snapshot()
	if store.StoreID() != "" || store.ShareID() != "" || zeroSnapshot.Process() != (ScopeSnapshot{}) ||
		zeroSnapshot.Share() != (ScopeSnapshot{}) || zeroSnapshot.Closed() || len(zeroSnapshot.Sessions()) != 0 {
		t.Fatal("zero store registration exposed state")
	}
	if store.WithdrawIdle("missing") || store.Close() != nil {
		t.Fatal("zero store registration was not inert")
	}
	if err := store.WaitForReclaims(); err == nil {
		t.Fatal("zero store reclaim wait was accepted")
	}
	if _, err := store.RegisterSession(SessionConfig{}); err == nil {
		t.Fatal("zero store session registration was accepted")
	}
	if _, err := store.Admit(context.Background(), AdmissionRequest{}); err == nil {
		t.Fatal("zero store admission was accepted")
	}
	if err := store.PublishIdle(IdleCandidate{}); err == nil {
		t.Fatal("zero store idle publication was accepted")
	}

	var session SessionRegistration
	if session.SessionID() != "" || session.Snapshot() != (ScopeSnapshot{}) || session.Close() != nil {
		t.Fatal("zero session registration was not inert")
	}
	var grant AdmissionGrant
	if grant.DecisionID() != "" || grant.ReclaimDiagnostic() != nil {
		t.Fatal("zero admission grant exposed state")
	}
	if _, err := grant.Commit(); err == nil {
		t.Fatal("zero admission grant committed")
	}
	if err := grant.Abort(); err == nil {
		t.Fatal("zero admission grant aborted")
	}
	if err := grant.QuarantineStableHandle(nil); err == nil {
		t.Fatal("zero admission grant quarantined capacity")
	}

	var stable StableHandleCharge
	var lease ActiveLeaseCharge
	var sessionHandle SessionHandleCharge
	if stable.Valid() || lease.Valid() || sessionHandle.Valid() {
		t.Fatal("zero capacity charge reported valid")
	}
	if stable.Release() == nil || stable.Quarantine(nil) == nil || lease.Release() == nil || sessionHandle.Release() == nil {
		t.Fatal("zero capacity charge mutation was accepted")
	}
	if err := (AdmissionCharges{}).Release(); err != nil {
		t.Fatalf("empty aggregate release = %v, want nil", err)
	}

	var claim ReclaimClaim
	if claim.ClaimID() != "" || claim.DecisionID() != "" || claim.CandidateToken() != "" ||
		claim.RevisionID() != "" || !claim.RecoveryUntil().IsZero() || claim.LifecycleGeneration() != 0 {
		t.Fatal("zero reclaim claim exposed identity")
	}
	if diagnostic := ReclaimOwnershipUncertain(claim, nil).Diagnostic(); !errors.Is(diagnostic, ErrInvalidReclaimResult) {
		t.Fatalf("nil uncertain diagnostic = %v, want ErrInvalidReclaimResult", diagnostic)
	}
	if got := (*CapacityBusyError)(nil).Error(); got != "revision capacity is busy" {
		t.Fatalf("nil busy error = %q", got)
	}
	reclaimCause := errors.New("ownership proof failed")
	reclaimErr := &ReclaimOwnershipError{
		decisionID: "decision", claimID: "claim", candidate: "candidate", cause: reclaimCause,
	}
	if !errors.Is(reclaimErr, reclaimCause) || !strings.Contains(reclaimErr.Error(), "candidate") {
		t.Fatalf("reclaim ownership error lost its cause or candidate: %v", reclaimErr)
	}
}

func TestCapacitySnapshotsAndErrorsExposeLifecycleDecisions(t *testing.T) {
	limits := CapacityLimits{StableHandles: 1, ActiveLeases: 1}
	retryAfter := 37 * time.Millisecond
	events := make([]TraceEvent, 0, 4)
	owner, err := NewProcessOwner(ProcessConfig{
		Limits: limits, RetryAfter: retryAfter,
		Tracer: TracerFunc(func(event TraceEvent) { events = append(events, event) }),
	})
	if err != nil {
		t.Fatalf("new owner: %v", err)
	}
	coordinator := owner.Coordinator()
	if _, err := (*Coordinator)(nil).RegisterStore(StoreConfig{}, nil); err == nil {
		t.Fatal("nil coordinator accepted a store")
	}
	if _, err := coordinator.RegisterStore(StoreConfig{}, reclaimTargetFunc(decliningTarget)); err == nil {
		t.Fatal("store without identities was accepted")
	}
	if _, err := coordinator.RegisterStore(StoreConfig{
		StoreID: "invalid-limits", ShareID: "invalid-limits-share",
	}, reclaimTargetFunc(decliningTarget)); err == nil {
		t.Fatal("store with zero limits was accepted")
	}

	store, err := coordinator.RegisterStore(StoreConfig{
		StoreID: "store", ShareID: "share", Limits: limits,
	}, reclaimTargetFunc(decliningTarget))
	if err != nil {
		t.Fatalf("register store: %v", err)
	}
	if store.StoreID() != "store" || store.ShareID() != "share" {
		t.Fatalf("store identity = %q/%q", store.StoreID(), store.ShareID())
	}
	if _, err := store.RegisterSession(SessionConfig{}); err == nil {
		t.Fatal("session without identity was accepted")
	}
	if _, err := store.RegisterSession(SessionConfig{SessionID: "invalid-limits"}); err == nil {
		t.Fatal("session with zero limits was accepted")
	}
	sessionZ, err := store.RegisterSession(SessionConfig{SessionID: "z-session", Limits: limits})
	if err != nil {
		t.Fatalf("register z session: %v", err)
	}
	sessionA, err := store.RegisterSession(SessionConfig{SessionID: "a-session", Limits: limits})
	if err != nil {
		t.Fatalf("register a session: %v", err)
	}
	if sessionA.SessionID() != "a-session" {
		t.Fatalf("session identity = %q", sessionA.SessionID())
	}

	snapshot := store.Snapshot()
	if snapshot.Process().Scope() != CapacityScopeProcess || snapshot.Process().Limits() != limits ||
		snapshot.Share().Scope() != CapacityScopeShare || snapshot.Share().Identity() != "share" || snapshot.Share().Limits() != limits {
		t.Fatalf("capacity snapshot scopes = %+v", snapshot)
	}
	sessions := snapshot.Sessions()
	if len(sessions) != 2 || sessions[0].Identity() != "a-session" || sessions[1].Identity() != "z-session" {
		t.Fatalf("sorted session snapshots = %+v", sessions)
	}
	sessions[0] = ScopeSnapshot{}
	if store.Snapshot().Sessions()[0].Identity() != "a-session" {
		t.Fatal("caller mutation changed the coordinator snapshot")
	}
	if sessionA.Snapshot().Scope() != CapacityScopeSession || sessionA.Snapshot().Identity() != "a-session" {
		t.Fatalf("session snapshot = %+v", sessionA.Snapshot())
	}

	if _, err := store.Admit(nil, AdmissionRequest{}); err == nil {
		t.Fatal("nil admission context was accepted")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Admit(cancelled, AdmissionRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled admission = %v, want context.Canceled", err)
	}
	if _, err := store.Admit(context.Background(), AdmissionRequest{}); err == nil {
		t.Fatal("admission without identities was accepted")
	}

	charges := commitAdmission(t, store, sessionA, AdmissionNewRevision, "resident")
	_, err = store.Admit(context.Background(), AdmissionRequest{
		Kind: AdmissionAdditionalSessionLease, RevisionID: "resident", Session: sessionZ,
	})
	var busy *CapacityBusyError
	if !errors.As(err, &busy) || busy.Resource() != CapacityResourceActiveLease || busy.Scope() != CapacityScopeShare {
		t.Fatalf("share lease boundary = %v, want active_lease/share", err)
	}
	if busy.RetryAfter() != retryAfter || !strings.Contains(busy.Error(), string(busy.DecisionID())) {
		t.Fatalf("busy retry/error = %v/%q", busy.RetryAfter(), busy.Error())
	}
	busySnapshot := busy.Snapshot()
	if busySnapshot.Process().Used() != (CapacityUsage{StableHandles: 1, ActiveLeases: 1}) ||
		busySnapshot.Process().PendingAdmissions() != 0 || busySnapshot.Process().ActiveReclaims() != 0 || busySnapshot.Closed() {
		t.Fatalf("busy process snapshot = %+v", busySnapshot.Process())
	}

	var denied TraceEvent
	for _, event := range events {
		if event.Stage() == TraceAdmissionDenied {
			denied = event
			break
		}
	}
	if denied.DecisionID() != busy.DecisionID() || denied.ClaimID() != "" || denied.StoreID() != "store" ||
		denied.ShareID() != "share" || denied.SessionID() != "z-session" || denied.RevisionID() != "resident" ||
		denied.CandidateToken() != "" || !errors.As(denied.Diagnostic(), &busy) || denied.Snapshot().Share().Identity() != "share" {
		t.Fatalf("denied trace omitted decision context: %+v", denied)
	}

	var liveStores *LiveStoreRegistrationsError
	if err := owner.Close(); !errors.As(err, &liveStores) || liveStores.Count() != 1 || !strings.Contains(liveStores.Error(), "1 live store") {
		t.Fatalf("owner close with live store = %v", err)
	}
	var liveSessions *LiveSessionRegistrationsError
	if err := store.Close(); !errors.As(err, &liveSessions) || liveSessions.StoreID() != "store" || liveSessions.Count() != 2 ||
		!strings.Contains(liveSessions.Error(), "2 live session") {
		t.Fatalf("store close with live sessions = %v", err)
	}
	if _, err := store.Admit(context.Background(), AdmissionRequest{
		Kind: AdmissionAdditionalSessionLease, RevisionID: "resident", Session: sessionZ,
	}); !errors.Is(err, ErrRegistrationClosing) {
		t.Fatalf("admission during store close = %v", err)
	}

	var liveSessionOwnership *LiveCapacityOwnershipError
	if err := sessionA.Close(); !errors.As(err, &liveSessionOwnership) || liveSessionOwnership.Scope() != CapacityScopeSession ||
		liveSessionOwnership.Identity() != "a-session" || liveSessionOwnership.Usage() != (CapacityUsage{StableHandles: 1, ActiveLeases: 1}) ||
		!strings.Contains(liveSessionOwnership.Error(), "a-session") {
		t.Fatalf("session close with capacity = %v", err)
	}
	stable := releaseTransientCharges(t, charges)
	if err := sessionA.Close(); err != nil {
		t.Fatalf("close released a session: %v", err)
	}
	if err := sessionZ.Close(); err != nil {
		t.Fatalf("close z session: %v", err)
	}
	var liveStoreOwnership *LiveCapacityOwnershipError
	if err := store.Close(); !errors.As(err, &liveStoreOwnership) || liveStoreOwnership.Scope() != CapacityScopeShare ||
		liveStoreOwnership.Identity() != "share" || liveStoreOwnership.Usage() != (CapacityUsage{StableHandles: 1}) {
		t.Fatalf("store close with stable ownership = %v", err)
	}
	if err := stable.Release(); err != nil {
		t.Fatalf("release stable ownership: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close released store: %v", err)
	}
	if !store.Snapshot().Closed() {
		t.Fatal("closed store snapshot remained open")
	}
	if sessionA.Close() != nil || store.Close() != nil {
		t.Fatal("registration close was not idempotent")
	}
	if err := owner.Close(); err != nil {
		t.Fatalf("close owner: %v", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatalf("second owner close: %v", err)
	}
	if _, err := store.Admit(context.Background(), AdmissionRequest{
		Kind: AdmissionNewRevision, RevisionID: "late", Session: sessionA,
	}); !errors.Is(err, ErrCoordinatorClosed) {
		t.Fatalf("admission after owner close = %v", err)
	}
}

func TestProcessLeaseBoundaryCarriesConfiguredRetry(t *testing.T) {
	processLimits := CapacityLimits{StableHandles: 4, ActiveLeases: 1}
	storeLimits := CapacityLimits{StableHandles: 4, ActiveLeases: 4}
	retryAfter := 41 * time.Millisecond
	owner, err := NewProcessOwner(ProcessConfig{Limits: processLimits, RetryAfter: retryAfter})
	if err != nil {
		t.Fatalf("new owner: %v", err)
	}
	storeA, sessionA := registerTestStore(t, owner, "a", storeLimits, reclaimTargetFunc(decliningTarget))
	storeB, sessionB := registerTestStore(t, owner, "b", storeLimits, reclaimTargetFunc(decliningTarget))
	grant, err := storeA.Admit(context.Background(), AdmissionRequest{
		Kind: AdmissionAdditionalSessionLease, RevisionID: "resident-a", Session: sessionA,
	})
	if err != nil {
		t.Fatalf("admit process lease: %v", err)
	}
	if err := grant.QuarantineStableHandle(errors.New("not a stable admission")); err == nil {
		t.Fatal("lease-only grant quarantined a stable handle")
	}
	charges, err := grant.Commit()
	if err != nil {
		t.Fatalf("commit process lease: %v", err)
	}

	_, err = storeB.Admit(context.Background(), AdmissionRequest{
		Kind: AdmissionAdditionalSessionLease, RevisionID: "resident-b", Session: sessionB,
	})
	var busy *CapacityBusyError
	if !errors.As(err, &busy) || busy.Resource() != CapacityResourceActiveLease ||
		busy.Scope() != CapacityScopeProcess || busy.RetryAfter() != retryAfter {
		t.Fatalf("process lease boundary = %v", err)
	}
	if got := busy.Snapshot().Process().Used(); got != (CapacityUsage{ActiveLeases: 1}) {
		t.Fatalf("process lease snapshot = %+v", got)
	}
	if err := charges.Release(); err != nil {
		t.Fatalf("release process lease: %v", err)
	}
	closeRegistrationTree(t, owner, sessionA, sessionB, storeA, storeB)
}

func TestWaitForReclaimsJoinsCallbackLifecycle(t *testing.T) {
	processLimits := CapacityLimits{StableHandles: 1, ActiveLeases: 4}
	storeLimits := CapacityLimits{StableHandles: 4, ActiveLeases: 4}
	owner := newTestOwner(t, processLimits)
	entered := make(chan struct{})
	finish := make(chan struct{})
	target := reclaimTargetFunc(func(_ context.Context, claim ReclaimClaim) ReclaimResult {
		if claim.ClaimID() == "" || claim.DecisionID() == "" || claim.CandidateToken() != "joining" ||
			claim.RevisionID() != "joining-revision" || !claim.RecoveryUntil().Equal(testRecoveryEpoch.Add(4*time.Minute)) ||
			claim.LifecycleGeneration() != 1 {
			t.Errorf("reclaim claim omitted lifecycle identity: %+v", claim)
		}
		close(entered)
		<-finish
		return ReclaimCompleted(claim, nil)
	})
	victim, victimSession := registerTestStore(t, owner, "victim", storeLimits, target)
	requester, requesterSession := registerTestStore(t, owner, "requester", storeLimits, reclaimTargetFunc(decliningTarget))
	victimStable := publishSingleIdle(t, victim, victimSession, "joining")

	type admissionResult struct {
		grant AdmissionGrant
		err   error
	}
	admitted := make(chan admissionResult, 1)
	go func() {
		grant, err := requester.Admit(context.Background(), AdmissionRequest{
			Kind: AdmissionNewRevision, RevisionID: "requester", Session: requesterSession,
		})
		admitted <- admissionResult{grant: grant, err: err}
	}()
	<-entered
	active := victim.Snapshot()
	if active.Process().ActiveReclaims() != 1 || active.Share().ActiveReclaims() != 1 {
		t.Fatalf("active reclaim snapshot = process %d share %d", active.Process().ActiveReclaims(), active.Share().ActiveReclaims())
	}
	if err := victimStable.Release(); !errors.Is(err, ErrOwnershipClaimed) {
		t.Fatalf("release during reclaim = %v, want ErrOwnershipClaimed", err)
	}
	if err := victimStable.Quarantine(errors.New("claim still owns resolution")); !errors.Is(err, ErrOwnershipClaimed) {
		t.Fatalf("quarantine during reclaim = %v, want ErrOwnershipClaimed", err)
	}

	waiting := make(chan error, 1)
	go func() { waiting <- victim.WaitForReclaims() }()
	select {
	case err := <-waiting:
		t.Fatalf("reclaim wait returned before callback completion: %v", err)
	default:
	}
	close(finish)
	if err := <-waiting; err != nil {
		t.Fatalf("wait for reclaims: %v", err)
	}
	outcome := <-admitted
	if outcome.err != nil {
		t.Fatalf("reclaiming admission: %v", outcome.err)
	}
	requesterCharges, err := outcome.grant.Commit()
	if err != nil {
		t.Fatalf("commit requester: %v", err)
	}
	if err := victimStable.Release(); !errors.Is(err, ErrOwnershipResolved) {
		t.Fatalf("release transferred victim = %v", err)
	}
	if err := requesterCharges.Release(); err != nil {
		t.Fatalf("release requester: %v", err)
	}
	closeRegistrationTree(t, owner, victimSession, requesterSession, victim, requester)
}

func TestAdmissionCancellationBeforeCapacityDecisionRollsBackReservation(t *testing.T) {
	limits := CapacityLimits{StableHandles: 2, ActiveLeases: 2}
	owner := newTestOwner(t, limits)
	store, session := registerTestStore(t, owner, "cancel", limits, reclaimTargetFunc(decliningTarget))
	ctx, cancel := context.WithCancel(context.Background())
	requirements, err := requirementsFor(AdmissionAdditionalSessionLease)
	if err != nil {
		t.Fatalf("additional lease requirements: %v", err)
	}
	run, err := store.beginAdmission(ctx, AdmissionRequest{
		Kind: AdmissionAdditionalSessionLease, RevisionID: "revision", Session: session,
	}, requirements)
	if err != nil {
		t.Fatalf("begin admission: %v", err)
	}
	if got := store.Snapshot().Process(); got.Used() != (CapacityUsage{ActiveLeases: 1}) || got.PendingAdmissions() != 1 {
		t.Fatalf("provisional admission snapshot = %+v", got)
	}

	cancel()
	if _, err := run.admit(); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled admission = %v, want context.Canceled", err)
	}
	if got := store.Snapshot().Process(); got.Used() != (CapacityUsage{}) || got.PendingAdmissions() != 0 {
		t.Fatalf("rolled-back admission snapshot = %+v", got)
	}
	closeRegistrationTree(t, owner, session, store)
}

func TestIdlePublicationRejectsAmbiguousAndClosingOwnership(t *testing.T) {
	limits := CapacityLimits{StableHandles: 4, ActiveLeases: 4}
	owner := newTestOwner(t, limits)
	victim, victimSession := registerTestStore(t, owner, "victim-publication", limits, reclaimTargetFunc(decliningTarget))
	other, otherSession := registerTestStore(t, owner, "other-publication", limits, reclaimTargetFunc(decliningTarget))
	if err := victim.PublishIdle(IdleCandidate{}); err == nil {
		t.Fatal("incomplete idle candidate was accepted")
	}
	if victim.WithdrawIdle("") {
		t.Fatal("empty idle token was withdrawn")
	}

	victimStable := publishSingleIdle(t, victim, victimSession, "published")
	candidate := IdleCandidate{
		Token: "published", RevisionID: "published-revision", RecoveryUntil: testRecoveryEpoch.Add(4 * time.Minute),
		LifecycleGeneration: 2, StableHandle: victimStable,
	}
	if err := victim.PublishIdle(candidate); err == nil {
		t.Fatal("candidate token reuse hid a lifecycle-generation change")
	}
	candidate.Token = "second-token"
	candidate.LifecycleGeneration = 1
	if err := victim.PublishIdle(candidate); err == nil {
		t.Fatal("one stable handle acquired two idle identities")
	}
	otherStable := publishSingleIdle(t, other, otherSession, "other")

	if err := victimSession.Close(); err != nil {
		t.Fatalf("close victim session: %v", err)
	}
	var live *LiveCapacityOwnershipError
	if err := victim.Close(); !errors.As(err, &live) {
		t.Fatalf("closing published store = %v, want live ownership", err)
	}
	candidate.Token = "late"
	if err := victim.PublishIdle(candidate); !errors.Is(err, ErrRegistrationClosing) {
		t.Fatalf("publication during store close = %v", err)
	}
	if err := victimStable.Release(); err != nil {
		t.Fatalf("release withdrawn victim: %v", err)
	}
	if err := victim.Close(); err != nil {
		t.Fatalf("close released victim: %v", err)
	}

	if !other.WithdrawIdle("other") {
		t.Fatal("other idle candidate was not withdrawable")
	}
	if err := otherStable.Release(); err != nil {
		t.Fatalf("release other stable handle: %v", err)
	}
	closeRegistrationTree(t, owner, otherSession, other)
	if err := victim.PublishIdle(candidate); !errors.Is(err, ErrCoordinatorClosed) {
		t.Fatalf("publication after owner close = %v", err)
	}
}

func TestProvisionalQuarantineUsesOwnershipContractDiagnostic(t *testing.T) {
	limits := CapacityLimits{StableHandles: 1, ActiveLeases: 1}
	var traced TraceEvent
	owner, err := NewProcessOwner(ProcessConfig{
		Limits: limits, RetryAfter: DefaultCapacityRetryAfter,
		Tracer: TracerFunc(func(event TraceEvent) {
			if event.Stage() == TraceOwnershipQuarantined {
				traced = event
			}
		}),
	})
	if err != nil {
		t.Fatalf("new owner: %v", err)
	}
	store, session := registerTestStore(t, owner, "provisional-quarantine", limits, reclaimTargetFunc(decliningTarget))
	grant, err := store.Admit(context.Background(), AdmissionRequest{
		Kind: AdmissionNewRevision, RevisionID: "revision", Session: session,
	})
	if err != nil {
		t.Fatalf("admit revision: %v", err)
	}
	if err := grant.QuarantineStableHandle(nil); err != nil {
		t.Fatalf("quarantine provisional ownership: %v", err)
	}
	if traced.Stage() != TraceOwnershipQuarantined || !errors.Is(traced.Diagnostic(), ErrInvalidReclaimResult) {
		t.Fatalf("quarantine trace = %+v", traced)
	}
	if snapshot := store.Snapshot(); snapshot.Process().Used() != (CapacityUsage{StableHandles: 1}) ||
		snapshot.Process().QuarantinedStableHandles() != 1 {
		t.Fatalf("quarantine snapshot = %+v", snapshot.Process())
	}
	closeRegistrationTree(t, owner, session, store)
}
