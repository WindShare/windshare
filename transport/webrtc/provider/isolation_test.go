package provider

import (
	"net"
	"sync"
	"testing"

	"github.com/pion/stun/v3"
	"github.com/windshare/windshare/connectivity/socketauthority"
)

type capturedPacketConn struct {
	net.PacketConn
	mu          sync.Mutex
	packet      []byte
	destination net.Addr
}

func (c *capturedPacketConn) WriteTo(payload []byte, destination net.Addr) (int, error) {
	if stun.IsMessage(payload) {
		c.mu.Lock()
		if c.packet == nil {
			c.packet = append([]byte{}, payload...)
			c.destination = destination
		}
		c.mu.Unlock()
	}
	return c.PacketConn.WriteTo(payload, destination)
}
func (c *capturedPacketConn) replay() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.PacketConn.WriteTo(c.packet, c.destination)
	return err
}

func TestIndependentPathsRetainPayloadAndRejectOldAttemptChecks(t *testing.T) {
	var captured []*capturedPacketConn
	authority := socketauthority.New(socketauthority.Config{ListenPacket: func(network, address string) (net.PacketConn, error) {
		conn, err := net.ListenPacket(network, address)
		if err != nil {
			return nil, err
		}
		wrapped := &capturedPacketConn{PacketConn: conn}
		captured = append(captured, wrapped)
		return wrapped, nil
	}})
	defer authority.Close()
	a, b := testLease(t, authority, 1, "127.0.0.1"), testLease(t, authority, 2, "127.0.0.1")
	// Equal path IDs in different authenticated sessions must preserve the same
	// independent lifetime as different paths within one session.
	c, d := testSessionLease(t, authority, [16]byte{2}, 1, "127.0.0.1"), testSessionLease(t, authority, [16]byte{2}, 2, "127.0.0.1")
	firstLeft, firstRight := testConnection(t, a, nil, nil), testConnection(t, b, nil, nil)
	connectPayload(t, firstLeft, firstRight, nil)
	otherLeft, otherRight := testConnection(t, c, nil, nil), testConnection(t, d, nil, nil)
	otherChannel, otherReceived := connectPayload(t, otherLeft, otherRight, nil)
	_ = firstLeft.Close()
	_ = firstRight.Close()
	nextLeft, nextRight := testConnection(t, a, nil, nil), testConnection(t, b, nil, nil)
	nextChannel, nextReceived := connectPayload(t, nextLeft, nextRight, nil)
	if err := captured[0].replay(); err != nil {
		t.Fatal(err)
	}
	if err := captured[1].replay(); err != nil {
		t.Fatal(err)
	}
	const marker = "payload after prior attempt check replay"
	if err := nextChannel.SendText(marker); err != nil {
		t.Fatal(err)
	}
	if got := await(t, nextReceived); got != marker {
		t.Fatal(got)
	}
	if err := otherChannel.SendText(marker); err != nil {
		t.Fatal(err)
	}
	if got := await(t, otherReceived); got != marker {
		t.Fatal("unrelated path lost payload")
	}
}
