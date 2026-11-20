package relayv2

import (
	"testing"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

func TestChannelCapacityClearanceRequiresAllDirectionsCleared(t *testing.T) {
	sessionID := v2.RelaySessionID{1, 2, 3, 4, 5, 6, 7, 8}
	channel := &Channel{id: sessionID}
	owner := &link{channels: map[v2.RelaySessionID]*Channel{sessionID: channel}}

	owner.observeCapacityWait(v2.CapacityWait{RelaySessionID: sessionID, Direction: 0, Milliseconds: 15_000})
	owner.observeCapacityWait(v2.CapacityWait{RelaySessionID: sessionID, Direction: 1, Milliseconds: 15_000})
	if channel.CapacityWait() <= 0 || channel.CapacityCleared() {
		t.Fatalf("expected active congestion, wait=%v cleared=%v", channel.CapacityWait(), channel.CapacityCleared())
	}
	clearance := channel.CapacityClearance()
	select {
	case <-clearance:
		t.Fatal("clearance closed while both directions are congested")
	default:
	}

	// Clearing only one direction must not clear the channel while the reverse direction is still busy.
	owner.observeCapacityWait(v2.CapacityWait{RelaySessionID: sessionID, Direction: 0, Milliseconds: 0})
	if channel.CapacityCleared() || channel.CapacityWait() <= 0 {
		t.Fatalf("single-direction clear prematurely cleared channel: wait=%v cleared=%v", channel.CapacityWait(), channel.CapacityCleared())
	}
	select {
	case <-clearance:
		t.Fatal("clearance closed while direction 1 was still congested")
	default:
	}

	// Clearing the remaining direction releases the channel and closes the waiter channel.
	owner.observeCapacityWait(v2.CapacityWait{RelaySessionID: sessionID, Direction: 1, Milliseconds: 0})
	if !channel.CapacityCleared() || channel.CapacityWait() != 0 {
		t.Fatalf("expected full clearance, wait=%v cleared=%v", channel.CapacityWait(), channel.CapacityCleared())
	}
	select {
	case <-clearance:
	default:
		t.Fatal("clearance channel did not close when pressure cleared")
	}

	// Renewed pressure resets clearance and allocates a fresh waiter channel.
	owner.observeCapacityWait(v2.CapacityWait{RelaySessionID: sessionID, Direction: 1, Milliseconds: 5_000})
	if channel.CapacityCleared() || channel.CapacityWait() <= 0 {
		t.Fatalf("renewed congestion did not reset clearance")
	}
	select {
	case <-channel.CapacityClearance():
		t.Fatal("fresh clearance channel was already closed")
	default:
	}
}
