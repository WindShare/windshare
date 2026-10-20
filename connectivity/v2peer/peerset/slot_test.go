package peerset

import (
	"context"
	"testing"

	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/peerslot"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func testPeerSlot(t *testing.T) *peerslot.Lease {
	t.Helper()
	slot, err := (&peerslot.Pool{}).Acquire()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(slot.Close)
	return slot
}

func TestPhysicalPathReplacementPreservesSessionContinuationAuthority(t *testing.T) {
	clock := newClock()
	owner, _ := New(Config{Clock: clock})
	table, err := protocolsession.NewOperationTableWithContinuations(
		protocolsession.OperationLimits{MaxActive: 8, MaxTracked: 64}, clock.Now, v2signal.ReceiverControlValidator{})
	if err != nil {
		t.Fatal(err)
	}
	type admitted struct {
		binding    v2signal.Binding
		generation protocolsession.OperationGeneration
		offer      protocolsession.Message
	}
	var ordinal byte
	open := func() (*Path, admitted) {
		t.Helper()
		ordinal++
		id := protocolsession.OperationID{ordinal}
		started := make(chan admitted, 1)
		path, err := owner.Open(t.Context(), PathConfig{
			SessionID: protocolsession.ProtocolSessionID{1}, Demand: ContentDemand,
			Budget: NewBudget(clock.Now()),
			Start: func(_ context.Context, binding v2signal.Binding) (Attempt, error) {
				body, err := v2signal.EncodeOffer(v2signal.Offer{Binding: binding, SDP: "v=0"})
				if err != nil {
					return nil, err
				}
				offer, err := protocolsession.NewMessage(protocolsession.MessagePeerOffer, &id, body)
				if err != nil {
					return nil, err
				}
				admission, err := table.ObserveInbound(protocolsession.DirectionReceiverToSender, offer)
				if err != nil {
					return nil, err
				}
				started <- admitted{binding, admission.Generation, offer}
				attempt := newAttempt()
				close(attempt.ready)
				return attempt, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = path.Close() })
		return path, receive(t, started)
	}
	direct, directAdmission := open()
	current, currentAdmission := open()
	first := currentAdmission.offer
	for range 24 {
		next, nextAdmission := open()
		if nextAdmission.binding.PeerSlot == currentAdmission.binding.PeerSlot ||
			nextAdmission.binding.PeerSlot == directAdmission.binding.PeerSlot {
			t.Fatal("replacement occupied a live path's slot")
		}
		if err := table.CancelGeneration(currentAdmission.generation); err != nil {
			t.Fatal(err)
		}
		_ = current.Close()
		clock.advance(protocolsession.OperationTombstoneLifetime)
		if disposition, err := table.Observe(protocolsession.DirectionReceiverToSender, first); err != nil ||
			disposition != protocolsession.OperationDrop {
			t.Fatal("retired path resumed after replacement", disposition, err)
		}
		current, currentAdmission = next, nextAdmission
	}
	if table.ActiveCount() != 2 || table.Terminated() {
		t.Fatal("replacement disturbed the surviving session")
	}
	_ = current.Close()
	_ = direct.Close()
}
