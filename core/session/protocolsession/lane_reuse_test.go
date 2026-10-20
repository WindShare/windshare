package protocolsession

import (
	"errors"
	"testing"
	"time"
)

func TestAutomaticLaneGrantsReuseSessionSlotsAcrossPathReplacement(t *testing.T) {
	now := time.Unix(100, 0)
	registry, share, session, traffic, _ := newLifecycleLaneRegistry(t, &now)
	registry.maxLogical = 3 // Initial control, current content, and its replacement.
	registry.maxPending = DefaultMaxPendingGrants
	defer registry.Close()
	var current LaneGrant
	var released []LaneGrant
	for i := range DefaultMaxLogicalLanes * 3 {
		grant, err := registry.IssueGrant(0, testOperationID(byte(i+1)), laneSequence(byte(i+2), LaneAttachNonceBytes))
		if err != nil {
			t.Fatalf("replacement %d: %v", i, err)
		}
		if grant.LaneID == current.LaneID {
			t.Fatal("replacement stole the live lane")
		}
		hello, err := NewLaneHello(share, session, grant.LaneID, grant.LaneEpoch, grant.OperationID, grant.AttachNonce[:], traffic)
		if err != nil {
			t.Fatal(err)
		}
		admission, err := registry.AdmitCandidate(hello.Encoded(), laneSequence(180, LaneSenderNonceBytes), allowLaneSettlement)
		if err != nil || admission.Disposition != LaneAdmissionAccepted {
			t.Fatalf("replacement %d admission: %+v, %v", i, admission, err)
		}
		for _, old := range released {
			if registry.Release(old.LaneID, old.LaneEpoch) {
				t.Fatal("late release removed a newer occupant")
			}
		}
		if current.LaneID != 0 {
			if !registry.Release(current.LaneID, current.LaneEpoch) {
				t.Fatal("current lane was not retained until handoff")
			}
			released = append(released, current)
		}
		current = grant
		if len(registry.lanes) > 3 || registry.activeCount != 2 {
			t.Fatalf("unbounded session slots: %d, active: %d", len(registry.lanes), registry.activeCount)
		}
	}
}

func TestAutomaticLaneReservationExcludesOutstandingGrants(t *testing.T) {
	now := time.Unix(100, 0)
	registry, _, _, _, _ := newLifecycleLaneRegistry(t, &now)
	registry.maxPending = DefaultMaxPendingGrants
	first, err := registry.IssueGrant(0, testOperationID(1), laneSequence(2, LaneAttachNonceBytes))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.IssueGrant(0, testOperationID(3), laneSequence(4, LaneAttachNonceBytes)); !errors.Is(err, ErrLaneGrantBudget) {
		t.Fatalf("pending slot was allocated twice: %v", err)
	}
	now = now.Add(LaneGrantTTL)
	next, err := registry.IssueGrant(0, testOperationID(5), laneSequence(6, LaneAttachNonceBytes))
	if err != nil || next.LaneID != first.LaneID || next.LaneEpoch <= first.LaneEpoch {
		t.Fatalf("expired reservation was not recycled: %+v, %v", next, err)
	}
	if !registry.RevokeGrant(first) {
		t.Fatal("expired grant tombstone was lost")
	}
	if _, err := registry.IssueGrant(0, testOperationID(7), laneSequence(8, LaneAttachNonceBytes)); !errors.Is(err, ErrLaneGrantBudget) {
		t.Fatalf("old revocation released the new reservation: %v", err)
	}
	if !registry.RevokeGrant(next) {
		t.Fatal("current reservation not released")
	}
	if _, err := registry.IssueGrant(0, testOperationID(9), laneSequence(10, LaneAttachNonceBytes)); err != nil {
		t.Fatal(err)
	}
}
