package turnpath

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/connectionpath"
	"github.com/windshare/windshare/core/session/turncontrol"
)

type canceledFirstAccounts struct {
	accountFixture
	started  chan struct{}
	requests atomic.Int32
}

func (f *canceledFirstAccounts) Acquire(ctx context.Context, _ string, subject Subject) (*Lease, error) {
	if f.requests.Add(1) == 1 {
		close(f.started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.issue(ctx, subject)
}

func TestCanceledIssuerImmediatelyServesNextDemandWithoutFailureObservation(t *testing.T) {
	now := time.Unix(1700000000, 0)
	accounts := &canceledFirstAccounts{accountFixture: accountFixture{now: func() time.Time { return now }}, started: make(chan struct{})}
	var messages []turncontrol.Message
	var events []Event
	s := NewSender(SenderConfig{Accounts: accounts, SessionID: [16]byte{1}, Now: accounts.now,
		Send: func(_ context.Context, body []byte) error {
			message, err := turncontrol.Decode(body)
			messages = append(messages, message)
			return err
		},
		Observe: func(event Event) { events = append(events, event) }})
	t.Cleanup(s.close)
	s.apply(t.Context(), turncontrol.Demand{Authority: fixtureHeader(s, 1), HoldFor: time.Minute})
	<-accounts.started
	s.apply(t.Context(), turncontrol.Demand{Authority: fixtureHeader(s, 2), HoldFor: 0})
	s.apply(t.Context(), turncontrol.Demand{Authority: fixtureHeader(s, 3), HoldFor: time.Minute})
	issueNext(t, s)
	if !s.services[0].retryAt.IsZero() {
		t.Fatal("caller cancellation imposed a provider cooldown")
	}
	issueNext(t, s)
	if accounts.requests.Load() != 2 || s.services[0].pending == nil {
		t.Fatal("new demand did not start immediately")
	}
	for _, message := range messages {
		if _, unavailable := message.(turncontrol.Unavailable); unavailable {
			t.Fatal("cancellation reported a service outage")
		}
	}
	discarded := false
	for _, event := range events {
		if event.Milestone == connectionpath.MilestoneIssuanceFailed {
			t.Fatal("cancellation reported as provider failure")
		}
		discarded = discarded || event.Milestone == connectionpath.MilestoneIssuanceDiscarded && event.Reason == "demand_superseded"
	}
	if !discarded {
		t.Fatal("missing discarded-request diagnostic")
	}
}

type providerCanceledAccounts struct{ accountFixture }

func (*providerCanceledAccounts) Acquire(context.Context, string, Subject) (*Lease, error) {
	return nil, context.Canceled
}

func TestProviderCancellationWithoutCallerCancellationStillBacksOff(t *testing.T) {
	now := time.Unix(1700000000, 0)
	accounts := &providerCanceledAccounts{accountFixture{now: func() time.Time { return now }}}
	s := NewSender(SenderConfig{Accounts: accounts, SessionID: [16]byte{1}, Now: accounts.now, Send: func(context.Context, []byte) error { return nil }})
	t.Cleanup(s.close)
	s.apply(t.Context(), turncontrol.Demand{Authority: fixtureHeader(s, 1), HoldFor: time.Minute})
	issueNext(t, s)
	if s.services[0].retryAt.Sub(now) != ServiceCooldown || s.services[0].issuance != nil {
		t.Fatal("provider-local cancellation caused an immediate retry loop")
	}
}

func TestProviderFailureRetainsCooldownAndIdleDiscardDoesNotStartWork(t *testing.T) {
	now := time.Unix(1700000000, 0)
	accounts := &accountFixture{now: func() time.Time { return now }, fail: true}
	s := NewSender(SenderConfig{Accounts: accounts, SessionID: [16]byte{1}, Now: accounts.now, Send: func(context.Context, []byte) error { return nil }})
	t.Cleanup(s.close)
	s.apply(t.Context(), turncontrol.Demand{Authority: fixtureHeader(s, 1), HoldFor: time.Minute})
	issueNext(t, s)
	if s.services[0].retryAt.Sub(now) != ServiceCooldown {
		t.Fatal("real provider failure lost backoff")
	}
	s.maintain(t.Context())
	if calls, _ := accounts.counts(); calls != 1 {
		t.Fatal("provider retried during cooldown")
	}
	accounts.mu.Lock()
	accounts.fail = false
	accounts.mu.Unlock()
	now = now.Add(ServiceCooldown)
	s.maintain(t.Context())
	s.apply(t.Context(), turncontrol.Demand{Authority: fixtureHeader(s, 2), HoldFor: 0})
	issueNext(t, s)
	if calls, _ := accounts.counts(); calls != 2 {
		t.Fatal("discarded idle result restarted acquisition")
	}
	if s.services[0].retryAt.After(now) {
		t.Fatal("idle result extended failure backoff")
	}
}
