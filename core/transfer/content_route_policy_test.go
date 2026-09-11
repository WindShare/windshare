package transfer

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestContentRoutePolicyCoversInitialAdditionalAndReplacementLanes(t *testing.T) {
	for _, policy := range []ContentRoutePolicy{ContentRouteAll, ContentRouteDirectOnly, ContentRouteRelayOnly} {
		t.Run(fmt.Sprintf("policy=%d", policy), func(t *testing.T) {
			lanes, err := NewLaneSet(LaneSetConfig{ProtocolSessionID: protocolsession.ProtocolSessionID{1}, ContentRoutePolicy: policy, RaceWidth: 1})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(lanes.Close)
			const policyTestTimeout = 5 * time.Second
			ctx, cancel := context.WithTimeout(t.Context(), policyTestTimeout)
			defer cancel()
			descriptor := transferDescriptor(t, 1)
			demand := validDemand(t, descriptor, 0)
			record := transferRecord(t, descriptor, 0)
			var callsMu sync.Mutex
			called := make(map[LaneRoute]int)
			routes := []LaneRoute{LaneRouteRelay, LaneRouteDirect, LaneRouteTURN}
			addRoutes := func(firstID, epoch uint32) {
				t.Helper()
				for index, route := range routes {
					lane := laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
						callsMu.Lock()
						called[route]++
						callsMu.Unlock()
						return record, nil
					})
					if err := lanes.Add(LaneIdentity{ID: firstID + uint32(index), Epoch: epoch}, route, lane); err != nil {
						t.Fatal(err)
					}
				}
			}
			fetch := func(stage string) {
				t.Helper()
				if _, err := lanes.fetch(ctx, demand, validateTransferRecord(demand)); err != nil {
					t.Fatalf("%s lanes: %v", stage, err)
				}
			}
			const initialLaneID = 1
			additionalLaneID := initialLaneID + uint32(len(routes))
			addRoutes(initialLaneID, 0)
			fetch("initial")
			addRoutes(additionalLaneID, 0)
			fetch("additional")
			addRoutes(initialLaneID, 1)
			addRoutes(additionalLaneID, 1)
			fetch("replacement")

			// RaceWidth limits initial selection; exploration can still run a probe.
			// Close joins every callback before the complete observations are read.
			lanes.Close()
			if len(called) == 0 {
				t.Fatal("no policy-admitted route was called")
			}
			for route, count := range called {
				if !policy.Allows(route) {
					t.Errorf("policy %v admitted route %v for %d calls", policy, route, count)
				}
			}
		})
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
