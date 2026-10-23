package lanes

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestLaneCallbackCanStopWithoutSelfJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		descriptor := transferDescriptor(t, 1)
		demand := validDemand(t, descriptor, 0)
		var lanes *Set
		stopReturned := make(chan struct{})
		lane := laneFunction(func(context.Context, Demand) (records.BlockRecord, error) {
			lanes.Stop()
			close(stopReturned)
			return records.BlockRecord{}, ErrClosed
		})
		var err error
		lanes, err = New(Config{
			ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](92),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := lanes.Add(Identity{ID: 1}, RouteRelay, lane); err != nil {
			t.Fatal(err)
		}

		fetchDone := make(chan error, 1)
		go func() {
			_, fetchErr := lanes.Fetch(context.Background(), Request{Demand: demand, Validate: validateTransferRecord(demand)})
			fetchDone <- fetchErr
		}()
		select {
		case <-stopReturned:
		case <-time.After(time.Second):
			t.Fatal("callback Stop did not return")
		}
		lanes.Close()
		if err := <-fetchDone; !errors.Is(err, ErrClosed) {
			t.Fatalf("self-stopped lane set error = %v", err)
		}
	})
}
