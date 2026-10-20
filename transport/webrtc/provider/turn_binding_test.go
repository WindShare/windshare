package provider

import (
	"bytes"
	"encoding/binary"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/stun/v4"
	"github.com/pion/turn/v5"
)

// Reject one peer's ChannelBind while preserving its permission. This models
// server-side binding policy independently of any provider's undocumented limit.
type rejectingTURNEndpoint struct {
	net.PacketConn
	rejectedPeer net.Addr
	rejections   atomic.Int32
}

func (c *rejectingTURNEndpoint) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, addr, err := c.PacketConn.ReadFrom(p)
		if err != nil || !stun.IsMessage(p[:n]) {
			return n, addr, err
		}
		message := &stun.Message{Raw: p[:n]}
		if message.Decode() != nil || message.Type != stun.NewType(stun.MethodChannelBind, stun.ClassRequest) {
			return n, addr, err
		}
		var peer stun.XORMappedAddress
		if peer.GetFromAs(message, stun.AttrXORPeerAddress) != nil ||
			(&net.UDPAddr{IP: peer.IP, Port: peer.Port}).String() != c.rejectedPeer.String() {
			return n, addr, err
		}
		response, buildErr := stun.Build(stun.NewTransactionIDSetter(message.TransactionID),
			stun.NewType(stun.MethodChannelBind, stun.ClassErrorResponse),
			stun.CodeBadRequest, stun.NewLongTermIntegrity("u", "windshare.test", "p"), stun.Fingerprint)
		if buildErr != nil {
			return 0, nil, buildErr
		}
		c.rejections.Add(1)
		if _, err := c.PacketConn.WriteTo(response.Raw, addr); err != nil {
			return 0, nil, err
		}
	}
}

type turnWireCounter struct {
	net.PacketConn
	channelData atomic.Int32
	indications atomic.Int32
}

func (c *turnWireCounter) WriteTo(p []byte, addr net.Addr) (int, error) {
	if len(p) >= 4 && binary.BigEndian.Uint16(p) >= 0x4000 && binary.BigEndian.Uint16(p) <= 0x7fff {
		c.channelData.Add(1)
	} else if stun.IsMessage(p) {
		message := &stun.Message{Raw: p}
		if message.Decode() == nil && message.Type == stun.NewType(stun.MethodSend, stun.ClassIndication) {
			c.indications.Add(1)
		}
	}
	return c.PacketConn.WriteTo(p, addr)
}

func TestTURNChannelBindRejectionPreservesAllocationAndOtherChannels(t *testing.T) {
	listen := func() net.PacketConn {
		t.Helper()
		socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = socket.Close() })
		return socket
	}
	accepted, rejected, later := listen(), listen(), listen()
	endpoint := &rejectingTURNEndpoint{PacketConn: listen(), rejectedPeer: rejected.LocalAddr()}
	server, err := turn.NewServer(turn.ServerConfig{
		Realm: "windshare.test",
		AuthHandler: func(attributes *turn.RequestAttributes) (string, []byte, bool) {
			return "u", turn.GenerateAuthKey("u", "windshare.test", "p"), attributes.Username == "u"
		},
		PacketConnConfigs: []turn.PacketConnConfig{{PacketConn: endpoint,
			RelayAddressGenerator: &turn.RelayAddressGeneratorStatic{
				RelayAddress: net.IPv4(127, 0, 0, 1), Address: "127.0.0.1",
			}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	wire := &turnWireCounter{PacketConn: listen()}
	client, err := turn.NewClient(&turn.ClientConfig{
		TURNServerAddr: endpoint.LocalAddr().String(), Username: "u", Password: "p", Conn: wire,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	if err = client.Listen(); err != nil {
		t.Fatal(err)
	}
	allocation, err := client.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = allocation.Close() })
	exchange := func(peer net.PacketConn) {
		t.Helper()
		payload := []byte("allocation survives a rejected peer binding")
		if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := allocation.WriteTo(payload, peer.LocalAddr()); err != nil {
			t.Fatal(err)
		}
		buffer := make([]byte, 1024)
		n, source, err := peer.ReadFrom(buffer)
		if err != nil || !bytes.Equal(buffer[:n], payload) {
			t.Fatalf("outbound TURN payload: %q, %v", buffer[:n], err)
		}
		if err := allocation.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := peer.WriteTo(payload, source); err != nil {
			t.Fatal(err)
		}
		n, source, err = allocation.ReadFrom(buffer)
		if err != nil || !bytes.Equal(buffer[:n], payload) || source.String() != peer.LocalAddr().String() {
			t.Fatalf("inbound TURN payload: %q from %v, %v", buffer[:n], source, err)
		}
	}
	// Observe a confirmed channel on the wire before provoking an unrelated
	// rejection, then require a new channel to succeed on the same allocation.
	confirmChannel := func(peer net.PacketConn) {
		t.Helper()
		before := wire.channelData.Load()
		deadline := time.Now().Add(time.Second)
		for wire.channelData.Load() == before {
			if time.Now().After(deadline) {
				t.Fatal("accepted binding never switched to ChannelData")
			}
			exchange(peer)
		}
	}
	confirmChannel(accepted)
	exchange(rejected)
	confirmChannel(later)
	for range 16 {
		exchange(rejected)
		exchange(accepted)
	}
	if endpoint.rejections.Load() != 1 {
		t.Fatalf("permanent rejection retried: %d requests", endpoint.rejections.Load())
	}
	if wire.indications.Load() < 16 {
		t.Fatal("rejected binding did not continue through Send indications")
	}
	awaitTURNAllocations(t, server, 1)
	if err := allocation.Close(); err != nil {
		t.Fatal(err)
	}
	awaitTURNAllocations(t, server, 0)
}
