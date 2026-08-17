package sessionruntime

import (
	"sync"
	"testing"

	"github.com/windshare/windshare/core/transfer"
)

type receiverLaneSettlementCollector struct {
	mu        sync.Mutex
	summaries []transfer.LaneSettlementSummary
}

func (collector *receiverLaneSettlementCollector) TraceLaneSettlement(summary transfer.LaneSettlementSummary) {
	collector.mu.Lock()
	collector.summaries = append(collector.summaries, summary)
	collector.mu.Unlock()
}

func (collector *receiverLaneSettlementCollector) snapshot() []transfer.LaneSettlementSummary {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	return append([]transfer.LaneSettlementSummary(nil), collector.summaries...)
}

func TestReceiverLaneSettlementLabelsInitialRelayAndAttachedDirect(t *testing.T) {
	fixture := newVerticalFixture(t)
	collector := &receiverLaneSettlementCollector{}
	config := fixture.receiverConfig
	config.LaneSettlementTracer = collector
	receiverFactory, err := NewReceiverFactory(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(receiverFactory.Close)

	sender, receiver := connectVerticalPair(t, fixture.senderFactory, receiverFactory)
	initialID, initialEpoch := receiver.LaneIdentity()
	initial := LaneIdentity{ID: initialID, Epoch: initialEpoch}
	attached, _, _, _ := attachObservedLane(t, fixture.senderFactory, receiver, 0)
	receiver.Close()
	sender.Close()

	routes := make(map[LaneIdentity]transfer.LaneRoute)
	for _, summary := range collector.snapshot() {
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
