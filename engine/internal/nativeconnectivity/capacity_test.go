package nativeconnectivity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/v2peer"
)

func newTestCapacity(t *testing.T) *Capacity {
	t.Helper()
	capacity, err := NewCapacity(nativepeer.AdmissionClock{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return capacity
}

type stubMonitor struct{}

func (stubMonitor) Poll(context.Context, time.Time) (networkstate.Snapshot, bool, error) {
	return networkstate.Snapshot{}, false, nil
}

func TestCapacitySharesDefaultNetworkMonitorAcrossNativeOwners(t *testing.T) {
	shared := networkstate.NewWatcher(networkstate.WatcherConfig{Interval: time.Hour})
	defer shared.Close()
	capacity, err := NewCapacity(nativepeer.AdmissionClock{}, shared)
	if err != nil {
		t.Fatal(err)
	}
	a, err := capacity.NewNative(nativepeer.Config{Side: nativepeer.SideSender})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	b, err := capacity.NewNative(nativepeer.Config{Side: nativepeer.SideReceiver, Monitor: stubMonitor{}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(context.Background())
	if capacity.monitor != shared {
		t.Fatal("capacity lost shared monitor")
	}
}

func TestFactoriesRejectMissingResourceOwner(t *testing.T) {
	if factory, err := NewSender(SenderConfig{}); factory != nil || !errors.Is(err, ErrCapacityRequired) {
		t.Fatalf("sender accepted missing capacity: factory=%v err=%v", factory, err)
	}
	if factory, err := NewReceiver(ReceiverConfig{}); factory != nil || !errors.Is(err, v2peer.ErrConfig) {
		t.Fatalf("receiver accepted missing native owner: factory=%v err=%v", factory, err)
	}
}
