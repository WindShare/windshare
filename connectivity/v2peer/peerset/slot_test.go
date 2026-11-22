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

func TestPhysicalPathReplacementPreservesSessionContinuationContract(t *testing.T) {
	clock := newClock()
	owner, _ := newTestPeerSet(t, Config{Clock: clock})
	validator := v2signal.ReceiverControlValidator{}
	issuedThrough := make(map[uint8]uint64)
	type admitted struct {
		binding v2signal.Binding
	}
	open := func() (*Path, admitted) {
		t.Helper()
		started := make(chan admitted, 1)
		path, err := owner.Open(t.Context(), PathConfig{
			SessionID: protocolsession.ProtocolSessionID{1}, Demand: ContentDemand,
			Budget: NewBudget(clock.Now()),
			Start: func(_ context.Context, binding v2signal.Binding) (Attempt, error) {
				body, err := v2signal.EncodeOffer(v2signal.Offer{Binding: binding, SDP: "v=0"})
				if err != nil {
					return nil, err
				}
				contract, tracked, err := validator.BeginOperationContinuation(protocolsession.MessagePeerOffer, body)
				if err != nil || !tracked {
					t.Fatalf("peer offer contract tracked=%v err=%v", tracked, err)
				}
				authority, ok := contract.(protocolsession.PeerAttemptAuthority)
				if !ok {
					t.Fatal("peer offer contract missing PeerAttemptAuthority")
				}
				peerBinding := authority.PeerAttemptBinding()
				if !peerBinding.Valid() {
					t.Fatalf("invalid peer attempt binding: %+v", peerBinding)
				}
				if peerBinding.AttemptSequence <= issuedThrough[peerBinding.PeerSlot] {
					t.Fatalf("slot %d sequence %d did not advance past %d", peerBinding.PeerSlot, peerBinding.AttemptSequence, issuedThrough[peerBinding.PeerSlot])
				}
				issuedThrough[peerBinding.PeerSlot] = peerBinding.AttemptSequence
				started <- admitted{binding}
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
	for range 24 {
		next, nextAdmission := open()
		if nextAdmission.binding.PeerSlot == currentAdmission.binding.PeerSlot ||
			nextAdmission.binding.PeerSlot == directAdmission.binding.PeerSlot {
			t.Fatal("replacement occupied a live path's slot")
		}
		_ = current.Close()
		clock.advance(protocolsession.OperationTombstoneLifetime)
		current, currentAdmission = next, nextAdmission
	}
	_ = current.Close()
	_ = direct.Close()
}
