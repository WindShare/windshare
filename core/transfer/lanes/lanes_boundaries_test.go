package lanes

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestLaneCapabilitiesRejectNilAndStaleAuthorities(t *testing.T) {
	notAdmitted := NewDemandNotAdmitted(nil)
	if !errors.Is(notAdmitted, ErrInvalid) || !isDemandNotAdmitted(notAdmitted) {
		t.Fatalf("nil demand rejection = %v", notAdmitted)
	}
	retired := NewDemandReassignableAfterRetirement(nil)
	if !errors.Is(retired, ErrInvalid) || !isDemandReassignableAfterRetirement(retired) {
		t.Fatalf("nil retired demand = %v", retired)
	}
	var nilSuspension *ContentSuspension
	if err := nilSuspension.Resume(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil suspension resume = %v", err)
	}

	lanes := newBoundaryLaneSet(t, 1)
	identity := Identity{ID: 1, Epoch: 1}
	if err := lanes.Add(identity, RouteRelay, boundaryBlockLaneFunc(func(context.Context, Demand) (records.BlockRecord, error) {
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
	if err := suspension.Resume(); !errors.Is(err, ErrStale) {
		t.Fatalf("replaced suspension resume = %v", err)
	}
}

func TestLaneFetchReassignsOnlyWithRetiredOperationAuthority(t *testing.T) {
	lanes := newBoundaryLaneSet(t, 9)
	retiredCause := errors.New("authenticated fragment progress stopped")
	if err := lanes.Add(Identity{ID: 1, Epoch: 1}, RouteRelay, boundaryBlockLaneFunc(func(context.Context, Demand) (records.BlockRecord, error) {
		return records.BlockRecord{}, NewDemandReassignableAfterRetirement(retiredCause)
	})); err != nil {
		t.Fatal(err)
	}
	if err := lanes.Add(Identity{ID: 2, Epoch: 1}, RouteRelay, boundaryBlockLaneFunc(func(context.Context, Demand) (records.BlockRecord, error) {
		return records.BlockRecord{}, nil
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := lanes.Fetch(context.Background(), Request{Demand: Demand{}, Validate: func(records.BlockRecord) error { return nil }}); err != nil {
		t.Fatalf("retired block operation did not reach replacement lane: %v", err)
	}
}

func TestLaneCandidateTerminalConditions(t *testing.T) {
	t.Run("attempt budget", func(t *testing.T) {
		lanes := newBoundaryLaneSet(t, 2)
		attempted := make(map[Identity]struct{}, MaxDemandAttempts)
		for index := 1; index <= MaxDemandAttempts; index++ {
			attempted[Identity{ID: uint32(index), Epoch: 1}] = struct{}{}
		}
		selected, exhausted, err := lanes.candidates(context.Background(), attempted, Request{})
		if err != nil || !exhausted || len(selected) != 0 {
			t.Fatalf("attempt budget = selected %d, exhausted %v, err %v", len(selected), exhausted, err)
		}
	})

	t.Run("all current identities attempted", func(t *testing.T) {
		lanes := newBoundaryLaneSet(t, 3)
		identity := Identity{ID: 1, Epoch: 1}
		if err := lanes.Add(identity, RouteRelay, boundaryBlockLaneFunc(func(context.Context, Demand) (records.BlockRecord, error) {
			return records.BlockRecord{}, nil
		})); err != nil {
			t.Fatal(err)
		}
		selected, exhausted, err := lanes.candidates(context.Background(), map[Identity]struct{}{identity: {}}, Request{})
		if err != nil || !exhausted || len(selected) != 0 {
			t.Fatalf("attempted identities = selected %d, exhausted %v, err %v", len(selected), exhausted, err)
		}
	})

	t.Run("caller cancellation", func(t *testing.T) {
		lanes := newBoundaryLaneSet(t, 4)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, err := lanes.candidates(ctx, nil, Request{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled candidate wait = %v", err)
		}
	})
}

func TestLaneFetchTerminatesAfterEveryIdentityRejectsAdmission(t *testing.T) {
	lanes := newBoundaryLaneSet(t, 7)
	rejection := errors.New("transport not reached")
	if err := lanes.Add(Identity{ID: 1, Epoch: 1}, RouteRelay, boundaryBlockLaneFunc(func(context.Context, Demand) (records.BlockRecord, error) {
		return records.BlockRecord{}, NewDemandNotAdmitted(rejection)
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := lanes.Fetch(context.Background(), Request{Demand: Demand{}, Validate: func(records.BlockRecord) error { return nil }}); !errors.Is(err, rejection) {
		t.Fatalf("exhausted lane fetch = %v", err)
	}
}

type boundaryBlockLaneFunc func(context.Context, Demand) (records.BlockRecord, error)

func (function boundaryBlockLaneFunc) FetchBlock(
	ctx context.Context,
	demand Demand, _ func(uint64),
) (records.BlockRecord, error) {
	return function(ctx, demand)
}

func newBoundaryLaneSet(t *testing.T, seed byte) *Set {
	t.Helper()
	lanes, err := New(Config{
		ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](seed),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lanes.Close)
	return lanes
}
