package relayv2_test

import (
	"github.com/windshare/windshare/core/liveshare"
	"testing"
)

func testReceiveCapacity(t testing.TB) *liveshare.ReceiveCapacity {
	t.Helper()
	capacity, err := liveshare.NewReceiveCapacity(liveshare.ReceiveCapacityLimits{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := capacity.Close(); err != nil {
			t.Error(err)
		}
	})
	return capacity
}
