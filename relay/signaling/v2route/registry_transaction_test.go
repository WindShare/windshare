package v2route

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

type commitReply struct {
	outcome CommitOutcome
	err     error
}

type blockingCommitStore struct {
	mu      sync.Mutex
	records map[v2.ShareID]Tombstone
	entered chan Tombstone
	replies chan commitReply
	calls   int
}

func newBlockingCommitStore() *blockingCommitStore {
	return &blockingCommitStore{
		records: make(map[v2.ShareID]Tombstone),
		entered: make(chan Tombstone, 8),
		replies: make(chan commitReply, 8),
	}
}

func (s *blockingCommitStore) Load(context.Context) ([]Tombstone, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Tombstone, 0, len(s.records))
	for _, record := range s.records {
		result = append(result, record)
	}
	return result, nil
}

func (s *blockingCommitStore) Commit(ctx context.Context, record Tombstone) (CommitOutcome, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return CommitNotCommitted, ctx.Err()
	case s.entered <- record:
	}
	var reply commitReply
	select {
	case <-ctx.Done():
		return CommitNotCommitted, ctx.Err()
	case reply = <-s.replies:
	}
	if reply.outcome == CommitCommitted && reply.err == nil {
		s.mu.Lock()
		s.records[record.ShareID] = record
		s.mu.Unlock()
	}
	return reply.outcome, reply.err
}

func (s *blockingCommitStore) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type stopResult struct {
	retirement RouteRetirement
	err        error
}

func startStop(registry *Registry, fixture routeFixture) <-chan stopResult {
	done := make(chan stopResult, 1)
	go func() {
		retirement, err := registry.Stop(context.Background(), fixture.stop, fixture.stopAuth)
		done <- stopResult{retirement: retirement, err: err}
	}()
	return done
}

func publishRoute(t *testing.T, registry *Registry, fixture routeFixture, owner ConnectionRef) {
	t.Helper()
	if err := registry.BeginRegistration(fixture.init, owner); err != nil {
		t.Fatal(err)
	}
	if err := registry.Publish(fixture.init.ShareID, owner, fixture.verified); err != nil {
		t.Fatal(err)
	}
}

func TestPendingStopRetiresDisconnectedOwnerWithoutBlockingOtherRoutes(t *testing.T) {
	now := time.Unix(1_700_001_000, 0)
	store := newBlockingCommitStore()
	registry := newRegistry(t, &now, store, 4)
	stopping := makeFixture(t, 0x21)
	healthy := makeFixture(t, 0x41)
	stoppingSender := routeTestConnection("sender-stopping")
	healthySender := routeTestConnection("sender-healthy")
	stoppingReceiver := routeTestConnection("receiver-stopping")
	healthyReceiver := routeTestConnection("receiver-healthy")
	publishRoute(t, registry, stopping, stoppingSender)
	publishRoute(t, registry, healthy, healthySender)
	stoppingSession, err := registry.Join(stopping.init.ShareID, stoppingReceiver)
	if err != nil || stoppingSession.Status != JoinReady {
		t.Fatalf("stopping join = %+v, %v", stoppingSession, err)
	}
	healthySession, err := registry.Join(healthy.init.ShareID, healthyReceiver)
	if err != nil || healthySession.Status != JoinReady {
		t.Fatalf("healthy join = %+v, %v", healthySession, err)
	}

	stopDone := startStop(registry, stopping)
	if record := <-store.entered; record.StopID != stopping.stop.StopID {
		t.Fatalf("pending STOP record = %+v", record)
	}
	if result, _ := registry.Join(stopping.init.ShareID, routeTestConnection("receiver-fenced")); result.Status != JoinStarting {
		t.Fatalf("pending STOP admitted join: %+v", result)
	}
	if result, joinErr := registry.Join(healthy.init.ShareID, routeTestConnection("receiver-healthy-2")); joinErr != nil || result.Status != JoinReady {
		t.Fatalf("unrelated route blocked by STOP storage: %+v, %v", result, joinErr)
	}
	if retirement, ended := registry.EndSession(healthySession.RelaySessionID, healthyReceiver); !ended || retirement.Receiver != healthyReceiver {
		t.Fatalf("unrelated EndSession blocked or lost authority: %+v, %t", retirement, ended)
	}

	retirement, transitioned := registry.UnexpectedDisconnect(stopping.init.ShareID, stoppingSender)
	if !transitioned || retirement.Owner != stoppingSender || len(retirement.Sessions) != 1 ||
		retirement.Sessions[0].RelaySessionID != stoppingSession.RelaySessionID {
		t.Fatalf("pending STOP disconnect retirement = %+v, %t", retirement, transitioned)
	}
	if resolution, resolveErr := registry.ResolveSession(stoppingSession.RelaySessionID, stoppingSender); resolveErr != nil || resolution.Disposition != SessionRetired {
		t.Fatalf("pending STOP retained disconnected session = %+v, %v", resolution, resolveErr)
	}

	definiteFailure := errors.New("definite durability failure")
	store.replies <- commitReply{outcome: CommitNotCommitted, err: definiteFailure}
	if result := <-stopDone; !errors.Is(result.err, definiteFailure) || len(result.retirement.Sessions) != 0 {
		t.Fatalf("failed STOP result = %+v", result)
	}
	if result, _ := registry.Join(stopping.init.ShareID, routeTestConnection("receiver-after-failure")); result.Status != JoinStarting {
		t.Fatalf("disconnect plus failed STOP did not enter grace: %+v", result)
	}
	resume := stopping.init
	resume.Mode = v2.RegistrationResume
	if err := registry.ValidateResumeCredential(resume, stopping.token); err != nil {
		t.Fatalf("failed STOP emitted a permanent state: %v", err)
	}
	if err := registry.Resume(resume, resumeAuthority(t, stopping, resume), routeTestConnection("sender-resumed"), stopping.token); err != nil {
		t.Fatalf("resume after definite STOP failure: %v", err)
	}
}

func TestPendingStopFencesPublishAbortAndResumeWithoutClaimingDurability(t *testing.T) {
	t.Run("starting route", func(t *testing.T) {
		now := time.Unix(1_700_002_000, 0)
		store := newBlockingCommitStore()
		registry := newRegistry(t, &now, store, 2)
		fixture := makeFixture(t, 0x31)
		sender := routeTestConnection("sender")
		if err := registry.BeginRegistration(fixture.init, sender); err != nil {
			t.Fatal(err)
		}
		stopDone := startStop(registry, fixture)
		<-store.entered
		if err := registry.Publish(fixture.init.ShareID, sender, fixture.verified); !errors.Is(err, ErrStopping) {
			t.Fatalf("Publish during pending STOP = %v", err)
		}
		if registry.AbortRegistration(fixture.init.ShareID, sender) {
			t.Fatal("AbortRegistration deleted a pending STOP route")
		}
		if _, transitioned := registry.UnexpectedDisconnect(fixture.init.ShareID, sender); !transitioned {
			t.Fatal("starting owner disconnect was not recorded")
		}
		store.replies <- commitReply{outcome: CommitNotCommitted, err: ErrCommitFailed}
		if result := <-stopDone; !errors.Is(result.err, ErrCommitFailed) {
			t.Fatalf("definite STOP failure = %v", result.err)
		}
		if result, _ := registry.Join(fixture.init.ShareID, routeTestConnection("receiver")); result.Status != JoinNotFound {
			t.Fatalf("failed starting STOP resurrected an abandoned attempt: %+v", result)
		}
	})

	t.Run("grace route", func(t *testing.T) {
		now := time.Unix(1_700_003_000, 0)
		store := newBlockingCommitStore()
		registry := newRegistry(t, &now, store, 2)
		fixture := makeFixture(t, 0x51)
		sender := routeTestConnection("sender")
		publishRoute(t, registry, fixture, sender)
		if _, transitioned := registry.UnexpectedDisconnect(fixture.init.ShareID, sender); !transitioned {
			t.Fatal("route did not enter grace")
		}
		resume := fixture.init
		resume.Mode = v2.RegistrationResume
		stopDone := startStop(registry, fixture)
		<-store.entered
		if err := registry.ValidateResumeCredential(resume, fixture.token); !errors.Is(err, ErrStopping) {
			t.Fatalf("ValidateResumeCredential during STOP = %v", err)
		}
		if err := registry.Resume(resume, resumeAuthority(t, fixture, resume), routeTestConnection("sender-new"), fixture.token); !errors.Is(err, ErrStopping) {
			t.Fatalf("Resume during STOP = %v", err)
		}
		store.replies <- commitReply{outcome: CommitNotCommitted, err: ErrCommitFailed}
		if result := <-stopDone; !errors.Is(result.err, ErrCommitFailed) {
			t.Fatalf("definite STOP failure = %v", result.err)
		}
		if err := registry.ValidateResumeCredential(resume, fixture.token); err != nil {
			t.Fatalf("definite failure became permanent stopped: %v", err)
		}
	})
}

func TestConcurrentSameStopIDCommitsOnceAndReturnsOneRetirementBatch(t *testing.T) {
	now := time.Unix(1_700_004_000, 0)
	store := newBlockingCommitStore()
	registry := newRegistry(t, &now, store, 2)
	fixture := makeFixture(t, 0x61)
	publishRoute(t, registry, fixture, routeTestConnection("sender"))
	joined, err := registry.Join(fixture.init.ShareID, routeTestConnection("receiver"))
	if err != nil {
		t.Fatal(err)
	}
	first := startStop(registry, fixture)
	<-store.entered
	second := startStop(registry, fixture)
	store.replies <- commitReply{outcome: CommitCommitted}
	firstResult := <-first
	secondResult := <-second
	if firstResult.err != nil || secondResult.err != nil {
		t.Fatalf("same-ID STOP results = %v, %v", firstResult.err, secondResult.err)
	}
	retirementCount := len(firstResult.retirement.Sessions) + len(secondResult.retirement.Sessions)
	if retirementCount != 1 || firstResult.retirement.Sessions[0].RelaySessionID != joined.RelaySessionID {
		t.Fatalf("same-ID retirement batches = %+v, %+v", firstResult.retirement, secondResult.retirement)
	}
	if store.callCount() != 1 {
		t.Fatalf("same-ID STOP commit calls = %d", store.callCount())
	}
	select {
	case duplicate := <-store.entered:
		t.Fatalf("same-ID waiter entered storage twice: %+v", duplicate)
	default:
	}
}

func TestUncertainStopFailsClosedUntilSameIDResolves(t *testing.T) {
	now := time.Unix(1_700_005_000, 0)
	store := newBlockingCommitStore()
	registry := newRegistry(t, &now, store, 2)
	fixture := makeFixture(t, 0x71)
	sender := routeTestConnection("sender")
	publishRoute(t, registry, fixture, sender)
	joined, err := registry.Join(fixture.init.ShareID, routeTestConnection("receiver"))
	if err != nil {
		t.Fatal(err)
	}
	first := startStop(registry, fixture)
	<-store.entered
	store.replies <- commitReply{outcome: CommitUnknown, err: errors.New("ambiguous sync")}
	result := <-first
	if !errors.Is(result.err, ErrCommitUncertain) || result.retirement.Owner != sender ||
		len(result.retirement.Sessions) != 1 || result.retirement.Sessions[0].RelaySessionID != joined.RelaySessionID {
		t.Fatalf("uncertain STOP result = %+v", result)
	}
	if joinedResult, _ := registry.Join(fixture.init.ShareID, routeTestConnection("receiver-new")); joinedResult.Status != JoinStopped {
		t.Fatalf("uncertain STOP admitted join: %+v", joinedResult)
	}
	replacement := routeTestConnection("replacement")
	if err := registry.BeginRegistration(fixture.init, replacement); !errors.Is(err, ErrStopped) {
		t.Fatalf("uncertain STOP admitted registration: %v", err)
	}
	resume := fixture.init
	resume.Mode = v2.RegistrationResume
	if err := registry.ValidateResumeCredential(resume, fixture.token); !errors.Is(err, ErrStopped) {
		t.Fatalf("uncertain STOP resume precheck = %v", err)
	}
	if err := registry.Resume(resume, resumeAuthority(t, fixture, resume), replacement, fixture.token); !errors.Is(err, ErrStopped) {
		t.Fatalf("uncertain STOP Resume = %v", err)
	}
	different := fixture.stop
	different.StopID[0] ^= 1
	if _, err := registry.Stop(context.Background(), different, stopAuthority(t, fixture, different)); !errors.Is(err, ErrStopped) {
		t.Fatalf("uncertain STOP accepted different ID: %v", err)
	}

	retry := startStop(registry, fixture)
	<-store.entered
	store.replies <- commitReply{outcome: CommitCommitted}
	if retryResult := <-retry; retryResult.err != nil || len(retryResult.retirement.Sessions) != 0 {
		t.Fatalf("same-ID uncertain retry = %+v", retryResult)
	}
	if _, err := registry.Stop(context.Background(), fixture.stop, fixture.stopAuth); err != nil {
		t.Fatalf("resolved STOP is not idempotent: %v", err)
	}
	if store.callCount() != 2 {
		t.Fatalf("uncertain retry commit calls = %d", store.callCount())
	}
}

type fixedOutcomeStore struct {
	outcome CommitOutcome
	err     error
}

func (*fixedOutcomeStore) Load(context.Context) ([]Tombstone, error) { return nil, nil }

func (s *fixedOutcomeStore) Commit(context.Context, Tombstone) (CommitOutcome, error) {
	return s.outcome, s.err
}

func TestStopRejectsInvalidStoreOutcomeCombinations(t *testing.T) {
	tests := []struct {
		name          string
		outcome       CommitOutcome
		err           error
		wantUncertain bool
		wantError     error
	}{
		{name: "not committed without error", outcome: CommitNotCommitted, wantError: ErrCommitFailed},
		{name: "committed with error", outcome: CommitCommitted, err: errors.New("contradictory commit"), wantUncertain: true, wantError: ErrCommitUncertain},
		{name: "unknown without error", outcome: CommitUnknown, wantUncertain: true, wantError: ErrCommitUncertain},
		{name: "invalid outcome", outcome: CommitOutcome(99), wantUncertain: true, wantError: ErrCommitUncertain},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Unix(1_700_006_000+int64(index), 0)
			store := &fixedOutcomeStore{outcome: test.outcome, err: test.err}
			registry := newRegistry(t, &now, store, 1)
			fixture := makeFixture(t, byte(0x81+index))
			publishRoute(t, registry, fixture, routeTestConnection("sender"))
			_, err := registry.Stop(context.Background(), fixture.stop, fixture.stopAuth)
			if !errors.Is(err, test.wantError) {
				t.Fatalf("STOP error = %v, want %v", err, test.wantError)
			}
			joined, joinErr := registry.Join(fixture.init.ShareID, routeTestConnection("receiver"))
			if joinErr != nil {
				t.Fatal(joinErr)
			}
			wantStatus := JoinReady
			if test.wantUncertain {
				wantStatus = JoinStopped
			}
			if joined.Status != wantStatus {
				t.Fatalf("route status = %d, want %d", joined.Status, wantStatus)
			}
		})
	}
}
