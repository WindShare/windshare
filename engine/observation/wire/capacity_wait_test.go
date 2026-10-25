package wire_test

import (
	"testing"
	"time"

	appobs "github.com/windshare/windshare/engine/observation"
	wire "github.com/windshare/windshare/engine/observation/wire"
)

func TestProgressPayloadRecordsRawCapacityWaitIndependentOfPresentation(t *testing.T) {
	for _, visible := range []bool{false, true} {
		snapshot, err := appobs.NewProgressSnapshot(appobs.ProgressSpec{
			Discovery: appobs.DiscoveryComplete, CountersExact: true,
			CapacityActiveWaiters: 2, CapacityAccumulatedWait: 1250 * time.Millisecond,
			CapacityWaitAttempts: 3, CapacityWaitVisible: visible,
		})
		if err != nil {
			t.Fatal(err)
		}
		payload, err := wire.EncodeProgress(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if payload.CapacityWait.ActiveWaiters != "2" ||
			payload.CapacityWait.AccumulatedWaitMS != "1250" ||
			payload.CapacityWait.Attempts != "3" {
			t.Fatalf("capacity wait payload = %+v", payload.CapacityWait)
		}
	}
}
