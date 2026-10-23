package lanes

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/fault"
)

func TestRequestWithoutValidationCannotAdmitPhysicalWork(t *testing.T) {
	set, err := New(Config{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](43)})
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()
	var fetches, claims atomic.Int32
	if err := set.Add(Identity{ID: 1}, RouteDirect, laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
		fetches.Add(1)
		return records.BlockRecord{}, nil
	})); err != nil {
		t.Fatal(err)
	}
	_, err = set.Fetch(context.Background(), Request{Claim: func() (func(), <-chan struct{}, error) {
		claims.Add(1)
		return func() {}, nil, nil
	}})
	var boundary *fault.BoundaryError
	if !errors.Is(err, ErrInvalidRequest) || !errors.As(err, &boundary) ||
		boundary.Fault() != fault.DependencyContractFault() || fetches.Load() != 0 || claims.Load() != 0 {
		t.Fatalf("unvalidated demand reached a source: error=%v fetches=%d claims=%d", err, fetches.Load(), claims.Load())
	}
}
