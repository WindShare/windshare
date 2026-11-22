package peerset

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/socketauthority"
)

type addressedPacket struct {
	net.PacketConn
	address *net.UDPAddr
}

func (p addressedPacket) LocalAddr() net.Addr { return p.address }

func testSocketAuthority(t *testing.T, capacity int) *socketauthority.Authority {
	t.Helper()
	authority := socketauthority.New(socketauthority.Config{
		Capacity: capacity,
		ListenPacket: func(_, address string) (net.PacketConn, error) {
			requested, err := netip.ParseAddrPort(address)
			if err != nil {
				return nil, err
			}
			conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			port := conn.LocalAddr().(*net.UDPAddr).AddrPort().Port()
			return addressedPacket{PacketConn: conn, address: net.UDPAddrFromAddrPort(netip.AddrPortFrom(requested.Addr().Unmap(), port))}, nil
		},
	})
	t.Cleanup(func() { _ = authority.Close() })
	return authority
}

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
	if config.Sockets == nil {
		config.Sockets = testSocketAuthority(t, 0)
	}
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
	synctest.Test(t, testCapacityExplorationRetainsAnOrdinaryOpportunity)
}

func testCapacityExplorationRetainsAnOrdinaryOpportunity(t *testing.T) {
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
	assertCapacityQueueLength(t, capacity, 1)

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
