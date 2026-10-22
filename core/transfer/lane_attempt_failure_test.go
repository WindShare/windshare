package transfer

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/lanescheduling"
)

func TestFailedLaneAttemptReleasesCapacityBeforePublishing(t *testing.T) {
	transportErr := errors.New("transport failed after receiving bytes")
	for _, tc := range []struct {
		name          string
		fetchErr      error
		validationErr error
		want          error
	}{
		{name: "transport", fetchErr: transportErr, want: transportErr},
		{name: "identity", validationErr: ErrBlockIdentity, want: ErrBlockIdentity},
		{name: "cancellation", fetchErr: context.Canceled, want: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			descriptor := transferDescriptor(t, 1)
			demand := validDemand(t, descriptor, 0)
			record := transferRecord(t, descriptor, 0)
			lanes, err := NewLaneSet(LaneSetConfig{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](62)})
			if err != nil {
				t.Fatal(err)
			}
			defer lanes.Close()
			if err := lanes.Add(LaneIdentity{ID: 1}, LaneRouteRelay, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
				return record, tc.fetchErr
			})); err != nil {
				t.Fatal(err)
			}
			var releases atomic.Int32
			candidates, exhausted, err := lanes.candidates(context.Background(), make(map[LaneIdentity]struct{}), blockDemandBytes(demand), func() (func(), <-chan struct{}, error) {
				return func() { releases.Add(1) }, nil, nil
			})
			if err != nil || exhausted || len(candidates) != 1 {
				t.Fatalf("candidate admission: count=%d exhausted=%v error=%v", len(candidates), exhausted, err)
			}
			decision := &laneRoundDecision{done: make(chan struct{})}
			decide := sync.OnceFunc(func() { close(decision.done) })
			defer decide()
			results := make(chan laneResult, 1)
			go lanes.fetchLane(context.Background(), demand, func(records.BlockRecord) error {
				return tc.validationErr
			}, candidates[0], decision, results, lanescheduling.Content)

			// Keep the round undecided so scheduler timing cannot hide capacity
			// retained only for outcome bookkeeping after the physical read failed.
			result := <-results
			if !errors.Is(result.err, tc.want) {
				t.Fatalf("attempt error = %v, want %v", result.err, tc.want)
			}
			if releases.Load() != 1 {
				t.Fatalf("published failed attempt before releasing capacity: releases=%d", releases.Load())
			}
			if result.record.DataLength() != 0 {
				t.Fatal("failed outcome retained rejected plaintext")
			}
			decide()
			lanes.attempts.Wait()
			if releases.Load() != 1 {
				t.Fatalf("attempt capacity released more than once: releases=%d", releases.Load())
			}
		})
	}
}
