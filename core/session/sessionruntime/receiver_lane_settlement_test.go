package sessionruntime

import (
	"testing"

	"github.com/windshare/windshare/core/transfer"
)

func TestReceiverLaneSettlementLabelsInitialRelayAndAttachedDirect(t *testing.T) {
	fixture := newVerticalFixture(t)
	config := fixture.receiverConfig
	config.LaneSettlementObservationCapacity = transfer.DefaultLaneSettlementObservationCapacity
	receiverFactory, err := NewReceiverFactory(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(receiverFactory.Close)

	sender, receiver := connectVerticalPair(t, fixture.senderFactory, receiverFactory)
	observations := receiver.LaneSet().SettlementObservations()
	initialID, initialEpoch := receiver.LaneIdentity()
	initial := LaneIdentity{ID: initialID, Epoch: initialEpoch}
	attached, _, _, _ := attachObservedLane(t, fixture.senderFactory, receiver, 0)
	receiver.Close()
	sender.Close()

	routes := make(map[LaneIdentity]transfer.LaneRoute)
	for summary := range observations {
		identity := LaneIdentity{ID: summary.Lane.ID, Epoch: summary.Lane.Epoch}
		if _, duplicate := routes[identity]; duplicate {
			t.Fatalf("lane %v settled more than once", identity)
		}
		if summary.ProtocolSessionID != receiver.ProtocolSessionID() {
			t.Fatalf("lane %v session = %x", identity, summary.ProtocolSessionID)
		}
		routes[identity] = summary.Route
	}
	if len(routes) != 2 || routes[initial] != transfer.LaneRouteRelay || routes[attached] != transfer.LaneRouteDirect {
		t.Fatalf("receiver lane routes = %+v; initial=%v attached=%v", routes, initial, attached)
	}
}
