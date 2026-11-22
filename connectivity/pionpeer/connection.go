// Package pionpeer adapts one Pion PeerConnection to an immutable connectivity
// resource snapshot. Retry, mapping and network-generation ownership stay above it.
package pionpeer

import (
	"errors"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/logging"
	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/connectivity/socketauthority"
)

type MappedEndpoint struct {
	Local    netip.AddrPort
	External netip.AddrPort
	Protocol string
}

// AttemptConfig is a value snapshot; referenced lists are copied before creation.
type AttemptConfig struct {
	ProtocolSessionID   [16]byte
	PeerPathID          [16]byte
	AttemptID           [16]byte
	NetworkGenerationID uint64
	ICEProfileID        string
	ICE                 iceconfig.Connection
	SocketLease         *socketauthority.Lease
	MappedEndpoints     []MappedEndpoint
	Observe             func(Event)
	// allowLoopback is package-private so only local in-package ICE handshake
	// tests can opt into loopback sockets/candidates; production stays RFC 8445.
	allowLoopback bool
}

// The caller may reuse its configuration while this attempt is gathering.
func (request AttemptConfig) snapshot() AttemptConfig {
	request.MappedEndpoints = slices.Clone(request.MappedEndpoints)
	return request
}

type Event struct {
	ProtocolSessionID   [16]byte
	PeerPathID          [16]byte
	AttemptID           [16]byte
	NetworkGenerationID uint64
	ICEProfileID        string
	Milestone           string
	State               string
	Diagnostic          *Diagnostic
	At                  time.Time
	Candidate           *CandidateFacts
	Pair                *PairFacts
}

type Connection struct {
	*pion.PeerConnection
	lease     *socketauthority.Lease
	release   func()
	request   AttemptConfig
	mapped    []MappedEndpoint
	closeOnce sync.Once
	closeErr  error
}

// NewPeerConnection acquires an exclusive attempt reference. The caller's path
// lease must remain held if a future fresh attempt is already budgeted.
func NewPeerConnection(configuration pion.Configuration, request AttemptConfig) (*Connection, error) {
	if request.SocketLease == nil || request.ProtocolSessionID != request.SocketLease.SessionID() || request.NetworkGenerationID != request.SocketLease.GenerationID() || request.PeerPathID != request.SocketLease.PathID() {
		return nil, errors.New("provider socket identity mismatch")
	}
	request = request.snapshot()
	lease, err := request.SocketLease.Retain()
	if err != nil {
		return nil, err
	}
	endpoints := lease.Endpoints()
	if err := validateLeaseEndpoints(endpoints, request.allowLoopback); err != nil {
		_ = lease.Close()
		return nil, err
	}
	// Standard RFC 6544 ICE-TCP is optional and best-effort: listener capacity or
	// bind failures degrade cleanly to UDP-only without blocking connectivity.
	tcpFailure := lease.PrepareTCP()
	var tcpEndpoints []netip.AddrPort
	if tcpFailure == nil {
		tcpEndpoints = lease.TCPEndpoints()
	}
	mux, release, err := lease.Claim()
	if err != nil {
		_ = lease.Close()
		return nil, err
	}
	fail := func(err error) (*Connection, error) {
		release()
		_ = lease.Close()
		return nil, err
	}
	mapped, err := mapAttemptEndpoints(request.MappedEndpoints, endpoints, tcpEndpoints)
	if err != nil {
		return fail(err)
	}
	var settings pion.SettingEngine
	settings.SetICEUDPMux(mux)
	if request.allowLoopback {
		settings.SetIncludeLoopbackCandidate(true)
	}
	settings.SetIPFilter(localEndpointFilter(endpoints))
	settings.SetICEMulticastDNSMode(ice.MulticastDNSModeQueryOnly)
	configureNetworkTypes(&settings, lease, tcpEndpoints)
	configureICE(&configuration, request.ICE)
	connection := &Connection{lease: lease, release: release, request: request, mapped: mapped}
	settings.LoggerFactory = providerLoggerFactory{LoggerFactory: logging.NewDefaultLoggerFactory(), observe: connection.observeDiagnostic}
	pc, err := pion.NewAPI(pion.WithSettingEngine(settings)).NewPeerConnection(configuration)
	if err != nil {
		return fail(err)
	}
	connection.PeerConnection = pc
	pc.OnICEConnectionStateChange(func(state pion.ICEConnectionState) { connection.observe("ice", state.String()) })
	pc.OnConnectionStateChange(func(state pion.PeerConnectionState) { connection.observe("peerconnection", state.String()) })
	connection.OnICECandidate(nil)
	pc.SCTP().Transport().ICETransport().OnSelectedCandidatePairChange(func(pair *pion.ICECandidatePair) { connection.observeSelectedPair(pair) })
	if tcpFailure != nil {
		connection.observe("tcp_unavailable", tcpFailure.Error())
	}
	connection.observe("provider_created", "")
	return connection, nil
}

func configureNetworkTypes(settings *pion.SettingEngine, lease *socketauthority.Lease, tcpEndpoints []netip.AddrPort) {
	networks := []pion.NetworkType{pion.NetworkTypeUDP4, pion.NetworkTypeUDP6}
	var hasTCP4, hasTCP6 bool
	for _, endpoint := range tcpEndpoints {
		if endpoint.Addr().Is4() {
			hasTCP4 = true
		} else if endpoint.Addr().Is6() {
			hasTCP6 = true
		}
	}
	if hasTCP4 {
		networks = append(networks, pion.NetworkTypeTCP4)
	}
	if hasTCP6 {
		networks = append(networks, pion.NetworkTypeTCP6)
	}
	if hasTCP4 || hasTCP6 {
		// Keep Pion's default DisableActiveTCP(false): browsers emit only active
		// candidates (ignored by pion/ice) and dial our passive mux, while native
		// peers still dial each other's passive listeners on demand.
		settings.SetICETCPMux(lease.TCP())
	}
	settings.SetNetworkTypes(networks)
}

func validateLeaseEndpoints(endpoints []netip.AddrPort, allowLoopback bool) error {
	if allowLoopback {
		return nil
	}
	for _, endpoint := range endpoints {
		if endpoint.Addr().Unmap().IsLoopback() {
			return errors.New("loopback socket endpoint not permitted")
		}
	}
	return nil
}

func localEndpointFilter(endpoints []netip.AddrPort) func(net.IP) bool {
	return func(ip net.IP) bool {
		for _, endpoint := range endpoints {
			if net.IP(endpoint.Addr().AsSlice()).Equal(ip) {
				return true
			}
		}
		return false
	}
}

func configureICE(configuration *pion.Configuration, connection iceconfig.Connection) {
	configuration.ICEServers = nil
	if urls := connection.DiscoveryURLs(); len(urls) > 0 {
		configuration.ICEServers = []pion.ICEServer{{URLs: urls}}
	}
	configuration.ICETransportPolicy = pion.ICETransportPolicyAll
}

func (c *Connection) event(milestone, state string) Event {
	return Event{
		ProtocolSessionID: c.request.ProtocolSessionID, PeerPathID: c.request.PeerPathID, AttemptID: c.request.AttemptID,
		NetworkGenerationID: c.request.NetworkGenerationID, ICEProfileID: c.request.ICEProfileID,
		Milestone: milestone, State: state, At: time.Now(),
	}
}
func (c *Connection) observe(milestone, state string) {
	if c.request.Observe != nil {
		c.request.Observe(c.event(milestone, state))
	}
}

// These wrappers preserve provider observations when consumers install their
// own callbacks. A PC state is recorded as itself, never as an invented DTLS fact.
func (c *Connection) OnICEConnectionStateChange(callback func(pion.ICEConnectionState)) {
	c.PeerConnection.OnICEConnectionStateChange(func(state pion.ICEConnectionState) {
		c.observe("ice", state.String())
		if callback != nil {
			callback(state)
		}
	})
}
func (c *Connection) OnConnectionStateChange(callback func(pion.PeerConnectionState)) {
	c.PeerConnection.OnConnectionStateChange(func(state pion.PeerConnectionState) {
		c.observe("peerconnection", state.String())
		if callback != nil {
			callback(state)
		}
	})
}
func (c *Connection) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = c.PeerConnection.Close()
		c.release()
		c.closeErr = errors.Join(c.closeErr, c.lease.Close())
		c.observe("provider_closed", "")
	})
	return c.closeErr
}

// PairFacts reports the actual selected pair; caller still owns authenticated
// lane admission and must not infer directness from gathered candidates.
type PairFacts struct {
	LocalType     string
	RemoteType    string
	Protocol      string
	LocalAddress  string
	RemoteAddress string
	LocalPort     uint16
	RemotePort    uint16
	RoundTripTime time.Duration
}

func (c *Connection) SelectedPair() (PairFacts, bool) {
	transport := c.SCTP().Transport().ICETransport()
	pair, err := transport.GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return PairFacts{}, false
	}
	facts := PairFacts{
		LocalType: pair.Local.Typ.String(), RemoteType: pair.Remote.Typ.String(),
		Protocol: pair.Local.Protocol.String(), LocalAddress: pair.Local.Address, RemoteAddress: pair.Remote.Address,
		LocalPort: pair.Local.Port, RemotePort: pair.Remote.Port,
	}
	if stats, ok := transport.GetSelectedCandidatePairStats(); ok {
		facts.RoundTripTime = time.Duration(stats.CurrentRoundTripTime * float64(time.Second))
	}
	return facts, true
}

func mapAttemptEndpoints(requested []MappedEndpoint, endpoints, tcpEndpoints []netip.AddrPort) ([]MappedEndpoint, error) {
	mapped := make([]MappedEndpoint, 0, len(requested))
	for _, endpoint := range requested {
		protocol := endpoint.Protocol
		if protocol == "" {
			protocol = "udp"
		}
		bases := endpoints
		switch protocol {
		case "tcp":
			if len(tcpEndpoints) == 0 {
				continue
			}
			bases = tcpEndpoints
		case "udp":
		default:
			return nil, errors.New("unsupported mapped endpoint protocol")
		}
		if !slices.Contains(bases, endpoint.Local) || !endpoint.External.IsValid() || endpoint.External.Port() == 0 ||
			endpoint.External.Addr().Is4() != endpoint.Local.Addr().Is4() || endpoint.External == endpoint.Local {
			return nil, errors.New("mapped endpoint does not belong to attempt socket")
		}
		mapped = append(mapped, MappedEndpoint{
			Local:    netip.AddrPortFrom(endpoint.Local.Addr().Unmap(), endpoint.Local.Port()),
			External: netip.AddrPortFrom(endpoint.External.Addr().Unmap(), endpoint.External.Port()),
			Protocol: protocol,
		})
	}
	return mapped, nil
}
