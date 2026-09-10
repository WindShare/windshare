package socketauthority

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/pion/stun/v4"
)

func TestPathIsolationReferencesRetirementAndBounds(t *testing.T) {
	authority := New(Config{Capacity: 2})
	defer authority.Close()
	addresses := []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	first, err := authority.Acquire([16]byte{1}, 1, [16]byte{1}, addresses)
	if err != nil {
		t.Fatal(err)
	}
	same, err := authority.Acquire([16]byte{1}, 1, [16]byte{1}, addresses)
	if err != nil {
		t.Fatal(err)
	}
	if first.Endpoints()[0] != same.Endpoints()[0] {
		t.Fatal("path was rebound")
	}
	other, err := authority.Acquire([16]byte{1}, 1, [16]byte{2}, addresses)
	if err != nil {
		t.Fatal(err)
	}
	if first.Endpoints()[0] == other.Endpoints()[0] {
		t.Fatal("peer paths share socket")
	}
	if _, err = authority.Acquire([16]byte{1}, 1, [16]byte{3}, addresses); err != ErrCapacity {
		t.Fatal(err)
	}
	retained, err := first.Retain()
	if err != nil {
		t.Fatal(err)
	}
	mux, release, err := retained.Claim()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = same.Claim(); err != ErrActive {
		t.Fatal(err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = first.Retain(); err != ErrClosed {
		t.Fatal(err)
	}
	conn, err := mux.GetConn("owned", net.UDPAddrFromAddrPort(same.Endpoints()[0]))
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	mux.RemoveConnByUfrag("owned")
	release()
	release()
	authority.Retire(1)
	if _, err = same.Retain(); err != ErrRetired {
		t.Fatal(err)
	}
	if _, _, err = same.Claim(); err != ErrRetired {
		t.Fatal(err)
	}
	if _, err = authority.Acquire([16]byte{1}, 1, [16]byte{1}, addresses); err != ErrRetired {
		t.Fatal(err)
	}
	_ = retained.Close()
	_ = same.Close()
	_ = same.Close()
	_ = other.Close()
	next, err := authority.Acquire([16]byte{1}, 2, [16]byte{1}, addresses)
	if err != nil {
		t.Fatal(err)
	}
	if err = authority.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = authority.Acquire([16]byte{1}, 2, [16]byte{2}, addresses); err != ErrClosed {
		t.Fatal(err)
	}
	if _, _, err = next.Claim(); err != ErrClosed {
		t.Fatal(err)
	}
	_ = next.Close()
}
func TestAllocationValidationAndCleanup(t *testing.T) {
	authority := New(Config{})
	defer authority.Close()
	for _, addresses := range [][]netip.Addr{nil, {netip.Addr{}}, {netip.IPv4Unspecified()}, {netip.MustParseAddr("224.0.0.1")}} {
		if _, err := authority.Acquire([16]byte{1}, 1, [16]byte{1}, addresses); err != ErrInvalid {
			t.Fatal(err)
		}
	}
	addresses := []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	if _, err := authority.Acquire([16]byte{}, 1, [16]byte{1}, addresses); err != ErrInvalid {
		t.Fatal("zero session accepted", err)
	}
	if _, err := authority.Acquire([16]byte{1}, 0, [16]byte{1}, addresses); err != ErrInvalid {
		t.Fatal(err)
	}
	if _, err := authority.Acquire([16]byte{1}, 1, [16]byte{}, addresses); err != ErrInvalid {
		t.Fatal(err)
	}
	lease, err := authority.Acquire([16]byte{1}, 1, [16]byte{1}, addresses)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if _, err = authority.Acquire([16]byte{1}, 1, [16]byte{1}, []netip.Addr{netip.MustParseAddr("127.0.0.2")}); err != ErrInvalid {
		t.Fatal(err)
	}
	var calls int
	var allocated net.PacketConn
	failing := New(Config{ListenPacket: func(network, address string) (net.PacketConn, error) {
		calls++
		if calls == 2 {
			return nil, errors.New("allocation refused")
		}
		var listenErr error
		allocated, listenErr = net.ListenPacket(network, address)
		return allocated, listenErr
	}})
	if _, err = failing.Acquire([16]byte{1}, 1, [16]byte{1}, []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("127.0.0.2")}); err == nil {
		t.Fatal("allocation should fail")
	}
	if _, err = allocated.WriteTo([]byte("closed"), allocated.LocalAddr()); err == nil {
		t.Fatal("partial allocation leaked")
	}
	var absent *Lease
	if _, err = absent.Retain(); err != ErrInvalid {
		t.Fatal(err)
	}
	if _, _, err = absent.Claim(); err != ErrInvalid {
		t.Fatal(err)
	}
	if err = absent.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestLocalSTUNGatherStopsWhenContextIsCanceled(t *testing.T) {
	server, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	authority := New(Config{})
	defer authority.Close()
	lease, err := authority.Acquire([16]byte{1}, 1, [16]byte{1}, []netip.Addr{netip.MustParseAddr("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	mux, release, err := lease.Claim()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, lookupErr := mux.GetXORMappedAddrForLocal(ctx, server.LocalAddr(), mux.GetListenAddresses()[0], time.Minute)
		result <- lookupErr
	}()
	// Cancel an in-flight request so a lost STUN response cannot hold up agent retirement.
	if err = server.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var packet [1500]byte
	if _, _, err = server.ReadFrom(packet[:]); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err = <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("gather cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("gather ignored cancellation")
	}
}

const idleWireInterval = 5 * time.Millisecond
const stunTestPacketBytes = 1500

func TestIdleHandoffRefreshesActualSTUNAndStopsBeforeICE(t *testing.T) {
	server, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	refreshed := make(chan Event, 1)
	authority := New(Config{
		IdleInterval: idleWireInterval, RefreshTimeout: idleTestBudget,
		Observe: func(event Event) {
			if event.Kind == STUNRefreshFinished {
				select {
				case refreshed <- event:
				default:
				}
			}
		},
	})
	defer authority.Close()
	lease, err := authority.Acquire([16]byte{1}, 1, [16]byte{1}, []netip.Addr{netip.MustParseAddr("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	servers := []netip.AddrPort{server.LocalAddr().(*net.UDPAddr).AddrPort()}
	if err = lease.StartIdle(context.Background(), servers, time.Now().Add(idleTestBudget)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		respond := readSTUNRequest(t, server)
		respond()
		if event := receiveHandoff(t, refreshed); event.Result != "completed" {
			t.Fatalf("wire refresh did not complete: %+v", event)
		}
	}
	if err = lease.StartIdle(context.Background(), servers, time.Now().Add(idleTestBudget)); err != nil {
		t.Fatal(err)
	}
	// Withhold a wire response so replacing/claiming idle work also exercises
	// cancellation rather than depending on an already completed refresh.
	_ = readSTUNRequest(t, server)
	idle := lease.entry.idle
	mux, release, err := lease.Claim()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-idle.done:
	default:
		t.Fatal("ICE received the socket before idle work exited")
	}
	if err = lease.StartIdle(context.Background(), servers, time.Now().Add(idleTestBudget)); err != ErrActive {
		t.Fatal(err)
	}
	release()
	if _, err = mux.GetRelayedAddr(nil, 0); err == nil {
		t.Fatal("TURN accepted")
	}
	if _, err = mux.GetConn("x", &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 1}); err == nil {
		t.Fatal("foreign endpoint accepted")
	}
	if _, err = mux.GetConnForURL("x", "stun:x", &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 1}); err == nil {
		t.Fatal("foreign endpoint accepted")
	}
	// A fresh destination guarantees uncached discovery and cannot consume
	// datagrams left on the heartbeat server by canceled transactions.
	lookupServer, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lookupServer.Close()
	lookup := runHandoff(func() error {
		_, lookupErr := mux.GetXORMappedAddrForLocal(context.Background(), lookupServer.LocalAddr(), mux.GetListenAddresses()[0], handoffTestTimeout)
		return lookupErr
	})
	respond := readSTUNRequest(t, lookupServer)
	respond()
	if err = receiveHandoff(t, lookup); err != nil {
		t.Fatal(err)
	}
	if _, err = mux.GetXORMappedAddr(lookupServer.LocalAddr(), handoffTestTimeout); err != nil {
		t.Fatal(err)
	}
	if err = lease.StartIdle(context.Background(), servers, time.Now().Add(-idleTestBudget)); err != ErrInvalid {
		t.Fatal(err)
	}
	authority.Retire(1)
	if err = lease.StartIdle(context.Background(), servers, time.Now().Add(idleTestBudget)); err != ErrRetired {
		t.Fatal(err)
	}
}

// The test controls responses so wire receipt cannot stand in for completion,
// and canceled refreshes can deliberately leave the discovery cache empty.
func readSTUNRequest(t *testing.T, server net.PacketConn) func() {
	t.Helper()
	if err := server.SetReadDeadline(time.Now().Add(handoffTestTimeout)); err != nil {
		t.Fatal(err)
	}
	var packet [stunTestPacketBytes]byte
	n, source, err := server.ReadFrom(packet[:])
	if err != nil {
		t.Fatal(err)
	}
	request := &stun.Message{Raw: packet[:n]}
	if err = request.Decode(); err != nil {
		t.Fatal(err)
	}
	address := source.(*net.UDPAddr)
	response, err := stun.Build(stun.NewTransactionIDSetter(request.TransactionID), stun.BindingSuccess,
		&stun.XORMappedAddress{IP: address.IP, Port: address.Port})
	if err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if _, err := server.WriteTo(response.Raw, source); err != nil {
			t.Fatal(err)
		}
	}
}
