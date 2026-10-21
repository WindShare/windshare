package turnpath

import (
	"context"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/core/session/turncontrol"
)

func controlBody(t *testing.T, message turncontrol.Message) []byte {
	t.Helper()
	body, err := turncontrol.Encode(message)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func drainControls(t *testing.T, mailbox *controlMailbox, apply func(context.Context, turncontrol.Message)) {
	t.Helper()
	for {
		message, ok := mailbox.take()
		if !ok {
			return
		}
		apply(t.Context(), message)
	}
}

func TestReportBurstCannotDisplaceReadinessOrRetirement(t *testing.T) {
	now := time.Unix(1700000000, 0)
	accounts := &accountFixture{now: func() time.Time { return now }}
	s := NewSender(SenderConfig{Accounts: accounts, SessionID: [16]byte{1}, Now: accounts.now,
		Send: func(context.Context, []byte) error { return nil }})
	t.Cleanup(s.close)
	s.apply(t.Context(), turncontrol.Demand{Authority: fixtureHeader(s, 1), HoldFor: time.Minute})
	issueNext(t, s)
	path := s.services[0].pending
	lane := turncontrol.Lane{ID: 2, Epoch: 1}
	s.Admit(t.Context(), path.grant.PathID, lane, make(chan struct{}))
	for useful := uint64(1); useful <= controlCommandCapacity*4; useful++ {
		if err := s.Handle(t.Context(), controlBody(t, turncontrol.Report{
			Authority: path.grant.Authority, PathID: path.grant.PathID, UsefulBytes: useful,
		})); err != nil {
			t.Fatal(err)
		}
	}
	ready := turncontrol.Ready{Authority: path.grant.Authority, PathID: path.grant.PathID, Lane: lane}
	for range 2 {
		if err := s.Handle(t.Context(), controlBody(t, ready)); err != nil {
			t.Fatal(err)
		}
	}
	drainControls(t, s.controls, s.apply)
	s.maintain(t.Context())
	if s.services[0].current != path || path.useful != controlCommandCapacity*4 {
		t.Fatal("report burst displaced readiness or lost its final cumulative value")
	}
	// Retirement must settle the final observation even without a maintenance tick.
	if err := s.Handle(t.Context(), controlBody(t, turncontrol.Report{
		Authority: path.grant.Authority, PathID: path.grant.PathID, UsefulBytes: 1024,
	})); err != nil {
		t.Fatal(err)
	}
	if err := s.Handle(t.Context(), controlBody(t, turncontrol.Retire{
		Authority: path.grant.Authority, PathID: path.grant.PathID, Reason: turncontrol.Idle,
	})); err != nil {
		t.Fatal(err)
	}
	drainControls(t, s.controls, s.apply)
	if s.services[0].current != nil || path.useful != 1024 {
		t.Fatal("retirement lost the final report")
	}
}

func TestReceiverInventoryBurstPreservesGrantAndRetire(t *testing.T) {
	header := turncontrol.Header{SessionID: [16]byte{1}, OwnerPathID: [16]byte{2}, Sequence: 1}
	opened := 0
	path := &receiverFixturePath{ready: make(chan struct{}), done: make(chan struct{})}
	r := NewReceiver(ReceiverConfig{SessionID: header.SessionID,
		Send: func(context.Context, []byte) error { return nil },
		Open: func(context.Context, turncontrol.Grant, iceconfig.Connection) (ReceiverPath, error) {
			opened++
			return path, nil
		}})
	t.Cleanup(func() { _ = path.Close() })
	r.SetDemand(true)
	r.apply(t.Context(), turncontrol.Available{Authority: header, Profiles: []string{"primary"}})
	for sequence := uint64(2); sequence <= controlCommandCapacity*4; sequence++ {
		inventory := header
		inventory.Sequence = sequence
		if err := r.Handle(t.Context(), controlBody(t, turncontrol.Available{Authority: inventory, Profiles: []string{"primary"}})); err != nil {
			t.Fatal(err)
		}
	}
	grant := turncontrol.Grant{Authority: header, PathID: [16]byte{3}, Provider: "test", Account: "account", Profile: "primary",
		Username: "user", Password: "password", URLs: []string{"turn:localhost"}, CredentialKind: turncontrol.Fixed}
	for range 2 {
		if err := r.Handle(t.Context(), controlBody(t, grant)); err != nil {
			t.Fatal(err)
		}
	}
	drainControls(t, r.messages, r.apply)
	if opened != 1 || len(r.paths) != 1 {
		t.Fatal("inventory burst lost the grant or replay reopened its path")
	}
	for range 2 {
		if err := r.Handle(t.Context(), controlBody(t, turncontrol.Retire{Authority: header, PathID: grant.PathID, Reason: turncontrol.Replaced})); err != nil {
			t.Fatal(err)
		}
	}
	drainControls(t, r.messages, r.apply)
	if len(r.paths) != 0 || !signalClosed(path.done) {
		t.Fatal("accepted retirement did not close the path")
	}
	if err := r.Handle(t.Context(), controlBody(t, turncontrol.Report{Authority: header, PathID: grant.PathID})); err == nil {
		t.Fatal("receiver accepted a message from the wrong role")
	}
}

func TestDemandCancellationIsNotCoalescedAcrossReactivation(t *testing.T) {
	s, accounts, _ := recoverySender(t)
	for index, hold := range []time.Duration{0, time.Minute} {
		if err := s.Handle(t.Context(), controlBody(t, turncontrol.Demand{
			Authority: fixtureHeader(s, uint64(index+2)), HoldFor: hold,
		})); err != nil {
			t.Fatal(err)
		}
	}
	drainControls(t, s.controls, s.apply)
	if _, closed := accounts.counts(); closed != 1 {
		t.Fatal("new demand erased the preceding cancellation")
	}
	issueNext(t, s)
	if s.services[0].pending.grant.Authority.Sequence != 3 {
		t.Fatal("reactivation reused previous demand authority")
	}
}
