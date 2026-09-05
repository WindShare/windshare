package provider

import (
	"net/netip"

	"github.com/pion/ice/v4"
	pion "github.com/pion/webrtc/v4"
)

type CandidateFacts struct {
	Type     string
	Protocol string
	Address  string
	Port     uint16
	Family   string
	Origin   string
}

// IsMappedCandidate matches a candidate to the trusted immutable lease snapshot;
// srflx syntax alone can never grant mapped-candidate provenance.
func (c *Connection) IsMappedCandidate(raw string) bool {
	candidate, err := ice.UnmarshalCandidate(raw)
	if err != nil || candidate.Type() != ice.CandidateTypeServerReflexive || candidate.Port() < 1 || candidate.Port() > 65535 {
		return false
	}
	address, err := netip.ParseAddr(candidate.Address())
	if err != nil {
		return false
	}
	for _, endpoint := range c.request.MappedEndpoints {
		protocol := endpoint.Protocol
		if protocol == "" {
			protocol = "udp"
		}
		if candidate.NetworkType().NetworkShort() != protocol || endpoint.External != netip.AddrPortFrom(address, uint16(candidate.Port())) {
			continue
		}
		related := candidate.RelatedAddress()
		if related != nil && related.Address == endpoint.Local.Addr().String() && related.Port == int(endpoint.Local.Port()) {
			return true
		}
	}
	return false
}
func (c *Connection) OnICECandidate(callback func(*pion.ICECandidate)) {
	c.PeerConnection.OnICECandidate(func(candidate *pion.ICECandidate) {
		c.observeCandidate(candidate)
		if callback != nil {
			callback(candidate)
		}
	})
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
	event.Candidate = &CandidateFacts{Type: candidate.Typ.String(), Protocol: candidate.Protocol.String(), Address: candidate.Address, Port: candidate.Port, Family: family, Origin: origin}
	c.request.Observe(event)
}
