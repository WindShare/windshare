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
		if count != 1 {
			t.Fatalf("unexpected attempted identity %d/%d count=%d", identity.ID, identity.Epoch, count)
		}
	}
	if total != MaxDemandAttempts {
		t.Fatalf("physical attempts = %d, want %d", total, MaxDemandAttempts)
	}
	for epoch := uint32(1); epoch < uint32(MaxDemandAttempts); epoch++ {
		if attempts[Identity{ID: churnLaneID, Epoch: epoch}] != 1 {
			t.Fatalf("churn epoch %d was not attempted exactly once", epoch)
		}
	}
}

func TestSameLaneWindowRetryRecoversSoleHealthyLaneAndCapsLoop(t *testing.T) {
	retryCause := errors.New("operation fell behind replay window")

	t.Run("recovers_sole_healthy_lane_without_failure_penalty", func(t *testing.T) {
		descriptor := transferDescriptor(t, 1)
		demand := validDemand(t, descriptor, 0)
		set, err := New(Config{
			ProtocolSessionID:             transferID[protocolsession.ProtocolSessionID](63),
			SettlementObservationCapacity: DefaultSettlementObservationCapacity,
		})
		if err != nil {
			t.Fatal(err)
		}
		var calls int
		if err := set.Add(Identity{ID: 1, Epoch: 1}, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
			calls++
			if calls <= 2 {
				return records.BlockRecord{}, NewDemandRetryableAfterRetirement(retryCause)
			}
			return transferRecord(t, descriptor, 0), nil
		})); err != nil {
			t.Fatal(err)
		}
		if _, err := set.Fetch(t.Context(), Request{Demand: demand, Validate: validateTransferRecord(demand)}); err != nil {
			t.Fatalf("sole healthy lane window retry failed: %v", err)
		}
		if calls != 3 {
			t.Fatalf("calls = %d, want 3", calls)
		}
		set.attempts.Wait()
		set.mu.Lock()
		laneFailures := set.lanes[1].failures
		set.mu.Unlock()
		if laneFailures != 0 {
			t.Fatalf("window retry incremented lane failures: %d", laneFailures)
		}
		set.CompleteObservations()
		summary, ok := <-set.SettlementObservations()
		if !ok {
			t.Fatal("missing settlement summary")
		}
		if summary.FailedBlockAttempts != 0 || summary.DeliveredBlocks != 1 {
			t.Fatalf("settlement = %+v, want FailedBlockAttempts=0 DeliveredBlocks=1", summary)
		}
	})

	t.Run("caps_repeated_window_retries_on_sole_lane", func(t *testing.T) {
		descriptor := transferDescriptor(t, 1)
		demand := validDemand(t, descriptor, 0)
		set, err := New(Config{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](64)})
		if err != nil {
			t.Fatal(err)
		}
		defer set.Close()
		var calls int
		if err := set.Add(Identity{ID: 1, Epoch: 1}, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
			calls++
			return records.BlockRecord{}, NewDemandRetryableAfterRetirement(retryCause)
		})); err != nil {
			t.Fatal(err)
		}
		_, fetchErr := set.Fetch(t.Context(), Request{Demand: demand, Validate: validateTransferRecord(demand)})
		if !errors.Is(fetchErr, retryCause) {
			t.Fatalf("capped retry err = %v, want %v", fetchErr, retryCause)
		}
		if calls != maxSameLaneRetries+1 {
			t.Fatalf("calls = %d, want %d", calls, maxSameLaneRetries+1)
		}
	})

	t.Run("prefers_faster_lane_and_falls_back_to_healthy_origin_lane", func(t *testing.T) {
		descriptor := transferDescriptor(t, 1)
		demand := validDemand(t, descriptor, 0)
		set, err := New(Config{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](65)})
		if err != nil {
			t.Fatal(err)
		}
		defer set.Close()
		var order []uint32
		if err := set.Add(Identity{ID: 1, Epoch: 1}, RouteRelay, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
			order = append(order, 1)
			if len(order) == 1 {
				// Attach a faster direct lane before returning the replay-window retry hint.
				if addErr := set.Add(Identity{ID: 2, Epoch: 1}, RouteDirect, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
					order = append(order, 2)
					return records.BlockRecord{}, NewDemandReassignableAfterRetirement(errors.New("direct stalled"))
				})); addErr != nil {
					return records.BlockRecord{}, addErr
				}
				return records.BlockRecord{}, NewDemandRetryableAfterRetirement(retryCause)
			}
			return transferRecord(t, descriptor, 0), nil
		})); err != nil {
			t.Fatal(err)
		}
		if _, err := set.Fetch(t.Context(), Request{Demand: demand, Validate: validateTransferRecord(demand)}); err != nil {
			t.Fatalf("fallback after direct failure = %v", err)
		}
		if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 1 {
			t.Fatalf("attempt order = %v, want [1 2 1]", order)
		}
	})
}
