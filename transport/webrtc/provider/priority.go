package provider

import "net/netip"

// Socket order carries the network snapshot's interface opportunities. Preserve
// it within each family and interleave families so one cannot monopolize checks.
func localAddressOrder(endpoints []netip.AddrPort) []netip.Addr {
	families := [2][]netip.Addr{}
	seen := make(map[netip.Addr]bool)
	for _, endpoint := range endpoints {
		address := endpoint.Addr().Unmap()
		if seen[address] {
			continue
		}
		seen[address] = true
		family := 0
		if address.Is6() {
			family = 1
		}
		families[family] = append(families[family], address)
	}
	ordered := make([]netip.Addr, 0, len(seen))
	for index := 0; index < max(len(families[0]), len(families[1])); index++ {
		for _, addresses := range families {
			if index < len(addresses) {
				ordered = append(ordered, addresses[index])
			}
		}
	}
	return ordered
}
