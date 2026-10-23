package provider

import (
	"context"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/connectivity/turncapacity"
)

func turnRequest(path, replaces byte, sockets int) turncapacity.Request {
	var old [16]byte
	if replaces != 0 {
		old = [16]byte{replaces}
	}
	return turncapacity.Request{ProtocolSessionID: [16]byte{1}, PeerPathID: [16]byte{path},
		ReplacesPeerPathID: old, Service: iceconfig.Identity{Provider: "test", Account: "a", Profile: "p"}, Sockets: sockets}
}

func reserveTURN(t *testing.T, owner *turncapacity.Owner, request turncapacity.Request) *turncapacity.Reservation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	reservation, err := owner.Acquire(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reservation.Close)
	if err := reservation.Claim(request.ProtocolSessionID, request.PeerPathID, request.Service); err != nil {
		t.Fatal(err)
	}
	return reservation
}

func TestTURNNetworkShutdownLeavesProtocolWritableAndBoundsStalledCleanup(t *testing.T) {
	for _, stalled := range []bool{false, true} {
		t.Run(map[bool]string{false: "protocol", true: "forced"}[stalled], func(t *testing.T) {
			reservation := reserveTURN(t, turncapacity.NewPool().NewOwner(), turnRequest(1, 0, 1))
			network, err := newTURNNetwork(reservation)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(network.close)
			socket, err := network.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			err, forced := network.shutdown(func() error {
				if _, err := network.ListenPacket("udp4", "127.0.0.1:0"); err == nil {
					t.Fatal("shutdown admitted a new socket")
				}
				if !stalled {
					_, err := socket.WriteTo([]byte("release"), socket.LocalAddr())
					return err
				}
				_, _, err := socket.ReadFrom(make([]byte, 32))
				return err
			})
			if forced != stalled || (!stalled && err != nil) {
				t.Fatalf("shutdown: forced=%v err=%v", forced, err)
			}
			if network.snapshot().ActiveSockets != 0 || reservation.SocketLimit() != 0 {
				t.Fatal("shutdown retained resources")
			}
		})
	}
}

func TestTURNHandoffHasRealSocketCapacity(t *testing.T) {
	pool := turncapacity.NewPool()
	owner := pool.NewOwner()
	var networks []*turnNetwork
	for i := range turncapacity.MaximumConnections + 1 {
		predecessor := byte(0)
		if i == turncapacity.MaximumConnections {
			predecessor = 1
		}
		request := turnRequest(byte(i+1), predecessor, turncapacity.MaximumSockets/turncapacity.MaximumConnections)
		reservation := reserveTURN(t, owner, request)
		network, err := newTURNNetwork(reservation)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(network.close)
		networks = append(networks, network)
		for range request.Sockets {
			if _, err := network.ListenPacket("udp4", "127.0.0.1:0"); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Closing the predecessor must settle capacity without stopping its successor.
	networks[0].close()
	successor := networks[len(networks)-1]
	if successor.snapshot().ActiveSockets != turncapacity.MaximumSockets/turncapacity.MaximumConnections {
		t.Fatal("handoff closed the successor")
	}
	for _, network := range networks {
		network.close()
	}
	for i := range turncapacity.MaximumConnections {
		reserveTURN(t, owner, turnRequest(byte(i+1), 0, 1))
	}
}
