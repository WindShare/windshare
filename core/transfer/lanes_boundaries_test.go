package transfer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestLaneCapabilitiesRejectNilAndStaleAuthorities(t *testing.T) {
	notAdmitted := NewDemandNotAdmitted(nil)
	if !errors.Is(notAdmitted, ErrInvalidLane) || !isDemandNotAdmitted(notAdmitted) {
		t.Fatalf("nil demand rejection = %v", notAdmitted)
	}
	retired := NewDemandReassignableAfterRetirement(nil)
	if !errors.Is(retired, ErrInvalidLane) || !isDemandReassignableAfterRetirement(retired) {
		t.Fatalf("nil retired demand = %v", retired)
	}
	var nilSuspension *ContentLaneSuspension
	if err := nilSuspension.Resume(); !errors.Is(err, ErrInvalidLane) {
		t.Fatalf("nil suspension resume = %v", err)
	}

	lanes := newBoundaryLaneSet(t, 1)
	identity := LaneIdentity{ID: 1, Epoch: 1}
	if err := lanes.Add(identity, boundaryBlockLaneFunc(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		return records.BlockRecord{}, nil
	})); err != nil {
		t.Fatal(err)
	}
	suspension, err := lanes.SuspendContent(identity)
	if err != nil {
		t.Fatal(err)
	}
	lanes.mu.Lock()
	lanes.contentSuspensions[identity.ID] = &contentLaneSuspensionPolicy{laneID: identity.ID}
	lanes.mu.Unlock()
	if err := suspension.Resume(); !errors.Is(err, ErrStaleLane) {
		t.Fatalf("replaced suspension resume = %v", err)
	}
}

func TestLaneFetchReassignsOnlyWithRetiredOperationAuthority(t *testing.T) {
	lanes := newBoundaryLaneSet(t, 9)
	retiredCause := errors.New("authenticated fragment progress stopped")
	if err := lanes.Add(LaneIdentity{ID: 1, Epoch: 1}, boundaryBlockLaneFunc(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		return records.BlockRecord{}, NewDemandReassignableAfterRetirement(retiredCause)
	})); err != nil {
		t.Fatal(err)
	}
	if err := lanes.Add(LaneIdentity{ID: 2, Epoch: 1}, boundaryBlockLaneFunc(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		return records.BlockRecord{}, nil
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := lanes.fetch(context.Background(), BlockDemand{}, func(records.BlockRecord) error { return nil }); err != nil {
		t.Fatalf("retired block operation did not reach replacement lane: %v", err)
	}
}

func TestLaneCandidateWaitAndAttemptBudgetsAreTerminal(t *testing.T) {
	t.Run("attempt budget", func(t *testing.T) {
		lanes := newBoundaryLaneSet(t, 2)
		attempted := make(map[LaneIdentity]struct{}, MaxDemandLaneAttempts)
		for index := 1; index <= MaxDemandLaneAttempts; index++ {
			attempted[LaneIdentity{ID: uint32(index), Epoch: 1}] = struct{}{}
		}
		selected, exhausted, err := lanes.candidates(context.Background(), attempted)
		if err != nil || !exhausted || len(selected) != 0 {
			t.Fatalf("attempt budget = selected %d, exhausted %v, err %v", len(selected), exhausted, err)
		}
	})

	t.Run("all current identities attempted", func(t *testing.T) {
		lanes := newBoundaryLaneSet(t, 3)
		identity := LaneIdentity{ID: 1, Epoch: 1}
		if err := lanes.Add(identity, boundaryBlockLaneFunc(func(context.Context, BlockDemand) (records.BlockRecord, error) {
			return records.BlockRecord{}, nil
		})); err != nil {
			t.Fatal(err)
		}
		selected, exhausted, err := lanes.candidates(context.Background(), map[LaneIdentity]struct{}{identity: {}})
		if err != nil || !exhausted || len(selected) != 0 {
			t.Fatalf("attempted identities = selected %d, exhausted %v, err %v", len(selected), exhausted, err)
		}
	})

	t.Run("caller cancellation", func(t *testing.T) {
		lanes := newBoundaryLaneSet(t, 4)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, err := lanes.candidates(ctx, nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled candidate wait = %v", err)
		}
	})

	t.Run("lifecycle cancellation", func(t *testing.T) {
		lanes := newBoundaryLaneSet(t, 5)
		done := make(chan error, 1)
		go func() {
			_, _, err := lanes.candidates(context.Background(), nil)
			done <- err
		}()
		lanes.stop()
		if err := <-done; !errors.Is(err, ErrLaneClosed) {
			t.Fatalf("stopped candidate wait = %v", err)
		}
	})

	t.Run("availability change", func(t *testing.T) {
		lanes := newBoundaryLaneSet(t, 6)
		type laneCandidatesResult struct {
			selected []*laneState
			err      error
		}
		done := make(chan laneCandidatesResult, 1)
		go func() {
			selected, _, err := lanes.candidates(context.Background(), nil)
			done <- laneCandidatesResult{selected: selected, err: err}
		}()
		if err := lanes.Add(LaneIdentity{ID: 1, Epoch: 1}, boundaryBlockLaneFunc(func(context.Context, BlockDemand) (records.BlockRecord, error) {
			return records.BlockRecord{}, nil
		})); err != nil {
			t.Fatal(err)
		}
		got := <-done
		if got.err != nil || len(got.selected) != 1 {
			t.Fatalf("availability result = selected %d, err %v", len(got.selected), got.err)
		}
		lanes.attempts.Done()
	})
}

func TestLaneFetchTerminatesAfterEveryIdentityRejectsAdmission(t *testing.T) {
	lanes := newBoundaryLaneSet(t, 7)
	rejection := errors.New("transport not reached")
	if err := lanes.Add(LaneIdentity{ID: 1, Epoch: 1}, boundaryBlockLaneFunc(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		return records.BlockRecord{}, NewDemandNotAdmitted(rejection)
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := lanes.fetch(context.Background(), BlockDemand{}, func(records.BlockRecord) error { return nil }); !errors.Is(err, rejection) {
		t.Fatalf("exhausted lane fetch = %v", err)
	}
}

func TestLaneLatencyAccountingClampsClockRegressionAndSmoothsSamples(t *testing.T) {
	lanes := newBoundaryLaneSet(t, 8)
	state := &laneState{inflight: 3}
	lanes.finish(state, -time.Second, nil, false)
	lanes.finish(state, time.Second, nil, false)
	lanes.finish(state, 3*time.Second, nil, false)
	if state.inflight != 0 || state.latency != 1500*time.Millisecond {
		t.Fatalf("lane accounting = inflight %d, latency %v", state.inflight, state.latency)
	}
}

type boundaryBlockLaneFunc func(context.Context, BlockDemand) (records.BlockRecord, error)

func (function boundaryBlockLaneFunc) FetchBlock(
	ctx context.Context,
	demand BlockDemand,
) (records.BlockRecord, error) {
	return function(ctx, demand)
}

func newBoundaryLaneSet(t *testing.T, seed byte) *LaneSet {
	t.Helper()
	lanes, err := NewLaneSet(LaneSetConfig{
		ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](seed),
		RaceWidth:         1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lanes.Close)
	return lanes
}
