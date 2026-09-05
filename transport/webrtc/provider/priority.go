package provider

import (
	"net/netip"

	"github.com/pion/ice/v4"
)

// Keep interface opportunity interleaved between families instead of assigning
// every IPv6 candidate a higher preference than every IPv4 candidate.
func localPreference(endpoints []netip.AddrPort) func(ice.Candidate) (uint16, bool) {
	const highest = uint16(65535)
	families := [2][]netip.Addr{}
	seen := make(map[netip.Addr]bool)
	for _, endpoint := range endpoints {
		if seen[endpoint.Addr()] {
			continue
		}
		seen[endpoint.Addr()] = true
		family := 0
		if endpoint.Addr().Is6() {
			family = 1
		}
		families[family] = append(families[family], endpoint.Addr())
	}
	// Socket order carries the network snapshot's interface opportunities.
	// Sorting by IP here would let one interface consume the early checks.
	preferences := make(map[netip.Addr]uint16)
	for family, addresses := range families {
		for index, address := range addresses {
			preferences[address] = highest - uint16(index*2+family)
		}
	}
	return func(candidate ice.Candidate) (uint16, bool) {
		address := candidate.Address()
		if related := candidate.RelatedAddress(); related != nil && related.Address != "" {
			address = related.Address
		}
		ip, err := netip.ParseAddr(address)
		if err != nil {
			return 0, false
		}
		preference, ok := preferences[ip]
		return preference, ok
	}
}
