// Package gateway implements bounded, egress-bound gateway control protocols.
package gateway

import (
	"context"
	"net"
	"net/netip"
	"time"
)

const controlPort = 5351
const maxDatagram = 1100

type Exchange func(context.Context, netip.Addr, netip.AddrPort, []byte) ([]byte, error)

// DatagramExchange uses a connected UDP socket so replies from a different
// gateway/source port cannot be mistaken for a control response.
func DatagramExchange(ctx context.Context, local netip.Addr, server netip.AddrPort, request []byte) ([]byte, error) {
	network := "udp6"
	if local.Is4() {
		network = "udp4"
	}
	dialer := net.Dialer{LocalAddr: net.UDPAddrFromAddrPort(netip.AddrPortFrom(local, 0))}
	conn, err := dialer.DialContext(ctx, network, server.String())
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err = conn.Write(request); err != nil {
		return nil, err
	}
	response := make([]byte, maxDatagram+1)
	n, err := conn.Read(response)
	if err != nil {
		return nil, err
	}
	return response[:n], nil
}
