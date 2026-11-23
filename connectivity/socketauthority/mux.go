package socketauthority

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/logging"
)

// Mux routes each local interface/family to its own physical endpoint. It is
// immutable once published; only Authority closes it.
type Mux struct{ endpoints []*ice.UniversalUDPMuxDefault }

func (m *Mux) endpoint(addr net.Addr) (*ice.UniversalUDPMuxDefault, error) {
	for _, endpoint := range m.endpoints {
		if endpoint.GetListenAddresses()[0].String() == addr.String() {
			return endpoint, nil
		}
	}
	return nil, fmt.Errorf("socket endpoint unavailable: %s", addr)
}

func (m *Mux) DiscoverXORMappedAddr(ctx context.Context, local netip.AddrPort, server *net.UDPAddr, timeout time.Duration) (netip.AddrPort, error) {
	endpoint, err := m.endpoint(net.UDPAddrFromAddrPort(local))
	if err != nil {
		return netip.AddrPort{}, err
	}
	mapped, err := endpoint.GetXORMappedAddrContext(ctx, server, timeout)
	if err != nil {
		return netip.AddrPort{}, err
	}
	return stunMappedEndpoint(local.Addr(), mapped.IP, mapped.Port)
}

func stunMappedEndpoint(local netip.Addr, mappedIP net.IP, port int) (netip.AddrPort, error) {
	ip, ok := netip.AddrFromSlice(mappedIP)
	ip = ip.Unmap()
	// A reflexive address has no interface scope on the wire. Private unicast
	// can work across a LAN or VPN. The peer decides whether to admit loopback.
	if !ok || port < 1 || port > math.MaxUint16 || ip.Is4() != local.Unmap().Is4() ||
		(!ip.IsGlobalUnicast() && !ip.IsLoopback()) {
		return netip.AddrPort{}, ErrInvalid
	}
	return netip.AddrPortFrom(ip, uint16(port)), nil
}

func (m *Mux) GetConn(ufrag string, addr net.Addr) (net.PacketConn, error) {
	endpoint, err := m.endpoint(addr)
	if err != nil {
		return nil, err
	}
	return endpoint.GetConn(ufrag, addr)
}

func (m *Mux) RemoveConnByUfrag(ufrag string) {
	for _, endpoint := range m.endpoints {
		endpoint.RemoveConnByUfrag(ufrag)
	}
}

func (m *Mux) GetListenAddresses() []net.Addr {
	var result []net.Addr
	for _, endpoint := range m.endpoints {
		result = append(result, endpoint.GetListenAddresses()...)
	}
	return result
}

func (m *Mux) Close() error {
	var err error
	for _, endpoint := range m.endpoints {
		err = errors.Join(err, endpoint.Close())
	}
	return err
}

// closedPipeDropLogger downgrades routine late packets arriving on a closing
// UDPMux connection from error to trace without patching upstream pion/ice.
type closedPipeDropLogger struct {
	logging.LeveledLogger
}

func newUDPMuxLogger() logging.LeveledLogger {
	return closedPipeDropLogger{LeveledLogger: logging.NewDefaultLoggerFactory().NewLogger("ice")}
}

func isClosedPipeMuxWrite(message string) bool {
	return strings.Contains(message, "Failed to write packet:") && strings.Contains(message, io.ErrClosedPipe.Error())
}

func (l closedPipeDropLogger) Error(message string) {
	if isClosedPipeMuxWrite(message) {
		l.LeveledLogger.Trace(message)
		return
	}
	l.LeveledLogger.Error(message)
}

func (l closedPipeDropLogger) Errorf(format string, args ...any) {
	l.Error(fmt.Sprintf(format, args...))
}
