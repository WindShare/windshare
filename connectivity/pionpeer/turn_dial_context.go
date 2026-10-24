package pionpeer

import (
	"context"
	"net"

	"github.com/pion/transport/v4"
)

type turnDialContext struct {
	*turnNetwork
	ctx context.Context
}

// Each route may stop independently after another route wins. Established
// sockets remain owned by their allocation and are not tied to the dial context.
func (n *turnNetwork) WithContext(ctx context.Context) transport.Net {
	return &turnDialContext{turnNetwork: n, ctx: ctx}
}

func (n *turnDialContext) operation() (context.Context, func()) {
	ctx, cancel := context.WithCancel(n.ctx)
	stop := context.AfterFunc(n.turnNetwork.ctx, cancel)
	return ctx, func() { stop(); cancel() }
}

func (n *turnDialContext) ListenPacket(network, address string) (net.PacketConn, error) {
	ctx, stop := n.operation()
	defer stop()
	return n.listenPacket(ctx, network, address)
}

func (n *turnDialContext) DialTCP(network string, local, remote *net.TCPAddr) (transport.TCPConn, error) {
	ctx, stop := n.operation()
	defer stop()
	return n.dialTCP(ctx, network, local, remote)
}

func (n *turnDialContext) ResolveTCPAddr(network, address string) (*net.TCPAddr, error) {
	ctx, stop := n.operation()
	defer stop()
	return n.resolveTCPAddr(ctx, network, address)
}

func (n *turnDialContext) ResolveUDPAddr(network, address string) (*net.UDPAddr, error) {
	resolved, err := n.ResolveTCPAddr(network, address)
	if err != nil {
		return nil, err
	}
	return &net.UDPAddr{IP: resolved.IP, Port: resolved.Port}, nil
}
