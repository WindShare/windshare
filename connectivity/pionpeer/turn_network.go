package pionpeer

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/transport/v4"
	"github.com/pion/transport/v4/stdnet"
)

const (
	TURNDialTimeout = 10 * time.Second
	// TURN deallocation is sent without waiting for a response. This grace
	// bounds stalled writes/gathering without imposing a normal close delay.
	TURNCloseGrace = 250 * time.Millisecond
)

// Transport counters include TURN framing, TLS and protocol retries visible to
// the process. They are not provider billing or kernel TCP retransmission counts.
type TURNResourceSnapshot struct {
	ActiveSockets     int
	OpenedSockets     uint64
	TransportSent     uint64
	TransportReceived uint64
}

type turnNetwork struct {
	transport.Net
	ctx            context.Context
	cancel         context.CancelFunc
	mu             sync.Mutex
	stopping       bool
	closeOnce      sync.Once
	opening        sync.WaitGroup
	capacity       socketReservation
	sockets        map[netCloser]struct{}
	opened         uint64
	reserved       int
	sent, received atomic.Uint64
}

type netCloser interface{ Close() error }

// Socket ownership ends only after protocol shutdown has finished or timed out.
type socketReservation interface {
	SocketLimit() int
	Close()
}

var errTURNSocketLimit = errors.New("TURN socket reservation exhausted")

func claimTURNNetwork(request AttemptConfig) (*turnNetwork, error) {
	relay, relayed := request.ICE.Relay()
	if !relayed {
		if request.TURN != nil {
			return nil, errTURNConfiguration
		}
		return nil, nil
	}
	if request.TURN == nil || request.TURN.Reservation == nil || !request.TURN.Plan.Matches(relay) ||
		request.TURN.Plan.InitialSockets() != request.TURN.Reservation.SocketLimit() {
		return nil, errTURNConfiguration
	}
	if err := request.TURN.Reservation.Claim(request.ProtocolSessionID, request.PeerPathID, relay.Identity()); err != nil {
		return nil, err
	}
	return newTURNNetwork(request.TURN.Reservation)
}

func newTURNNetwork(capacity socketReservation) (*turnNetwork, error) {
	base, err := stdnet.NewNet()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &turnNetwork{Net: base, ctx: ctx, cancel: cancel, capacity: capacity, sockets: make(map[netCloser]struct{})}, nil
}

func (n *turnNetwork) reserve() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stopping {
		return net.ErrClosed
	}
	if n.reserved >= n.capacity.SocketLimit() {
		return errTURNSocketLimit
	}
	// The reservation also covers sockets whose OS dial has not completed.
	n.reserved++
	n.opening.Add(1)
	return nil
}

func (n *turnNetwork) release() {
	n.mu.Lock()
	n.reserved--
	n.mu.Unlock()
}

func (n *turnNetwork) register(socket netCloser) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stopping {
		return net.ErrClosed
	}
	n.sockets[socket] = struct{}{}
	n.opened++
	return nil
}

func (n *turnNetwork) remove(socket netCloser) {
	n.mu.Lock()
	delete(n.sockets, socket)
	n.mu.Unlock()
	n.release()
}

func (n *turnNetwork) ListenPacket(network, address string) (net.PacketConn, error) {
	return n.listenPacket(n.ctx, network, address)
}
func (n *turnNetwork) listenPacket(ctx context.Context, network, address string) (net.PacketConn, error) {
	if err := n.reserve(); err != nil {
		return nil, err
	}
	defer n.opening.Done()
	var config net.ListenConfig
	socket, err := config.ListenPacket(ctx, network, address)
	if err != nil {
		n.release()
		return nil, err
	}
	wrapped := &turnPacket{PacketConn: socket, owner: n}
	if err = n.register(wrapped); err != nil {
		_ = socket.Close()
		n.release()
		return nil, err
	}
	return wrapped, nil
}

func (n *turnNetwork) DialTCP(network string, local, remote *net.TCPAddr) (transport.TCPConn, error) {
	return n.dialTCP(n.ctx, network, local, remote)
}
func (n *turnNetwork) dialTCP(ctx context.Context, network string, local, remote *net.TCPAddr) (transport.TCPConn, error) {
	if err := n.reserve(); err != nil {
		return nil, err
	}
	defer n.opening.Done()
	dialer := net.Dialer{Timeout: TURNDialTimeout}
	if local != nil {
		dialer.LocalAddr = local
	}
	socket, err := dialer.DialContext(ctx, network, remote.String())
	if err != nil {
		n.release()
		return nil, err
	}
	tcp, ok := socket.(*net.TCPConn)
	if !ok {
		_ = socket.Close()
		n.release()
		return nil, errors.New("TURN dial did not return a TCP socket")
	}
	wrapped := &turnTCP{TCPConn: tcp, owner: n}
	if err = n.register(wrapped); err != nil {
		_ = socket.Close()
		n.release()
		return nil, err
	}
	return wrapped, nil
}

func (n *turnNetwork) ResolveTCPAddr(network, address string) (*net.TCPAddr, error) {
	return n.resolveTCPAddr(n.ctx, network, address)
}
func (n *turnNetwork) resolveTCPAddr(ctx context.Context, network, address string) (*net.TCPAddr, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := net.DefaultResolver.LookupPort(ctx, network, portText)
	if err != nil {
		return nil, err
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
	if err != nil {
		return nil, err
	}
	if len(addresses) == 0 {
		return nil, errors.New("TURN endpoint has no IPv4 address")
	}
	return &net.TCPAddr{IP: addresses[0].AsSlice(), Port: port}, nil
}

func (n *turnNetwork) ResolveUDPAddr(network, address string) (*net.UDPAddr, error) {
	// TURN's client resolves UDP again after opening its socket. Use the same
	// cancellable IPv4 resolver as TCP instead of stdnet's unbounded resolver.
	resolved, err := n.ResolveTCPAddr("udp", address)
	if err != nil {
		return nil, err
	}
	return &net.UDPAddr{IP: resolved.IP, Port: resolved.Port}, nil
}

func (n *turnNetwork) stopOpening() {
	n.mu.Lock()
	n.stopping = true
	n.cancel()
	n.mu.Unlock()
}

// shutdown keeps established sockets writable while Pion sends allocation
// deletion. DNS/dials stop immediately; stuck protocol work gets a bounded abort.
func (n *turnNetwork) shutdown(closeProtocol func() error) (error, bool) {
	n.stopOpening()
	forced := make(chan struct{})
	timer := time.AfterFunc(TURNCloseGrace, func() {
		n.close()
		close(forced)
	})
	err := closeProtocol()
	expired := !timer.Stop()
	if expired {
		<-forced
	}
	n.close()
	return err, expired
}

func (n *turnNetwork) close() {
	n.closeOnce.Do(func() {
		n.stopOpening()
		n.opening.Wait()
		n.mu.Lock()
		sockets := make([]netCloser, 0, len(n.sockets))
		for socket := range n.sockets {
			sockets = append(sockets, socket)
		}
		n.mu.Unlock()
		for _, socket := range sockets {
			_ = socket.Close()
		}
		n.capacity.Close()
	})
}

func (n *turnNetwork) snapshot() TURNResourceSnapshot {
	n.mu.Lock()
	defer n.mu.Unlock()
	return TURNResourceSnapshot{ActiveSockets: len(n.sockets), OpenedSockets: n.opened,
		TransportSent: n.sent.Load(), TransportReceived: n.received.Load()}
}

func (c *Connection) TURNResources() (TURNResourceSnapshot, bool) {
	if c.turn == nil {
		return TURNResourceSnapshot{}, false
	}
	return c.turn.snapshot(), true
}

type turnPacket struct {
	net.PacketConn
	owner *turnNetwork
	once  sync.Once
	err   error
}

func (s *turnPacket) ReadFrom(buffer []byte) (int, net.Addr, error) {
	count, address, err := s.PacketConn.ReadFrom(buffer)
	s.owner.received.Add(uint64(count))
	return count, address, err
}
func (s *turnPacket) WriteTo(buffer []byte, address net.Addr) (int, error) {
	count, err := s.PacketConn.WriteTo(buffer, address)
	s.owner.sent.Add(uint64(count))
	return count, err
}
func (s *turnPacket) Close() error {
	s.once.Do(func() { s.err = s.PacketConn.Close(); s.owner.remove(s) })
	return s.err
}

type turnTCP struct {
	*net.TCPConn
	owner *turnNetwork
	once  sync.Once
	err   error
}

func (s *turnTCP) Read(buffer []byte) (int, error) {
	count, err := s.TCPConn.Read(buffer)
	s.owner.received.Add(uint64(count))
	return count, err
}
func (s *turnTCP) Write(buffer []byte) (int, error) {
	count, err := s.TCPConn.Write(buffer)
	s.owner.sent.Add(uint64(count))
	return count, err
}
func (s *turnTCP) Close() error {
	s.once.Do(func() { s.err = s.TCPConn.Close(); s.owner.remove(s) })
	return s.err
}
