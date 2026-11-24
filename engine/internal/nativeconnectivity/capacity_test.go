package nativeconnectivity

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/turnsession"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func newTestCapacity(t *testing.T) *Capacity {
	t.Helper()
	return NewCapacity(nativepeer.AdmissionClock{}, nil)
}

type stubMonitor struct{}

func (stubMonitor) Poll(context.Context, time.Time) (networkstate.Snapshot, bool, error) {
	return networkstate.Snapshot{}, false, nil
}

func TestCapacitySharesDefaultNetworkMonitorAcrossNativeOwners(t *testing.T) {
	shared := networkstate.NewWatcher(networkstate.WatcherConfig{Interval: time.Hour})
	defer shared.Close()
	capacity := NewCapacity(nativepeer.AdmissionClock{}, shared)
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

func TestCapacityIsolatesSenderAndReceiverTURNCapacities(t *testing.T) {
	var nilCapacity *Capacity
	if nilCapacity.Sends() != nil || nilCapacity.SenderTURN() != nil || nilCapacity.ReceiverTURN() != nil {
		t.Fatal("nil capacity returned a non-nil authority")
	}

	synctest.Test(t, func(t *testing.T) {
		capacity := newTestCapacity(t)
		senderA, err := senderConfig(SenderConfig{Capacity: capacity})
		if err != nil {
			t.Fatal(err)
		}
		defer senderA.Native.Close(context.Background())
		senderB, err := senderConfig(SenderConfig{Capacity: capacity})
		if err != nil {
			t.Fatal(err)
		}
		defer senderB.Native.Close(context.Background())

		receiverNative, err := capacity.NewNative(nativepeer.Config{Side: nativepeer.SideReceiver})
		if err != nil {
			t.Fatal(err)
		}
		defer receiverNative.Close(context.Background())
		receiverA := receiverConfig(ReceiverConfig{Capacity: capacity, Native: receiverNative})
		receiverB := receiverConfig(ReceiverConfig{Capacity: capacity, Native: receiverNative})

		if senderA.TURNCapacity == nil || senderA.TURNCapacity != senderB.TURNCapacity {
			t.Fatal("sender factories did not share the engine sender TURN capacity")
		}
		if receiverA.TURNCapacity == nil || receiverA.TURNCapacity != receiverB.TURNCapacity {
			t.Fatal("receiver factories did not share the engine receiver TURN capacity")
		}
		if senderA.TURNCapacity == receiverA.TURNCapacity {
			t.Fatal("sender and receiver shared one TURN capacity pool")
		}

		var receiverReleases []func()
		for range protocolsession.MaxGrantsPerMessage {
			release, err := receiverA.TURNCapacity.Acquire(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			receiverReleases = append(receiverReleases, release)
		}
		defer func() {
			for _, release := range receiverReleases {
				release()
			}
		}()

		acquireAsync := func(pool *turnsession.Capacity) <-chan func() {
			done := make(chan func(), 1)
			go func() {
				release, err := pool.Acquire(t.Context())
				if err == nil {
					done <- release
				}
			}()
			return done
		}

		extraReceiver := acquireAsync(receiverB.TURNCapacity)
		synctest.Wait()
		if len(extraReceiver) != 0 {
			t.Fatal("receiver TURN capacity exceeded its bound")
		}

		// Full receiver load must leave the full sender TURN pool available.
		var senderReleases []func()
		for range protocolsession.MaxGrantsPerMessage {
			release, err := senderA.TURNCapacity.Acquire(t.Context())
			if err != nil {
				t.Fatalf("sender TURN acquire blocked by receiver permits: %v", err)
			}
			senderReleases = append(senderReleases, release)
		}
		defer func() {
			for _, release := range senderReleases {
				release()
			}
		}()

		extraSender := acquireAsync(senderB.TURNCapacity)
		synctest.Wait()
		if len(extraSender) != 0 {
			t.Fatal("sender TURN capacity exceeded its bound")
		}

		receiverReleases[0]()
		synctest.Wait()
		if len(extraSender) != 0 {
			t.Fatal("releasing a receiver permit unblocked the sender pool")
		}
		(<-extraReceiver)()

		senderReleases[0]()
		synctest.Wait()
		(<-extraSender)()
	})
}
