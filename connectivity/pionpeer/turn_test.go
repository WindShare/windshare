package pionpeer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/pion/turn/v5"
	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/connectivity/socketauthority"
	"github.com/windshare/windshare/connectivity/turncapacity"
	"github.com/windshare/windshare/connectivity/turnroute"
)

func localTURN(t *testing.T, protocol string) (string, *turn.Server) {
	t.Helper()
	config := turn.ServerConfig{Realm: "windshare.test", AuthHandler: func(attributes *turn.RequestAttributes) (string, []byte, bool) {
		return "u", turn.GenerateAuthKey("u", "windshare.test", "p"), attributes.Username == "u"
	}}
	generator := &turn.RelayAddressGeneratorStatic{RelayAddress: net.IPv4(127, 0, 0, 1), Address: "127.0.0.1"}
	var address string
	if protocol == "udp" {
		listener, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		config.PacketConnConfigs = []turn.PacketConnConfig{{PacketConn: listener, RelayAddressGenerator: generator}}
		address = listener.LocalAddr().String()
	} else {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		config.ListenerConfigs = []turn.ListenerConfig{{Listener: listener, RelayAddressGenerator: generator}}
		address = listener.Addr().String()
	}
	server, err := turn.NewServer(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return fmt.Sprintf("turn:%s?transport=%s", address, protocol), server
}
func turnConnection(t *testing.T, lease *socketauthority.Lease, endpoints ...string) *Connection {
	t.Helper()
	credential, _ := iceconfig.Fixed("u", "p")
	relay, err := iceconfig.NewRelay(iceconfig.Identity{Provider: "test", Account: "account", Profile: "local"}, endpoints, credential)
	if err != nil {
		t.Fatal(err)
	}
	config, _ := iceconfig.New(nil, &relay, iceconfig.RelayCandidates)
	reservation, err := turncapacity.NewPool().NewOwner().Acquire(context.Background(), turncapacity.Request{
		ProtocolSessionID: lease.SessionID(), PeerPathID: lease.PathID(), Service: relay.Identity(), Sockets: len(endpoints),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reservation.Close)
	plan, err := turnroute.NewPlan(relay, nil)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := NewPeerConnection(pion.Configuration{}, AttemptConfig{TURN: &TURNConfig{Plan: plan, Reservation: reservation}, ProtocolSessionID: lease.SessionID(), NetworkGenerationID: lease.GenerationID(), PeerPathID: lease.PathID(), SocketLease: lease, ICE: config})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}
func TestNativeTURNOverUDPAndTCPReleasesSeparateSockets(t *testing.T) {
	for _, protocol := range []string{"udp", "tcp"} {
		t.Run(protocol, func(t *testing.T) {
			endpoint, server := localTURN(t, protocol)
			authority := socketauthority.New(socketauthority.Config{})
			t.Cleanup(func() { _ = authority.Close() })
			left := turnConnection(t, testLease(t, authority, 1, "127.0.0.1"), endpoint)
			right := turnConnection(t, testLease(t, authority, 2, "127.0.0.1"), endpoint)
			received := make(chan string, 1)
			opened := make(chan struct{})
			right.OnDataChannel(func(channel *pion.DataChannel) {
				channel.OnMessage(func(message pion.DataChannelMessage) { received <- string(message.Data) })
			})
			channel, err := left.CreateDataChannel("turn-test", nil)
			if err != nil {
				t.Fatal(err)
			}
			channel.OnOpen(func() { close(opened) })
			leftGather := pion.GatheringCompletePromise(left.PeerConnection)
			offer, err := left.CreateOffer(nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := left.SetLocalDescription(offer); err != nil {
				t.Fatal(err)
			}
			await(t, leftGather)
			if err := right.SetRemoteDescription(*left.LocalDescription()); err != nil {
				t.Fatal(err)
			}
			rightGather := pion.GatheringCompletePromise(right.PeerConnection)
			answer, err := right.CreateAnswer(nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := right.SetLocalDescription(answer); err != nil {
				t.Fatal(err)
			}
			await(t, rightGather)
			if err := left.SetRemoteDescription(*right.LocalDescription()); err != nil {
				t.Fatal(err)
			}
			await(t, opened)
			if err := channel.SendText("TURN payload"); err != nil {
				t.Fatal(err)
			}
			if await(t, received) != "TURN payload" {
				t.Fatal("payload corrupted")
			}
			pair, ok := left.SelectedPair()
			if !ok || pair.LocalType != "relay" || pair.RemoteType != "relay" {
				t.Fatalf("not TURN: %+v", pair)
			}
			stats, ok := left.TURNResources()
			if !ok || stats.ActiveSockets == 0 || stats.TransportSent == 0 || stats.TransportReceived == 0 {
				t.Fatalf("untracked socket: %+v", stats)
			}
			awaitTURNAllocations(t, server, 2)
			if err := left.Close(); err != nil {
				t.Fatal(err)
			}
			stats, _ = left.TURNResources()
			if stats.ActiveSockets != 0 {
				t.Fatalf("socket leak: %+v", stats)
			}
			awaitTURNAllocations(t, server, 1)
			if err := right.Close(); err != nil {
				t.Fatal(err)
			}
			awaitTURNAllocations(t, server, 0)
		})
	}
}
func TestTURNCloseCancelsTLSHandshakeAndDNS(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			accepted <- connection
		}
	}()
	authority := socketauthority.New(socketauthority.Config{})
	t.Cleanup(func() { _ = authority.Close() })
	connection := turnConnection(t, testLease(t, authority, 1, "127.0.0.1"), "turns:"+listener.Addr().String()+"?transport=tcp")
	_, err = connection.CreateDataChannel("cancel", nil)
	if err != nil {
		t.Fatal(err)
	}
	offer, err := connection.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	socket := await(t, accepted)
	defer socket.Close()
	done := make(chan error, 1)
	go func() { done <- connection.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("TLS handshake blocked Close")
	}
	if _, err := connection.turn.ResolveUDPAddr("udp", "unresolved.invalid:3478"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if stats, _ := connection.TURNResources(); stats.ActiveSockets != 0 {
		t.Fatal(stats)
	}
}
func TestTURNPerPathSocketCapacityIsReclaimed(t *testing.T) {
	capacity := turncapacity.NewPool()
	reservation := reserveTURN(t, capacity.NewOwner(), turnRequest(1, 0, turncapacity.MaximumPathSockets))
	network, err := newTURNNetwork(reservation)
	if err != nil {
		t.Fatal(err)
	}
	defer network.close()
	for range turncapacity.MaximumPathSockets {
		if _, err := network.ListenPacket("udp4", "127.0.0.1:0"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := network.ListenPacket("udp4", "127.0.0.1:0"); !errors.Is(err, errTURNSocketLimit) {
		t.Fatal(err)
	}
	network.close()
	if network.snapshot().ActiveSockets != 0 {
		t.Fatal("sockets retained")
	}
}
