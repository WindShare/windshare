package pionpeer

import (
	"net/netip"
	"sync"

	"github.com/pion/ice/v4"
	pion "github.com/pion/webrtc/v4"
)

type CandidateFacts struct {
	Priority uint32
	TCPType  string
	Type     string
	Protocol string
	Address  string
	Port     uint16
	Family   string
	Origin   string
}

// IsMappedCandidate matches a candidate to the trusted immutable lease snapshot;
// srflx or passive TCP syntax alone can never grant mapped-candidate provenance.
func (c *Connection) IsMappedCandidate(raw string) bool {
	candidate, err := ice.UnmarshalCandidate(raw)
	if err != nil || candidate.Port() < 1 || candidate.Port() > 65535 {
		return false
	}
	address, err := netip.ParseAddr(candidate.Address())
	if err != nil {
		return false
	}
	addrPort := netip.AddrPortFrom(address.Unmap(), uint16(candidate.Port()))
	for _, endpoint := range c.mapped {
		if candidate.NetworkType().NetworkShort() != endpoint.Protocol || endpoint.External != addrPort {
			continue
		}
		if endpoint.Protocol == "tcp" {
			if candidate.Type() == ice.CandidateTypeHost && candidate.TCPType() == ice.TCPTypePassive {
				return true
			}
			continue
		}
		if candidate.Type() != ice.CandidateTypeServerReflexive {
			continue
		}
		related := candidate.RelatedAddress()
		if related != nil && related.Address == endpoint.Local.Addr().String() && related.Port == int(endpoint.Local.Port()) {
			return true
		}
	}
	return false
}

// OnICECandidate emits upstream gathered candidates and synthesizes verified
// router-mapped external endpoints alongside their matching local host sockets
// so UPnP/PCP/NAT-PMP reachability works without forking pion/ice or pion/webrtc.
func (c *Connection) OnICECandidate(callback func(*pion.ICECandidate)) {
	var mu sync.Mutex
	emitted := make([]bool, len(c.mapped))
	c.PeerConnection.OnICECandidate(func(candidate *pion.ICECandidate) {
		c.observeCandidate(candidate)
		if callback != nil {
			callback(candidate)
		}
		if candidate == nil || candidate.Typ != pion.ICECandidateTypeHost || len(c.mapped) == 0 {
			return
		}
		addr, err := netip.ParseAddr(candidate.Address)
		if err != nil {
			return
		}
		hostEndpoint := netip.AddrPortFrom(addr.Unmap(), candidate.Port)
		protocol := candidate.Protocol.String()
		mu.Lock()
		var synthesized []*pion.ICECandidate
		for i, endpoint := range c.mapped {
			if emitted[i] || endpoint.Protocol != protocol || endpoint.Local != hostEndpoint {
				continue
			}
			emitted[i] = true
			if extra, ok := synthesizeMappedCandidate(endpoint, candidate.SDPMid, candidate.SDPMLineIndex); ok {
				synthesized = append(synthesized, extra)
			}
		}
		mu.Unlock()
		for _, extra := range synthesized {
			c.observeCandidate(extra)
			if callback != nil {
				callback(extra)
			}
		}
	})
}

func synthesizeMappedCandidate(endpoint MappedEndpoint, sdpMid string, sdpMLineIndex uint16) (*pion.ICECandidate, bool) {
	if endpoint.Protocol == "tcp" {
		network := "tcp4"
		if endpoint.External.Addr().Is6() {
			network = "tcp6"
		}
		// Upstream pion/ice and pion/webrtc drop tcptype and resolve srflx as UDP.
		// Advertising the router-forwarded passive listener as a passive TCP host
		// candidate keeps RFC 6544 interoperability with both browsers and upstream Pion.
		iceCandidate, err := ice.NewCandidateHost(&ice.CandidateHostConfig{
			Network:   network,
			Address:   endpoint.External.Addr().String(),
			Port:      int(endpoint.External.Port()),
			Component: ice.ComponentRTP,
			TCPType:   ice.TCPTypePassive,
		})
		if err != nil {
			return nil, false
		}
		return &pion.ICECandidate{
			Foundation:    iceCandidate.Foundation(),
			Priority:      iceCandidate.Priority(),
			Address:       endpoint.External.Addr().String(),
			Protocol:      pion.ICEProtocolTCP,
			Port:          endpoint.External.Port(),
			Typ:           pion.ICECandidateTypeHost,
			Component:     ice.ComponentRTP,
			TCPType:       ice.TCPTypePassive.String(),
			SDPMid:        sdpMid,
			SDPMLineIndex: sdpMLineIndex,
		}, true
	}
	network := "udp4"
	if endpoint.External.Addr().Is6() {
		network = "udp6"
	}
	iceCandidate, err := ice.NewCandidateServerReflexive(&ice.CandidateServerReflexiveConfig{
		Network:   network,
		Address:   endpoint.External.Addr().String(),
		Port:      int(endpoint.External.Port()),
		Component: ice.ComponentRTP,
		RelAddr:   endpoint.Local.Addr().String(),
		RelPort:   int(endpoint.Local.Port()),
	})
	if err != nil {
		return nil, false
	}
	return &pion.ICECandidate{
		Foundation:     iceCandidate.Foundation(),
		Priority:       iceCandidate.Priority(),
		Address:        endpoint.External.Addr().String(),
		Protocol:       pion.ICEProtocolUDP,
		Port:           endpoint.External.Port(),
		Typ:            pion.ICECandidateTypeSrflx,
		Component:      ice.ComponentRTP,
		RelatedAddress: endpoint.Local.Addr().String(),
		RelatedPort:    endpoint.Local.Port(),
		SDPMid:         sdpMid,
		SDPMLineIndex:  sdpMLineIndex,
	}, true
}

func (c *Connection) observeSelectedPair(pair *pion.ICECandidatePair) {
	if c.request.Observe == nil || pair == nil {
		return
	}
	event := c.event("selected_pair", "")
	event.Pair = &PairFacts{LocalType: pair.Local.Typ.String(), RemoteType: pair.Remote.Typ.String(), Protocol: pair.Local.Protocol.String(),
		LocalAddress: pair.Local.Address, RemoteAddress: pair.Remote.Address, LocalPort: pair.Local.Port, RemotePort: pair.Remote.Port}
	c.request.Observe(event)
}

func (c *Connection) observeCandidate(candidate *pion.ICECandidate) {
	if candidate == nil {
		c.observe("gathering_complete", "")
		return
	}
	if c.request.Observe == nil {
		return
	}
	family := "unknown"
	if address, err := netip.ParseAddr(candidate.Address); err == nil {
		family = "ipv6"
		if address.Is4() {
			family = "ipv4"
		}
	}
	origin := "ordinary"
	if c.IsMappedCandidate(candidate.ToJSON().Candidate) {
		origin = "mapped"
	}
	event := c.event("candidate", "")
	event.Candidate = &CandidateFacts{Priority: candidate.Priority, TCPType: candidate.TCPType, Type: candidate.Typ.String(), Protocol: candidate.Protocol.String(), Address: candidate.Address, Port: candidate.Port, Family: family, Origin: origin}
	c.request.Observe(event)
}
