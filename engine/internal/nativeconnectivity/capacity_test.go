package nativeconnectivity

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/v2peer"
)

func newTestCapacity(t *testing.T) *Capacity {
	t.Helper()
	capacity, err := NewCapacity(nativepeer.AdmissionClock{})
	if err != nil {
		t.Fatal(err)
	}
	return capacity
}

func TestFactoriesRejectMissingResourceOwner(t *testing.T) {
	if factory, err := NewSender(SenderConfig{}); factory != nil || !errors.Is(err, ErrCapacityRequired) {
		t.Fatalf("sender accepted missing capacity: factory=%v err=%v", factory, err)
	}
	if factory, err := NewReceiver(ReceiverConfig{}); factory != nil || !errors.Is(err, v2peer.ErrConfig) {
		t.Fatalf("receiver accepted missing native owner: factory=%v err=%v", factory, err)
	}
}
