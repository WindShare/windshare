package socketauthority

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/stun/v3"
)

// A bound socket may already have queued peer checks when a new mux takes
// ownership. The first read must not depend on constructor-return signaling:
// that would hide an incompletely published embedded mux from the race detector.
func TestUniversalUDPMuxPublishesBeforeReadingQueuedSTUN(t *testing.T) {
	message, err := stun.Build(stun.BindingRequest, stun.TransactionID, stun.NewUsername("old:attempt"))
	if err != nil {
		t.Fatal(err)
	}
	socket := &queuedSTUNSocket{
		packet:  message.Raw,
		drained: make(chan struct{}),
		closed:  make(chan struct{}),
	}
	mux := ice.NewUniversalUDPMuxDefault(ice.UniversalUDPMuxParams{UDPConn: socket})
	t.Cleanup(func() { _ = mux.Close() })
	select {
	case <-socket.drained:
	case <-time.After(time.Second):
		t.Fatal("queued STUN was not processed")
	}
	if err := mux.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-socket.closed:
	default:
		t.Fatal("mux did not close its owned socket")
	}
}

// Only the reader owns packet. The second ReadFrom acknowledges that the first
// packet traversed the universal wrapper, without ordering construction before it.
type queuedSTUNSocket struct {
	packet    []byte
	drained   chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
}

func (s *queuedSTUNSocket) ReadFrom(p []byte) (int, net.Addr, error) {
	if s.packet != nil {
		n := copy(p, s.packet)
		s.packet = nil
		return n, s.LocalAddr(), nil
	}
	close(s.drained)
	<-s.closed
	return 0, nil, net.ErrClosed
}
func (s *queuedSTUNSocket) WriteTo(p []byte, _ net.Addr) (int, error) { return len(p), nil }
func (s *queuedSTUNSocket) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}
func (*queuedSTUNSocket) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}
}
func (*queuedSTUNSocket) SetDeadline(time.Time) error      { return nil }
func (*queuedSTUNSocket) SetReadDeadline(time.Time) error  { return nil }
func (*queuedSTUNSocket) SetWriteDeadline(time.Time) error { return nil }
