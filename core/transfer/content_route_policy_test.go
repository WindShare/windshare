package transfer

import (
	"context"
	"testing"

	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestContentRoutePolicyCoversInitialAdditionalAndReplacementLanes(t *testing.T) {
	for _, policy := range []ContentRoutePolicy{ContentRouteAll, ContentRouteDirectOnly, ContentRouteRelayOnly} {
		lanes, err := NewLaneSet(LaneSetConfig{ProtocolSessionID: protocolsession.ProtocolSessionID{1}, ContentRoutePolicy: policy, RaceWidth: 1})
		if err != nil {
			t.Fatal(err)
		}
		descriptor := transferDescriptor(t, 1)
		demand := validDemand(t, descriptor, 0)
		called := make(map[LaneRoute]int)
		for _, route := range []LaneRoute{LaneRouteRelay, LaneRouteDirect, LaneRouteTURN} {
			lane := laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
				called[route]++
				if !policy.Allows(route) {
					t.Errorf("policy %v admitted %v", policy, route)
				}
				return transferRecord(t, descriptor, 0), nil
			})
			for epoch := uint32(0); epoch < 3; epoch++ {
				if err := lanes.Add(LaneIdentity{ID: uint32(route), Epoch: epoch}, route, lane); err != nil {
					t.Fatal(err)
				}
			}
		}
		// A high race width would intentionally invoke routes concurrently; one is
		// enough here to prove the selector only returns policy-admitted routes.
		if _, err := lanes.fetch(context.Background(), demand, validateTransferRecord(demand)); err != nil {
			t.Fatal(err)
		}
		if len(called) != 1 {
			t.Fatal(called)
		}
		lanes.Close()
	}
	for _, policy := range []ContentRoutePolicy{ContentRouteAll, ContentRouteDirectOnly, ContentRouteRelayOnly, 99} {
		if policy.Allows(0) {
			t.Fatal("invalid route accepted")
		}
	}
	if _, err := NewLaneSet(LaneSetConfig{ProtocolSessionID: protocolsession.ProtocolSessionID{1}, ContentRoutePolicy: 99}); err == nil {
		t.Fatal("invalid policy accepted")
	}
	if ContentRoutePolicy(99).Allows(LaneRouteDirect) {
		t.Fatal("invalid policy allowed direct")
	}
}
