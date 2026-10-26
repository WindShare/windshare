package receiversession

import (
	sessioncontract "github.com/windshare/windshare/core/session"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
	"testing"
)

func TestReceiverLaneSettlementLabelsInitialRelayAndAttachedDirect(t *testing.T) {
	fixture := newVerticalFixture(t)
	config := fixture.receiverConfig
	config.LaneSettlementObservationCapacity = transferlanes.DefaultSettlementObservationCapacity
	receiverFactory, err := NewFactory(config)
	if err == nil {
		trackReceiverFactory(receiverFactory)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(receiverFactory.Close)

	sender, receiver := connectVerticalPair(t, fixture.senderFactory, receiverFactory)
	observations := receiver.LaneSet().SettlementObservations()
	initialID, initialEpoch := receiver.LaneIdentity()
	initial := sessioncontract.LaneIdentity{ID: initialID, Epoch: initialEpoch}
	attached, _, _, _ := attachObservedLane(t, fixture.senderFactory, receiver, 0)
	receiver.Close()
	sender.Close()

	routes := make(map[sessioncontract.LaneIdentity]transferlanes.Route)
	for summary := range observations {
		identity := sessioncontract.LaneIdentity{ID: summary.Lane.ID, Epoch: summary.Lane.Epoch}
		if _, duplicate := routes[identity]; duplicate {
			t.Fatalf("lane %v settled more than once", identity)
		}
		if summary.ProtocolSessionID != receiver.ProtocolSessionID() {
			t.Fatalf("lane %v session = %x", identity, summary.ProtocolSessionID)
		}
		routes[identity] = summary.Route
	}
	if len(routes) != 2 || routes[initial] != transferlanes.RouteRelay || routes[attached] != transferlanes.RouteDirect {
		t.Fatalf("receiver lane routes = %+v; initial=%v attached=%v", routes, initial, attached)
	}
}
