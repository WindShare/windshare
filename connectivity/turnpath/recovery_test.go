package turnpath

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/core/session/turncontrol"
)

type fixedRecoveryAccounts struct{ accountFixture }

func (f *fixedRecoveryAccounts) Acquire(context.Context, string, Subject) (*Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail {
		return nil, errors.New("service unavailable")
	}
	credential, _ := iceconfig.Fixed("user", "password")
	relay, _ := iceconfig.NewRelay(f.Profiles()[0].Identity, []string{"turn:localhost"}, credential)
	return NewLease(LeaseConfig{Relay: relay, Release: func() { f.mu.Lock(); f.closed++; f.mu.Unlock() }}), nil
}

func recoverySender(t *testing.T) (*Sender, *fixedRecoveryAccounts, *time.Time) {
	t.Helper()
	now := time.Unix(1700000000, 0)
	accounts := &fixedRecoveryAccounts{now: func() time.Time { return now }}
	s := NewSender(SenderConfig{Accounts: accounts, SessionID: [16]byte{1}, Now: accounts.now, Send: func(context.Context, []byte) error { return nil }})
	t.Cleanup(s.close)
	s.apply(t.Context(), turncontrol.Demand{Authority: fixtureHeader(s, 1), HoldFor: turncontrol.MaximumDemandLifetime})
	issueNext(t, s)
	pending := s.services[0].preparation.pending()
	s.Admit(t.Context(), pending.grant.PathID, turncontrol.Lane{ID: 1, Epoch: 1}, make(chan struct{}))
	s.apply(t.Context(), turncontrol.Ready{Authority: pending.grant.Authority, PathID: pending.grant.PathID, Lane: turncontrol.Lane{ID: 1, Epoch: 1}})
	return s, accounts, &now
}

func TestFixedCredentialsRecoverWithoutRenewalOrBreakingCurrentPath(t *testing.T) {
	s, accounts, _ := recoverySender(t)
	old := s.services[0].current
	if old.lease.Renewable() {
		t.Fatal("fixture unexpectedly renewable")
	}
	s.Recover(old.grant.PathID, turncontrol.AllocationStateMismatch)
	s.maintain(t.Context())
	issueNext(t, s)
	next := s.services[0].preparation.pending()
	if next == nil || next.grant.Replaces != old.grant.PathID || s.services[0].current != old {
		t.Fatal("fixed recovery did not preserve predecessor")
	}
	s.apply(t.Context(), turncontrol.Ready{Authority: next.grant.Authority, PathID: next.grant.PathID, Lane: turncontrol.Lane{ID: 1, Epoch: 1}})
	if _, closed := accounts.counts(); closed != 0 {
		t.Fatal("receiver-only readiness retired old path")
	}
	s.Admit(t.Context(), next.grant.PathID, turncontrol.Lane{ID: 1, Epoch: 1}, make(chan struct{}))
	s.maintain(t.Context())
	if s.services[0].current != next {
		t.Fatal("authenticated replacement not promoted")
	}
	if calls, closed := accounts.counts(); calls != 2 || closed != 1 {
		t.Fatalf("leases: %d/%d", calls, closed)
	}
	s.Recover(old.grant.PathID, turncontrol.AllocationStateMismatch)
	s.maintain(t.Context())
	if calls, _ := accounts.counts(); calls != 2 {
		t.Fatal("late old-path fault restarted recovery")
	}
}

func TestFailedRecoveryIsBoundedAndUntrustedPathExpires(t *testing.T) {
	s, accounts, now := recoverySender(t)
	old := s.services[0].current
	accounts.mu.Lock()
	accounts.fail = true
	accounts.mu.Unlock()
	s.Recover(old.grant.PathID, turncontrol.AllocationStateMismatch)
	s.maintain(t.Context())
	issueNext(t, s)
	if s.services[0].current != old {
		t.Fatal("first failure interrupted current path")
	}
	s.maintain(t.Context())
	if calls, _ := accounts.counts(); calls != 2 {
		t.Fatal("recovery ignored cooldown")
	}
	*now = now.Add(ServiceCooldown)
	s.maintain(t.Context())
	issueNext(t, s)
	*now = now.Add(ServiceCooldown)
	s.maintain(t.Context())
	if calls, _ := accounts.counts(); calls != 3 {
		t.Fatal("recovery exceeded per-demand attempt budget")
	}
	*now = now.Add(ServiceCooldown)
	s.maintain(t.Context())
	if s.services[0].current != nil {
		t.Fatal("uncertain allocation retained beyond recovery deadline")
	}
	*now = now.Add(ServiceCooldown)
	s.apply(t.Context(), turncontrol.Demand{Authority: fixtureHeader(s, 2), HoldFor: time.Minute})
	// The previous demand ends exactly here: a new demand epoch may try again.
	issueNext(t, s)
	if calls, _ := accounts.counts(); calls != 4 {
		t.Fatal("new demand could not re-evaluate service")
	}
}

func TestRecoveryRequiresGrantAuthorityAndDemand(t *testing.T) {
	s, accounts, _ := recoverySender(t)
	old := s.services[0].current
	stale := old.grant.Authority
	stale.Sequence++
	s.apply(t.Context(), turncontrol.Recover{Authority: stale, PathID: old.grant.PathID, Reason: turncontrol.ChannelUnavailable})
	s.maintain(t.Context())
	if calls, _ := accounts.counts(); calls != 1 {
		t.Fatal("stale recovery started work")
	}
	s.apply(t.Context(), turncontrol.Demand{Authority: fixtureHeader(s, 2), HoldFor: 0})
	s.Recover(old.grant.PathID, turncontrol.ChannelUnavailable)
	s.maintain(t.Context())
	if calls, _ := accounts.counts(); calls != 1 {
		t.Fatal("idle path started recovery")
	}
}

func TestReceiverRecoveryRetainsPathAndRetriesFailedControl(t *testing.T) {
	var sent []turncontrol.Recover
	fail := true
	r := NewReceiver(ReceiverConfig{SessionID: [16]byte{1}, Send: func(_ context.Context, body []byte) error {
		message, err := turncontrol.Decode(body)
		if err != nil {
			return err
		}
		if recover, ok := message.(turncontrol.Recover); ok {
			if fail {
				return errors.New("control unavailable")
			}
			sent = append(sent, recover)
		}
		return nil
	}})
	grant := turncontrol.Grant{Authority: turncontrol.Header{SessionID: [16]byte{1}, OwnerPathID: [16]byte{2}, Sequence: 1}, PathID: [16]byte{3}}
	path := &receiverFixturePath{ready: make(chan struct{}), done: make(chan struct{})}
	r.paths[grant.PathID] = &receiverPath{grant: grant, path: path}
	r.SetDemand(true)
	r.Recover(grant.PathID, turncontrol.ChannelUnavailable)
	r.requestRecoveries(t.Context())
	if len(r.recoveries) != 1 {
		t.Fatal("failed recovery control was lost")
	}
	fail = false
	r.requestRecoveries(t.Context())
	if len(sent) != 1 || len(r.paths) != 1 {
		t.Fatal("recovery retired the still-live path")
	}
	select {
	case <-path.done:
		t.Fatal("recovery closed the old lane")
	default:
	}
}

func TestEfficiencyRecoveryDoesNotExpireUsefulIndications(t *testing.T) {
	s, accounts, now := recoverySender(t)
	old := s.services[0].current
	accounts.mu.Lock()
	accounts.fail = true
	accounts.mu.Unlock()
	s.Recover(old.grant.PathID, turncontrol.ChannelUnavailable)
	s.services[0].recoveryAttempts = MaximumRecoveryReplacements
	*now = now.Add(ReplacementBudget)
	s.maintain(t.Context())
	if s.services[0].current != old {
		t.Fatal("efficiency recovery closed a usable fallback")
	}
	if _, _, err := s.Lookup(old.grant.PathID); err == nil {
		t.Fatal("damaged grant allowed an unrelated new allocation")
	}
	// Stronger evidence must still bound the lifetime of an uncertain mapping.
	s.Recover(old.grant.PathID, turncontrol.AllocationStateMismatch)
	*now = now.Add(ReplacementBudget)
	s.demandUntil = now.Add(time.Minute)
	s.maintain(t.Context())
	if s.services[0].current != nil {
		t.Fatal("mismatch escalation left an uncertain mapping alive")
	}
}

func TestRecoveryBudgetResetsOnNetworkChangeOrStableProgress(t *testing.T) {
	s, _, now := recoverySender(t)
	network := uint64(1)
	s.config.NetworkGeneration = func() uint64 { return network }
	s.maintain(t.Context())
	service := s.services[0]
	service.recoveryAttempts = MaximumRecoveryReplacements
	network++
	s.maintain(t.Context())
	if service.recoveryAttempts != 0 {
		t.Fatal("new network inherited an exhausted recovery budget")
	}
	service.recoveryAttempts = MaximumRecoveryReplacements
	old := service.current
	report := turncontrol.Report{Authority: old.grant.Authority, PathID: old.grant.PathID, UsefulBytes: 1}
	s.apply(t.Context(), report)
	if service.recoveryAttempts == 0 {
		t.Fatal("one immediate packet reset a potentially flapping service")
	}
	*now = now.Add(ServiceCooldown)
	report.UsefulBytes++
	s.apply(t.Context(), report)
	if service.recoveryAttempts != 0 {
		t.Fatal("stable useful traffic did not restore recovery capacity")
	}
}

func TestFaultyReplacementCannotRetireWorkingPredecessor(t *testing.T) {
	s, _, _ := recoverySender(t)
	old := s.services[0].current
	s.Recover(old.grant.PathID, turncontrol.ChannelUnavailable)
	s.maintain(t.Context())
	issueNext(t, s)
	next := s.services[0].preparation.pending()
	s.Recover(next.grant.PathID, turncontrol.AllocationStateMismatch)
	s.Admit(t.Context(), next.grant.PathID, turncontrol.Lane{ID: 1, Epoch: 1}, make(chan struct{}))
	s.apply(t.Context(), turncontrol.Ready{Authority: next.grant.Authority, PathID: next.grant.PathID, Lane: turncontrol.Lane{ID: 1, Epoch: 1}})
	if s.services[0].current != old {
		t.Fatal("authenticated but mismatched replacement retired predecessor")
	}
}

func TestRecoverySurvivesControlMailboxCongestion(t *testing.T) {
	s, _, _ := recoverySender(t)
	old := s.services[0].current
	for range controlCommandCapacity {
		if err := s.controls.put(t.Context(), turncontrol.Demand{}); err != nil {
			t.Fatal(err)
		}
	}
	body, err := turncontrol.Encode(turncontrol.Recover{Authority: old.grant.Authority, PathID: old.grant.PathID,
		Reason: turncontrol.AllocationStateMismatch})
	if err != nil || s.Handle(t.Context(), body) != nil {
		t.Fatal("valid recovery control was rejected")
	}
	if !s.needsRecovery(old) {
		t.Fatal("control congestion silently lost recovery")
	}
}
