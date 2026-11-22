package socketauthority

import (
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

const shutdownTestTimeout = 2 * time.Second

type closingTestSocket struct {
	closed    chan struct{}
	closeGate <-chan struct{}
	closeOnce sync.Once
}

func newClosingTestSocket(gate <-chan struct{}) *closingTestSocket {
	return &closingTestSocket{closed: make(chan struct{}), closeGate: gate}
}
func (s *closingTestSocket) ReadFrom([]byte) (int, net.Addr, error) {
	<-s.closed
	return 0, nil, net.ErrClosed
}
func (*closingTestSocket) WriteTo(p []byte, _ net.Addr) (int, error) { return len(p), nil }
func (s *closingTestSocket) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	if s.closeGate != nil {
		<-s.closeGate
	}
	return nil
}
func (*closingTestSocket) LocalAddr() net.Addr {
	return net.UDPAddrFromAddrPort(netip.MustParseAddrPort("127.0.0.1:12345"))
}
func (*closingTestSocket) SetDeadline(time.Time) error      { return nil }
func (*closingTestSocket) SetReadDeadline(time.Time) error  { return nil }
func (*closingTestSocket) SetWriteDeadline(time.Time) error { return nil }

func receiveShutdown[T any](t *testing.T, result <-chan T) T {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-time.After(shutdownTestTimeout):
		t.Fatal("socket operation did not complete")
		var zero T
		return zero
	}
}

func runAsync[T any](f func() T) <-chan T {
	done := make(chan T, 1)
	go func() { done <- f() }()
	return done
}

func TestClosingPathRetainsCapacityAndAllCloseCallersJoin(t *testing.T) {
	closeGate := make(chan struct{})
	socket := newClosingTestSocket(closeGate)
	var unblocked sync.Once
	unblock := func() { unblocked.Do(func() { close(closeGate) }) }
	defer unblock()
	a := New(Config{Capacity: 1, ListenPacket: func(string, string) (net.PacketConn, error) { return socket, nil }})
	t.Cleanup(func() { _ = a.Close() })
	lease, err := a.Acquire([16]byte{1}, 1, [16]byte{2}, []netip.Addr{netip.MustParseAddr("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	closing := runAsync(lease.Close)
	receiveShutdown(t, socket.closed)
	if err := receiveShutdown(t, runAsync(func() error {
		_, err := a.Acquire([16]byte{1}, 1, [16]byte{3}, []netip.Addr{netip.MustParseAddr("127.0.0.1")})
		return err
	})); !errors.Is(err, ErrCapacity) {
		t.Fatalf("closing socket stopped counting against capacity: %v", err)
	}
	repeated := runAsync(lease.Close)
	all := runAsync(a.Close)
	receiveShutdown(t, runAsync(func() struct{} {
		a.Retire(1)
		return struct{}{}
	}))
	select {
	case <-closing:
		t.Fatal("lease close returned while socket close was blocked")
	case <-repeated:
		t.Fatal("repeated close skipped the join")
	default:
	}
	unblock()
	for _, result := range []<-chan error{closing, repeated, all} {
		if err := receiveShutdown(t, result); err != nil {
			t.Fatal(err)
		}
	}
	if a.socketCount != 0 || len(a.paths) != 0 {
		t.Fatal("closed resources retained capacity")
	}
}

func TestAcquireReplacesClosingPathOnlyAfterSocketCloses(t *testing.T) {
	closeGate := make(chan struct{})
	first := newClosingTestSocket(closeGate)
	second := newClosingTestSocket(nil)
	var unblocked sync.Once
	unblock := func() { unblocked.Do(func() { close(closeGate) }) }
	defer unblock()
	calls := 0
	a := New(Config{Capacity: 1, ListenPacket: func(string, string) (net.PacketConn, error) {
		calls++
		if calls == 1 {
			return first, nil
		}
		return second, nil
	}})
	t.Cleanup(func() { _ = a.Close() })
	addresses := []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	lease, err := a.Acquire([16]byte{1}, 1, [16]byte{2}, addresses)
	if err != nil {
		t.Fatal(err)
	}
	closing := runAsync(lease.Close)
	receiveShutdown(t, first.closed)
	replacement := runAsync(func() *Lease {
		next, acquireErr := a.Acquire([16]byte{1}, 1, [16]byte{2}, addresses)
		if acquireErr != nil {
			return nil
		}
		return next
	})
	if err := receiveShutdown(t, runAsync(func() error {
		_, acquireErr := a.Acquire([16]byte{1}, 1, [16]byte{3}, addresses)
		return acquireErr
	})); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	unblock()
	if err := receiveShutdown(t, closing); err != nil {
		t.Fatal(err)
	}
	next := receiveShutdown(t, replacement)
	if next == nil || next.entry == lease.entry || calls != 2 {
		t.Fatal("acquisition reused a closing socket")
	}
}
