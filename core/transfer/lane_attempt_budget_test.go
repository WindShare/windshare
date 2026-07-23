package transfer

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestLaneSetBoundsOneDemandAcrossEpochChurnAndFinalHedgeBatch(t *testing.T) {
	descriptor := transferDescriptor(t, 1)
	demand := validDemand(t, descriptor, 0)
	lanes, err := NewLaneSet(LaneSetConfig{
		ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](61),
		RaceWidth:         MaxLogicalLanes,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lanes.Close()

	var attemptsMu sync.Mutex
	attempts := make(map[LaneIdentity]int)
	var expansionErr error
	recordAttempt := func(identity LaneIdentity) {
		attemptsMu.Lock()
		attempts[identity]++
		attemptsMu.Unlock()
	}
	recordExpansionError := func(err error) {
		attemptsMu.Lock()
		expansionErr = errors.Join(expansionErr, err)
		attemptsMu.Unlock()
	}

	notAdmitted := errors.New("identity did not reach a transport")
	churnLaneID := uint32(MaxLogicalLanes)
	var laneFor func(LaneIdentity) BlockLane
	laneFor = func(identity LaneIdentity) BlockLane {
		return laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
			recordAttempt(identity)
			if identity.ID == churnLaneID {
				switch {
				case identity.Epoch < uint32(MaxDemandLaneAttempts-1):
					next := LaneIdentity{ID: churnLaneID, Epoch: identity.Epoch + 1}
					if addErr := lanes.Add(next, laneFor(next)); addErr != nil {
						recordExpansionError(addErr)
					}
				case identity.Epoch == uint32(MaxDemandLaneAttempts-1):
					// Fifteen sequential incarnations leave one attempt. Exposing a
					// full replacement set here catches a final hedge batch that is
					// bounded only by current lane count instead of remaining demand
					// authority.
					next := LaneIdentity{ID: churnLaneID, Epoch: identity.Epoch + 1}
					if addErr := lanes.Add(next, laneFor(next)); addErr != nil {
						recordExpansionError(addErr)
					}
					for laneID := uint32(1); laneID < churnLaneID; laneID++ {
						candidate := LaneIdentity{ID: laneID, Epoch: 1}
						if addErr := lanes.Add(candidate, laneFor(candidate)); addErr != nil {
							recordExpansionError(addErr)
						}
					}
				}
			}
			return records.BlockRecord{}, NewDemandNotAdmitted(notAdmitted)
		})
	}

	initial := LaneIdentity{ID: churnLaneID, Epoch: 1}
	if err := lanes.Add(initial, laneFor(initial)); err != nil {
		t.Fatal(err)
	}
	if _, err := lanes.fetch(context.Background(), demand, validateTransferRecord(demand)); !errors.Is(err, notAdmitted) {
		t.Fatalf("bounded demand error = %v", err)
	}

	attemptsMu.Lock()
	defer attemptsMu.Unlock()
	if expansionErr != nil {
		t.Fatalf("lane churn failed: %v", expansionErr)
	}
	if len(attempts) != MaxDemandLaneAttempts {
		t.Fatalf("unique identity attempts = %d, want %d: %+v", len(attempts), MaxDemandLaneAttempts, attempts)
	}
	total := 0
	for identity, count := range attempts {
		total += count
		if identity.ID != churnLaneID || count != 1 {
			t.Fatalf("unexpected attempted identity %d/%d count=%d", identity.ID, identity.Epoch, count)
		}
	}
	if total != MaxDemandLaneAttempts {
		t.Fatalf("physical attempts = %d, want %d", total, MaxDemandLaneAttempts)
	}
	for epoch := uint32(1); epoch <= uint32(MaxDemandLaneAttempts); epoch++ {
		if attempts[LaneIdentity{ID: churnLaneID, Epoch: epoch}] != 1 {
			t.Fatalf("churn epoch %d was not attempted exactly once", epoch)
		}
	}
}
