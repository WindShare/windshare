package peerset

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/turncapacity"
)

func newTestPeerSet(t *testing.T, config Config) (*PeerSet, error) {
	t.Helper()
	if config.Capacity == nil {
		var err error
		config.Capacity, err = NewCapacity(DefaultAttemptCapacity)
		if err != nil {
			t.Fatal(err)
		}
	}
	return New(config)
}

func newTestNative(t *testing.T, config nativepeer.Config) *nativepeer.NativePeerConnectivity {
	t.Helper()
	if config.Admission == nil {
		config.Admission = nativepeer.NewAdmission(nativepeer.AdmissionClock{})
	}
	config.TURNCapacity = turncapacity.NewPool()
	native, err := nativepeer.New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := native.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return native
}

func TestPeerSetRequiresExplicitAttemptCapacity(t *testing.T) {
	if owner, err := New(Config{}); owner != nil || !errors.Is(err, ErrConfig) {
		t.Fatalf("missing capacity accepted: owner=%v err=%v", owner, err)
	}
}

func TestCapacityExplorationRetainsAnOrdinaryOpportunity(t *testing.T) {
	capacity, err := NewCapacity(2)
	if err != nil {
		t.Fatal(err)
	}
	explore := capacity.tryExplore()
	if explore == nil || capacity.tryExplore() != nil {
		t.Fatal("exploration did not preserve an ordinary attempt slot")
	}
	ordinary, err := capacity.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	queued := make(chan func(), 1)
	go func() { release, _ := capacity.acquire(context.Background()); queued <- release }()
	waitQueue(t, capacity, 1)

	// Cleanup paths can race, but they share only the exploratory slot's claim.
	var cleanup sync.WaitGroup
	for range 8 {
		cleanup.Go(explore)
	}
	cleanup.Wait()
	next := receive(t, queued)
	if capacity.tryExplore() != nil {
		t.Fatal("repeated cleanup released another attempt's capacity")
	}
	ordinary()
	next()
	capacity.mu.Lock()
	defer capacity.mu.Unlock()
	if snapshot := capacity.ledger.Snapshot(); snapshot.Used != 0 || snapshot.Holds != 0 {
		t.Fatalf("attempt cleanup retained capacity: %+v", snapshot)
	}
}
