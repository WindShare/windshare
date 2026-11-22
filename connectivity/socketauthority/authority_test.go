package socketauthority

import (
	"errors"
	"io"
	"net"
	"net/netip"
	"testing"

	"github.com/pion/logging"
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
	if _, err = authority.Acquire([16]byte{1}, 1, [16]byte{3}, addresses); !errors.Is(err, ErrCapacity) {
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
	if _, err = mux.GetConn("x", &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 1}); err == nil {
		t.Fatal("foreign endpoint accepted")
	}
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

type recordingMuxLogger struct {
	logging.LeveledLogger
	errors []string
	traces []string
}

func (l *recordingMuxLogger) Error(msg string)          { l.errors = append(l.errors, msg) }
func (l *recordingMuxLogger) Trace(msg string)          { l.traces = append(l.traces, msg) }
func (l *recordingMuxLogger) Errorf(f string, a ...any) { l.Error(f) }
func (l *recordingMuxLogger) Tracef(f string, a ...any) { l.Trace(f) }

func TestClosedPipeMuxWriteDowngradedToTrace(t *testing.T) {
	base := &recordingMuxLogger{}
	logger := closedPipeDropLogger{LeveledLogger: base}
	logger.Errorf("Failed to write packet: %v", io.ErrClosedPipe)
	logger.Errorf("Failed to write packet: %v", errors.New("real failure"))
	if len(base.traces) != 1 || len(base.errors) != 1 {
		t.Fatalf("traces=%q errors=%q", base.traces, base.errors)
	}
}
