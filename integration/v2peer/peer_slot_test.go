package v2peer_test

import (
	"testing"

	"github.com/windshare/windshare/core/session/peerslot"
)

func integrationPeerSlot(t *testing.T, pool *peerslot.Pool) *peerslot.Lease {
	t.Helper()
	slot, err := pool.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(slot.Close)
	return slot
}
