package v2peer

import (
	"testing"

	"github.com/windshare/windshare/core/session/peerslot"
)

func testPeerSlot(t *testing.T) *peerslot.Lease {
	t.Helper()
	slot, err := (&peerslot.Pool{}).Acquire()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(slot.Close)
	return slot
}
