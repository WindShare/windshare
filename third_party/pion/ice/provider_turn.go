// SPDX-FileCopyrightText: 2026 WindShare contributors
// SPDX-License-Identifier: MIT

package ice

import (
	"context"
	"crypto/tls"
	"net"
	"net/netip"
	"strconv"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/ice/v4/internal/fakenet"
	"github.com/pion/stun/v4"
	"github.com/pion/transport/v5"
	"github.com/pion/turn/v5"
)

const turnDiscardGrace = 250 * time.Millisecond

// TURNRoute separates relay egress from direct candidate filtering. A zero local
// address delegates egress selection to the operating system.
type TURNRoute struct {
	Server       stun.URI
	LocalAddress netip.Addr
}

// TURNAllocation retains the socket reservation until Discard or agent close.
type TURNAllocation struct {
	Address netip.AddrPort
	Discard func()
}

// TURNRouteAttempt transfers release to the allocation on success; otherwise it
// releases the attempt before returning. Callers reserve capacity before dialing.
type TURNRouteAttempt func(context.Context, TURNRoute, func()) *TURNAllocation

func (a *Agent) gatherTURNRoute(ctx context.Context, route TURNRoute, release func(), generation uint64) *TURNAllocation {
	transferred := false
	defer func() {
		if !transferred {
			release()
		}
	}()
	if ctx.Err() != nil {
		return nil
	}
	routeNet := a.net
	if scoped, ok := routeNet.(interface {
		WithContext(context.Context) transport.Net
	}); ok {
		routeNet = scoped.WithContext(ctx)
	}
	url := route.Server
	urlProto := effectiveURLProtoType(url)
	network := "udp4"
	if urlProto == stun.ProtoTypeTCP {
		network = "tcp4"
	}
	localBindAddr := "0.0.0.0:0"
	var localTCP *net.TCPAddr
	var localUDP *net.UDPAddr
	if route.LocalAddress.IsValid() {
		ip := net.IP(route.LocalAddress.AsSlice())
		localBindAddr = net.JoinHostPort(route.LocalAddress.String(), "0")
		localTCP = &net.TCPAddr{IP: ip}
		localUDP = &net.UDPAddr{IP: ip}
	}
	_, ifaces, _ := localInterfaces(a.net, nil, nil, a.networkTypes, a.includeLoopback)
	turnServerAddr := net.JoinHostPort(url.Host, strconv.Itoa(url.Port))
	var (
		locConn       net.PacketConn
		err           error
		relAddr       string
		relPort       int
		relayProtocol string
	)

	switch {
	case urlProto == stun.ProtoTypeUDP && url.Scheme == stun.SchemeTypeTURN:
		if locConn, err = routeNet.ListenPacket(network, localBindAddr); err != nil {
			a.log.Warnf("Failed to listen %s: %v", network, err)

			return nil
		}

		relAddr = locConn.LocalAddr().(*net.UDPAddr).IP.String() //nolint:forcetypeassert
		relPort = locConn.LocalAddr().(*net.UDPAddr).Port        //nolint:forcetypeassert
		relayProtocol = udp
	case a.proxyDialer != nil && urlProto == stun.ProtoTypeTCP &&
		(url.Scheme == stun.SchemeTypeTURN || url.Scheme == stun.SchemeTypeTURNS):
		conn, connectErr := a.proxyDialer.Dial(network, turnServerAddr)
		if connectErr != nil {
			a.log.Warnf("Failed to dial TCP address %s via proxy dialer: %v", turnServerAddr, connectErr)

			return nil
		}

		relAddr = conn.LocalAddr().(*net.TCPAddr).IP.String() //nolint:forcetypeassert
		relPort = conn.LocalAddr().(*net.TCPAddr).Port        //nolint:forcetypeassert
		if url.Scheme == stun.SchemeTypeTURN {
			relayProtocol = tcp
		} else if url.Scheme == stun.SchemeTypeTURNS {
			relayProtocol = "tls"
		}
		locConn = turn.NewSTUNConn(conn)

	case urlProto == stun.ProtoTypeTCP && url.Scheme == stun.SchemeTypeTURN:
		tcpAddr, connectErr := routeNet.ResolveTCPAddr(network, turnServerAddr)
		if connectErr != nil {
			a.log.Warnf("Failed to resolve TCP address %s: %v", turnServerAddr, connectErr)

			return nil
		}

		conn, connectErr := routeNet.DialTCP(network, localTCP, tcpAddr)
		if connectErr != nil {
			a.log.Warnf("Failed to dial TCP address %s: %v", turnServerAddr, connectErr)

			return nil
		}

		relAddr = conn.LocalAddr().(*net.TCPAddr).IP.String() //nolint:forcetypeassert
		relPort = conn.LocalAddr().(*net.TCPAddr).Port        //nolint:forcetypeassert
		relayProtocol = tcp
		locConn = turn.NewSTUNConn(conn)
	case urlProto == stun.ProtoTypeUDP && url.Scheme == stun.SchemeTypeTURNS:
		udpAddr, connectErr := routeNet.ResolveUDPAddr(network, turnServerAddr)
		if connectErr != nil {
			a.log.Warnf("Failed to resolve UDP address %s: %v", turnServerAddr, connectErr)

			return nil
		}

		udpConn, dialErr := routeNet.DialUDP(network, localUDP, udpAddr)
		if dialErr != nil {
			a.log.Warnf("Failed to dial DTLS address %s: %v", turnServerAddr, dialErr)

			return nil
		}

		conn, connectErr := dtls.ClientWithOptions(&fakenet.PacketConn{Conn: udpConn}, udpConn.RemoteAddr(),
			dtls.WithServerName(url.Host),
			dtls.WithInsecureSkipVerify(a.insecureSkipVerify), //nolint:gosec
			dtls.WithLoggerFactory(a.loggerFactory),
		)
		if connectErr != nil {
			a.log.Warnf("Failed to create DTLS client: %v", turnServerAddr, connectErr)
			if closeErr := udpConn.Close(); closeErr != nil {
				a.log.Errorf("Failed to close relay connection: %v", closeErr)
			}

			return nil
		}

		if connectErr = conn.HandshakeContext(ctx); connectErr != nil {
			a.log.Warnf("Failed to create DTLS client: %v", turnServerAddr, connectErr)
			if closeErr := conn.Close(); closeErr != nil {
				a.log.Errorf("Failed to close relay connection: %v", closeErr)
			}

			return nil
		}

		relAddr = conn.LocalAddr().(*net.UDPAddr).IP.String() //nolint:forcetypeassert
		relPort = conn.LocalAddr().(*net.UDPAddr).Port        //nolint:forcetypeassert
		relayProtocol = relayProtocolDTLS
		locConn = &fakenet.PacketConn{Conn: conn}
	case urlProto == stun.ProtoTypeTCP && url.Scheme == stun.SchemeTypeTURNS:
		tcpAddr, resolvErr := routeNet.ResolveTCPAddr(network, turnServerAddr)
		if resolvErr != nil {
			a.log.Warnf("Failed to resolve relay address %s: %v", turnServerAddr, resolvErr)

			return nil
		}

		tcpConn, dialErr := routeNet.DialTCP(network, localTCP, tcpAddr)
		if dialErr != nil {
			a.log.Warnf("Failed to connect to relay: %v", dialErr)

			return nil
		}

		conn := tls.Client(tcpConn, &tls.Config{
			ServerName:         url.Host,
			InsecureSkipVerify: a.insecureSkipVerify, //nolint:gosec
		})

		if hsErr := conn.HandshakeContext(ctx); hsErr != nil {
			if closeErr := tcpConn.Close(); closeErr != nil {
				a.log.Errorf("Failed to close relay connection: %v", closeErr)
			}
			a.log.Warnf("Failed to connect to relay: %v", hsErr)

			return nil
		}

		relAddr = conn.LocalAddr().(*net.TCPAddr).IP.String() //nolint:forcetypeassert
		relPort = conn.LocalAddr().(*net.TCPAddr).Port        //nolint:forcetypeassert
		relayProtocol = relayProtocolTLS
		locConn = turn.NewSTUNConn(conn)
	default:
		a.log.Warnf("Unable to handle URL in gatherCandidatesRelay %v", url)

		return nil
	}

	factory := a.turnClientFactory
	if factory == nil {
		factory = defaultTurnClient
	}

	client, err := factory(&turn.ClientConfig{
		TURNServerAddr:         turnServerAddr,
		Conn:                   locConn,
		Username:               url.Username,
		Password:               url.Password,
		LoggerFactory:          a.loggerFactory,
		OnBindingEvent:         a.providerConfig.OnTURNBinding,
		Net:                    routeNet,
		RequestedAddressFamily: requestedRelayAddressFamily(relayNetworkTypesForConfiguredCandidates(a.networkTypes)),
	})
	if err != nil {
		closeConnAndLog(locConn, a.log, "failed to create new TURN client %s %s", turnServerAddr, err)

		return nil
	}

	if err = client.Listen(); err != nil {
		client.Close()
		closeConnAndLog(locConn, a.log, "failed to listen on TURN client %s %s", turnServerAddr, err)

		return nil
	}

	relayConn, err := allocateRelay(ctx, client, locConn)
	if err != nil {
		client.Close()
		closeConnAndLog(locConn, a.log, "failed to allocate on TURN client %s %s", turnServerAddr, err)

		return nil
	}

	relayConn.(*turnAllocationConn).release = release
	transferred = true
	closeRelayConn := func() {
		if relayConErr := relayConn.Close(); relayConErr != nil {
			a.log.Warnf("Failed to close relay %v", relayConErr)
		}
	}

	rAddr := relayConn.LocalAddr().(*net.UDPAddr) //nolint:forcetypeassert
	if shouldFilterLocationTracked(rAddr.IP) {
		closeRelayConn()
		client.Close()
		closeConnAndLog(locConn, a.log,
			"TURN address %s is somehow filtered for location tracking reasons", rAddr.IP)

		return nil
	}

	// Relay allocations currently produce UDP relay endpoints regardless of
	// whether the TURN control connection uses UDP/TCP/TLS/DTLS.
	a.addRelayCandidates(ctx, generation, relayEndpoint{
		network:   udp,
		address:   rAddr.IP,
		port:      rAddr.Port,
		relAddr:   relAddr,
		relPort:   relPort,
		iface:     findIfaceForIP(ifaces, net.ParseIP(relAddr)),
		protocol:  relayProtocol,
		conn:      relayConn,
		onClose:   relayConn.Close,
		closeConn: closeRelayConn,
	})
	return &TURNAllocation{Address: rAddr.AddrPort(), Discard: func() {
		// A rejected candidate must not hold capacity indefinitely on a stalled
		// control transport. Deallocation still gets a bounded best-effort write.
		timer := time.AfterFunc(turnDiscardGrace, func() { _ = locConn.Close() })
		_ = relayConn.Close()
		timer.Stop()
	}}
}
