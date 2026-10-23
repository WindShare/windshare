package lanes

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
	lanes, err := New(Config{
		ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](61),
		RaceWidth:         MaxLogicalLanes,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lanes.Close()

	var attemptsMu sync.Mutex
	attempts := make(map[Identity]int)
	var expansionErr error
	recordAttempt := func(identity Identity) {
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
	var laneFor func(Identity) BlockSource
	laneFor = func(identity Identity) BlockSource {
		return laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
			recordAttempt(identity)
			if identity.ID == churnLaneID {
				switch {
				case identity.Epoch < uint32(MaxDemandAttempts-1):
					next := Identity{ID: churnLaneID, Epoch: identity.Epoch + 1}
					if addErr := lanes.Add(next, RouteRelay, laneFor(next)); addErr != nil {
						recordExpansionError(addErr)
					}
				case identity.Epoch == uint32(MaxDemandAttempts-1):
					// Fifteen sequential incarnations leave one attempt. Exposing a
					// full replacement set here catches a final hedge batch that is
					// bounded only by current lane count instead of remaining demand
					// authority.
					next := Identity{ID: churnLaneID, Epoch: identity.Epoch + 1}
					if addErr := lanes.Add(next, RouteRelay, laneFor(next)); addErr != nil {
						recordExpansionError(addErr)
					}
					for laneID := uint32(1); laneID < churnLaneID; laneID++ {
						candidate := Identity{ID: laneID, Epoch: 1}
						if addErr := lanes.Add(candidate, RouteRelay, laneFor(candidate)); addErr != nil {
							recordExpansionError(addErr)
						}
					}
				}
			}
			return records.BlockRecord{}, NewDemandNotAdmitted(notAdmitted)
		})
	}

	initial := Identity{ID: churnLaneID, Epoch: 1}
	if err := lanes.Add(initial, RouteRelay, laneFor(initial)); err != nil {
		t.Fatal(err)
	}
	if _, err := lanes.Fetch(context.Background(), Request{Demand: demand, Validate: validateTransferRecord(demand)}); !errors.Is(err, notAdmitted) {
		t.Fatalf("bounded demand error = %v", err)
	}

	attemptsMu.Lock()
	defer attemptsMu.Unlock()
	if expansionErr != nil {
		t.Fatalf("lane churn failed: %v", expansionErr)
	}
	if len(attempts) != MaxDemandAttempts {
		t.Fatalf("unique identity attempts = %d, want %d: %+v", len(attempts), MaxDemandAttempts, attempts)
	}
	total := 0
	for identity, count := range attempts {
		total += count
		if identity.ID != churnLaneID || count != 1 {
			t.Fatalf("unexpected attempted identity %d/%d count=%d", identity.ID, identity.Epoch, count)
		}
	}
	if total != MaxDemandAttempts {
		t.Fatalf("physical attempts = %d, want %d", total, MaxDemandAttempts)
	}
	for epoch := uint32(1); epoch <= uint32(MaxDemandAttempts); epoch++ {
		if attempts[Identity{ID: churnLaneID, Epoch: epoch}] != 1 {
			t.Fatalf("churn epoch %d was not attempted exactly once", epoch)
		}
	}
}
