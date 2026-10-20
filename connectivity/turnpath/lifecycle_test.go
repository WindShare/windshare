package turnpath

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/core/session/turncontrol"
)

type accountFixture struct {
	now           func() time.Time
	mu            sync.Mutex
	calls, closed int
	fail          bool
}

func (f *accountFixture) Inventory() Inventory { return Inventory{Revision: 1, Profiles: f.Profiles()} }
func (f *accountFixture) Profiles() []Profile {
	return []Profile{{Identity: iceconfig.Identity{Provider: "test", Account: "account", Profile: "profile"}}}
}
func (f *accountFixture) Acquire(ctx context.Context, _ string, subject Subject) (*Lease, error) {
	return f.issue(ctx, subject)
}
func (f *accountFixture) issue(ctx context.Context, subject Subject) (*Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail {
		return nil, errors.New("issuer unavailable")
	}
	credential, _ := iceconfig.Expiring("user", "password", f.now().Add(10*time.Minute))
	relay, _ := iceconfig.NewRelay(f.Profiles()[0].Identity, []string{"turn:localhost:3478"}, credential)
	return NewLease(LeaseConfig{Relay: relay, Renew: f.issue, Release: func() { f.mu.Lock(); f.closed++; f.mu.Unlock() }}), nil
}
func (f *accountFixture) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.closed
}
func fixtureHeader(s *Sender, sequence uint64) turncontrol.Header {
	return turncontrol.Header{SessionID: s.config.SessionID, OwnerPathID: s.owner, Sequence: sequence}
}
func issueNext(t *testing.T, s *Sender) {
	t.Helper()
	select {
	case result := <-s.completed:
		s.install(context.Background(), result)
	case <-time.After(time.Second):
		t.Fatal("issuance did not complete")
	}
}
func TestDemandReplacementRequiresBothAdmissionsAndRetainsValidOldOnFailure(t *testing.T) {
	now := time.Unix(1700000000, 0)
	accounts := &accountFixture{now: func() time.Time { return now }}
	var messages []turncontrol.Message
	var retired [][16]byte
	s := NewSender(SenderConfig{Accounts: accounts, SessionID: [16]byte{1}, Now: accounts.now,
		Send: func(_ context.Context, body []byte) error {
			message, err := turncontrol.Decode(body)
			messages = append(messages, message)
			return err
		},
		Retire: func(path [16]byte) { retired = append(retired, path) }})
	t.Cleanup(s.close)
	ctx := context.Background()
	s.advertise(ctx)
	s.maintain(ctx)
	if calls, _ := accounts.counts(); calls != 0 {
		t.Fatal("browsing issued credentials")
	}
	s.apply(ctx, turncontrol.Demand{Authority: fixtureHeader(s, 1), HoldFor: time.Minute})
	issueNext(t, s)
	pending := s.services[0].pending
	if pending == nil {
		t.Fatal("grant missing")
	}
	s.apply(ctx, turncontrol.Ready{Authority: pending.grant.Authority, PathID: pending.grant.PathID})
	if s.services[0].current != nil {
		t.Fatal("receiver readiness bypassed sender authentication")
	}
	s.Admitted(pending.grant.PathID)
	s.maintain(ctx)
	if s.services[0].current != pending {
		t.Fatal("early receiver readiness was lost")
	}
	old := pending
	now = now.Add(8 * time.Minute)
	s.apply(ctx, turncontrol.Demand{Authority: fixtureHeader(s, 2), HoldFor: time.Minute})
	issueNext(t, s)
	replacement := s.services[0].pending
	if replacement == nil || replacement.grant.Replaces != old.grant.PathID || len(retired) != 0 {
		t.Fatal("replacement broke current lane")
	}
	authorized, known, err := s.Lookup(replacement.grant.PathID)
	if err != nil || !known || authorized == nil || authorized.ReplacesPeerPathID != old.grant.PathID {
		t.Fatalf("replacement lost predecessor authority: %+v, %v", authorized, err)
	}
	s.Admitted(replacement.grant.PathID)
	s.apply(ctx, turncontrol.Ready{Authority: replacement.grant.Authority, PathID: replacement.grant.PathID})
	if len(retired) != 1 || retired[0] != old.grant.PathID {
		t.Fatal("old path retained after replacement")
	}
	if _, known, err := s.Lookup(old.grant.PathID); !known || err == nil {
		t.Fatal("retired grant admitted a new offer")
	}
	accounts.mu.Lock()
	accounts.fail = true
	accounts.mu.Unlock()
	now = now.Add(8 * time.Minute)
	s.apply(ctx, turncontrol.Demand{Authority: fixtureHeader(s, 3), HoldFor: time.Minute})
	issueNext(t, s)
	if s.services[0].current != replacement || len(retired) != 1 {
		t.Fatal("issuer failure killed still-valid lane")
	}
	s.apply(ctx, turncontrol.Report{Authority: replacement.grant.Authority, PathID: replacement.grant.PathID, UsefulBytes: 1024})
	s.apply(ctx, turncontrol.Report{Authority: replacement.grant.Authority, PathID: replacement.grant.PathID, UsefulBytes: 512})
	if replacement.useful != 1024 {
		t.Fatal("nonmonotonic useful report")
	}
	s.apply(ctx, turncontrol.Demand{Authority: fixtureHeader(s, 4), HoldFor: 0})
	if len(s.paths) != 0 {
		t.Fatal("idle authority retained")
	}
}
func TestCanceledDemandCannotPublishLateAuthority(t *testing.T) {
	now := time.Unix(1700000000, 0)
	accounts := &accountFixture{now: func() time.Time { return now }}
	s := NewSender(SenderConfig{Accounts: accounts, SessionID: [16]byte{1}, Now: accounts.now, Send: func(context.Context, []byte) error { return nil }})
	t.Cleanup(s.close)
	ctx := context.Background()
	s.apply(ctx, turncontrol.Demand{Authority: fixtureHeader(s, 1), HoldFor: time.Minute})
	s.apply(ctx, turncontrol.Demand{Authority: fixtureHeader(s, 2), HoldFor: 0})
	s.apply(ctx, turncontrol.Demand{Authority: fixtureHeader(s, 3), HoldFor: time.Minute})
	issueNext(t, s)
	if len(s.paths) != 0 {
		t.Fatal("published previous activation's credentials")
	}
	if _, closed := accounts.counts(); closed != 1 {
		t.Fatal("late credential not released")
	}
	if !s.services[0].retryAt.IsZero() {
		t.Fatal("canceled demand started a failure cooldown")
	}
	issueNext(t, s)
	if s.services[0].pending == nil || s.services[0].pending.grant.Authority.Sequence != 3 {
		t.Fatal("new demand did not immediately acquire its own credentials")
	}
}

type receiverFixturePath struct {
	ready, done chan struct{}
	once        sync.Once
}

func (p *receiverFixturePath) Ready() <-chan struct{} { return p.ready }
func (p *receiverFixturePath) Done() <-chan struct{}  { return p.done }
func (p *receiverFixturePath) Close() error           { p.once.Do(func() { close(p.done) }); return nil }

func TestReceiverSurvivingControlReplacesWithoutClosingOldLane(t *testing.T) {
	now := time.Unix(1700000000, 0)
	session := [16]byte{1}
	owner := [16]byte{2}
	var sent []turncontrol.Message
	opened := map[[16]byte]*receiverFixturePath{}
	r := NewReceiver(ReceiverConfig{SessionID: session, Now: func() time.Time { return now },
		Send: func(_ context.Context, body []byte) error {
			message, err := turncontrol.Decode(body)
			sent = append(sent, message)
			return err
		},
		Open: func(_ context.Context, grant turncontrol.Grant, config iceconfig.Connection) (ReceiverPath, error) {
			if config.Policy() != iceconfig.RelayCandidates {
				t.Fatal("TURN became direct")
			}
			p := &receiverFixturePath{ready: make(chan struct{}), done: make(chan struct{})}
			opened[grant.PathID] = p
			return p, nil
		}})
	ctx := context.Background()
	available := turncontrol.Available{Authority: turncontrol.Header{SessionID: session, OwnerPathID: owner, Sequence: 1}, Profiles: []string{"a"}}
	r.apply(ctx, available)
	changedInventory := available
	changedInventory.Profiles = []string{"unexpected"}
	r.apply(ctx, changedInventory)
	if len(sent) != 0 {
		t.Fatal("browse created demand")
	}
	r.SetDemand(true)
	r.maintain(ctx)
	grant := turncontrol.Grant{Authority: available.Authority, PathID: [16]byte{3}, Provider: "standard", Account: "owner", Profile: "a",
		CredentialKind: turncontrol.Expiring, Username: "u", Password: "p", ValidFor: time.Minute, URLs: []string{"turn:localhost"}}
	r.apply(ctx, grant)
	close(opened[grant.PathID].ready)
	r.maintain(ctx)
	next := grant
	next.PathID = [16]byte{4}
	next.Replaces = grant.PathID
	// The control callback remains usable after the application relay is gone;
	// neither replacement nor readiness depends on an application-relay handle.
	r.apply(ctx, next)
	close(opened[next.PathID].ready)
	r.maintain(ctx)
	if len(r.paths) != 2 {
		t.Fatal("replacement lacked bounded overlap")
	}
	select {
	case <-opened[grant.PathID].done:
		t.Fatal("old lane retired before sender confirmation")
	default:
	}
	r.apply(ctx, turncontrol.Retire{Authority: grant.Authority, PathID: grant.PathID, Reason: turncontrol.Replaced})
	if len(r.paths) != 1 {
		t.Fatal("retired slot not reclaimed")
	}
	r.apply(ctx, grant)
	if len(r.paths) != 1 {
		t.Fatal("replayed retired grant")
	}
	r.SetDemand(false)
	r.maintain(ctx)
	if len(r.paths) != 0 {
		t.Fatal("idle path retained")
	}
}
