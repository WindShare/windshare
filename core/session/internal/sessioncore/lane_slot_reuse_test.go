package sessioncore

import (
	"errors"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
	"testing"
)

func TestLogicalLaneSlotRemainsSelectableAcrossPhysicalReplacement(t *testing.T) {
	for _, retirement := range []string{"detach", "complete"} {
		t.Run(retirement, func(t *testing.T) {
			runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
			lanes := runtime.lanes
			excluded := map[session.LaneIdentity]struct{}{runtime.initial: {}}
			const replacements = protocolsession.DefaultMaxLogicalLanes * 3
			identity := session.LaneIdentity{ID: runtime.initial.ID + 1}
			for epoch := uint32(1); epoch <= replacements; epoch++ {
				identity.Epoch = epoch
				channel, peer := newMemoryChannelPair()
				t.Cleanup(func() { _ = peer.Close() })
				lane, err := lanes.add(identity, channel, permissiveInboundAuthenticator(), false)
				if err != nil {
					t.Fatalf("replacement %d: %v", epoch, err)
				}
				selected, err := lanes.selectLaneExcluding(excluded)
				if err != nil || selected.identity != identity {
					t.Fatalf("replacement %d disappeared from selection: %+v, %v", epoch, selected.identity, err)
				}
				if snapshot := lanes.snapshot(); len(snapshot) != 2 || snapshot[1].identity != identity {
					t.Fatalf("replacement %d lost or duplicated a logical slot: %+v", epoch, snapshot)
				}
				if len(lanes.logicalOrder) != 2 || len(lanes.latestEpochByID) != 2 {
					t.Fatalf("physical replacement accumulated logical slots: order=%d epochs=%d",
						len(lanes.logicalOrder), len(lanes.latestEpochByID))
				}

				switch retirement {
				case "detach":
					if !lanes.detach(identity) {
						t.Fatal("current lane did not detach")
					}
				case "complete":
					lane.closeChannel()
					lanes.completeLane(lane)
				}
				if snapshot := lanes.snapshot(); len(snapshot) != 1 || snapshot[0].identity != runtime.initial {
					t.Fatalf("retired physical lane remained selectable: %+v", snapshot)
				}
			}

			stale, peer := newMemoryChannelPair()
			t.Cleanup(func() { _ = peer.Close() })
			if _, err := lanes.add(identity, stale, permissiveInboundAuthenticator(), false); !errors.Is(err, ErrLaneStale) {
				t.Fatalf("logical slot forgot its retired epoch: %v", err)
			}
		})
	}
}
