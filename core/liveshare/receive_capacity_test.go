package liveshare

import (
	"testing"
)

func testReceiveCapacity(t testing.TB) *ReceiveCapacity {
	t.Helper()
	capacity, err := NewReceiveCapacity(ReceiveCapacityLimits{})
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
