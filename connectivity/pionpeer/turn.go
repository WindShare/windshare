package pionpeer

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"

	"github.com/pion/transport/v5"
	"github.com/pion/transport/v5/stdnet"
	pion "github.com/pion/webrtc/v4"

	"github.com/windshare/windshare/connectivity/iceconfig"
)

const maximumNetworkPort = 65535

type turnDialHooks struct {
	dialContext func(ctx context.Context, network string, localAddr net.Addr, address string) (net.Conn, error)
	lookupIP    func(ctx context.Context, network, host string) ([]net.IP, error)
	lookupPort  func(ctx context.Context, network, service string) (int, error)
}

func defaultTURNDialHooks() turnDialHooks {
	return turnDialHooks{
		dialContext: func(ctx context.Context, network string, localAddr net.Addr, address string) (net.Conn, error) {
			dialer := net.Dialer{LocalAddr: localAddr}
			return dialer.DialContext(ctx, network, address)
		},
		lookupIP:   net.DefaultResolver.LookupIP,
		lookupPort: net.DefaultResolver.LookupPort,
	}
}

// TURN uses the system route to the allocation server. Direct interface
// filters, gateway maps and leased host sockets do not apply to this transport.
// Dialing and DNS resolution stay bound to ctx and to Connection.Close so an
// unresponsive TCP/TLS TURN address cannot hold teardown until the OS timeout.
func NewTURNPeerConnection(ctx context.Context, base pion.Configuration, relay iceconfig.TURNRelay, identity AttemptConfig) (*Connection, error) {
	return newTURNPeerConnection(ctx, base, relay, identity, defaultTURNDialHooks())
}

func newTURNPeerConnection(ctx context.Context, base pion.Configuration, relay iceconfig.TURNRelay, identity AttemptConfig, hooks turnDialHooks) (*Connection, error) {
	urls, user, password := relay.Credentials()
	if len(urls) == 0 {
		return nil, iceconfig.ErrConfiguration
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, context.Cause(ctx)
	}
	dialCtx, cancelDial := context.WithCancel(ctx)
	dialNet, err := newTURNDialNet(dialCtx, hooks)
	if err != nil {
		cancelDial()
		return nil, err
	}
	base.ICEServers = []pion.ICEServer{{URLs: urls, Username: user, Credential: password, CredentialType: pion.ICECredentialTypePassword}}
	base.ICETransportPolicy = pion.ICETransportPolicyRelay
	var settings pion.SettingEngine
	settings.SetNet(dialNet)
	pc, err := pion.NewAPI(pion.WithSettingEngine(settings)).NewPeerConnection(base)
	if err != nil {
		cancelDial()
		return nil, err
	}
	c := &Connection{PeerConnection: pc, request: identity, beforeClose: cancelDial, release: func() {}}
	c.OnICEConnectionStateChange(nil)
	c.OnConnectionStateChange(nil)
	pc.SCTP().Transport().ICETransport().OnSelectedCandidatePairChange(c.observeSelectedPair)
	c.observe("provider_created", "turn")
	return c, nil
}

// turnDialNet replaces stdnet's uncancelable DialTCP and address lookups with
// context-aware calls while preserving system interface enumeration and UDP I/O.
type turnDialNet struct {
	*stdnet.Net
	ctx   context.Context
	hooks turnDialHooks
}

func newTURNDialNet(ctx context.Context, hooks turnDialHooks) (*turnDialNet, error) {
	base, err := stdnet.NewNet()
	if err != nil {
		return nil, err
	}
	defaults := defaultTURNDialHooks()
	if hooks.dialContext == nil {
		hooks.dialContext = defaults.dialContext
	}
	if hooks.lookupIP == nil {
		hooks.lookupIP = defaults.lookupIP
	}
	if hooks.lookupPort == nil {
		hooks.lookupPort = defaults.lookupPort
	}
	return &turnDialNet{Net: base, ctx: ctx, hooks: hooks}, nil
}

func (n *turnDialNet) Dial(network, address string) (net.Conn, error) {
	if err := n.ctx.Err(); err != nil {
		return nil, context.Cause(n.ctx)
	}
	return n.hooks.dialContext(n.ctx, network, nil, address)
}

func (n *turnDialNet) DialTCP(network string, laddr, raddr *net.TCPAddr) (transport.TCPConn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, net.UnknownNetworkError(network)
	}
	if err := n.ctx.Err(); err != nil {
		return nil, context.Cause(n.ctx)
	}
	if raddr == nil {
		return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("missing address")}
	}
	// Passing a typed-nil *net.TCPAddr into net.Dialer.LocalAddr produces a non-nil net.Addr.
	var localAddr net.Addr
	if laddr != nil {
		localAddr = laddr
	}
	conn, err := n.hooks.dialContext(n.ctx, network, localAddr, raddr.String())
	if err != nil {
		return nil, err
	}
	tcpConn, ok := conn.(transport.TCPConn)
	if !ok {
		_ = conn.Close()
		return nil, errors.New("TURN dialer did not return a TCP connection")
	}
	return tcpConn, nil
}

func (n *turnDialNet) ResolveTCPAddr(network, address string) (*net.TCPAddr, error) {
	switch network {
	case "":
		network = "tcp"
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, net.UnknownNetworkError(network)
	}
	ip, port, zone, err := n.resolveAddr(network, address)
	if err != nil {
		return nil, err
	}
	return &net.TCPAddr{IP: ip, Port: port, Zone: zone}, nil
}

func (n *turnDialNet) ResolveUDPAddr(network, address string) (*net.UDPAddr, error) {
	switch network {
	case "":
		network = "udp"
	case "udp", "udp4", "udp6":
	default:
		return nil, net.UnknownNetworkError(network)
	}
	ip, port, zone, err := n.resolveAddr(network, address)
	if err != nil {
		return nil, err
	}
	return &net.UDPAddr{IP: ip, Port: port, Zone: zone}, nil
}

func (n *turnDialNet) resolveAddr(network, address string) (net.IP, int, string, error) {
	if err := n.ctx.Err(); err != nil {
		return nil, 0, "", context.Cause(n.ctx)
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, 0, "", err
	}
	port, err := n.resolvePort(network, portText)
	if err != nil {
		return nil, 0, "", err
	}
	if host == "" {
		return nil, port, "", nil
	}
	if parsed, err := netip.ParseAddr(host); err == nil {
		ip, zone, err := literalAddressForNetwork(network, host, parsed)
		if err != nil {
			return nil, 0, "", err
		}
		return ip, port, zone, nil
	}
	ips, err := n.hooks.lookupIP(n.ctx, ipLookupNetwork(network), host)
	if err != nil {
		return nil, 0, "", err
	}
	if len(ips) == 0 {
		return nil, 0, "", &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return ips[0], port, "", nil
}

func (n *turnDialNet) resolvePort(network, portText string) (int, error) {
	if portText == "" {
		return 0, nil
	}
	if port, err := strconv.Atoi(portText); err == nil {
		if port < 0 || port > maximumNetworkPort {
			return 0, &net.AddrError{Err: "invalid port", Addr: portText}
		}
		return port, nil
	}
	return n.hooks.lookupPort(n.ctx, network, portText)
}

func literalAddressForNetwork(network, rawHost string, parsed netip.Addr) (net.IP, string, error) {
	switch ipLookupNetwork(network) {
	case "ip4":
		if !parsed.Is4() && !parsed.Is4In6() {
			return nil, "", &net.AddrError{Err: "no suitable address found", Addr: rawHost}
		}
		return net.IP(parsed.Unmap().AsSlice()), "", nil
	case "ip6":
		if !parsed.Is6() {
			return nil, "", &net.AddrError{Err: "no suitable address found", Addr: rawHost}
		}
		return net.IP(parsed.AsSlice()), parsed.Zone(), nil
	default:
		return net.IP(parsed.AsSlice()), parsed.Zone(), nil
	}
}

func ipLookupNetwork(network string) string {
	switch network {
	case "tcp4", "udp4":
		return "ip4"
	case "tcp6", "udp6":
		return "ip6"
	default:
		return "ip"
	}
}
