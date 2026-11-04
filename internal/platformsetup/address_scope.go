package platformsetup

import (
	"encoding/binary"
	"math/bits"
	"net/netip"
	"strings"
)

// Interface addresses let us exclude unrelated VPN/LAN rules without assuming a
// peer address or ICE port. Partial coverage stays unknown for the whole interface.
func matchLocalAddresses(filters, addresses []string) Setting {
	if len(filters) == 0 {
		return Yes
	}
	if len(addresses) == 0 {
		return Unspecified
	}
	matched, unknown := 0, false
	for _, address := range addresses {
		ip, err := netip.ParseAddr(address)
		if err != nil {
			unknown = true
			continue
		}
		match := matchAddress(filters, ip.Unmap())
		if match == Yes {
			matched++
		}
		unknown = unknown || match == Unspecified
	}
	if matched == len(addresses) {
		return Yes
	}
	if matched == 0 && !unknown {
		return No
	}
	return Unspecified
}

func matchAddress(filters []string, address netip.Addr) Setting {
	result := No
	for _, filter := range filters {
		match := matchAddressFilter(strings.TrimSpace(filter), address)
		if match == Yes {
			return Yes
		}
		if match == Unspecified {
			result = Unspecified
		}
	}
	return result
}

func matchAddressFilter(filter string, address netip.Addr) Setting {
	if filter == "Any" || filter == "*" {
		return Yes
	}
	if ip, err := netip.ParseAddr(filter); err == nil {
		if ip.Unmap() == address {
			return Yes
		}
		return No
	}
	if prefix, err := netip.ParsePrefix(filter); err == nil {
		if prefix.Contains(address) {
			return Yes
		}
		return No
	}
	if first, last, ok := strings.Cut(filter, "-"); ok {
		low, lowErr := netip.ParseAddr(first)
		high, highErr := netip.ParseAddr(last)
		if lowErr != nil || highErr != nil || low.BitLen() != high.BitLen() || low.Compare(high) > 0 {
			return Unspecified
		}
		if address.BitLen() == low.BitLen() && low.Compare(address) <= 0 && high.Compare(address) >= 0 {
			return Yes
		}
		return No
	}
	// NetSecurity emits IPv4 subnets with dotted masks as well as CIDR lengths.
	if network, mask, ok := strings.Cut(filter, "/"); ok {
		ip, ipErr := netip.ParseAddr(network)
		netmask, maskErr := netip.ParseAddr(mask)
		if ipErr != nil || maskErr != nil || !ip.Is4() || !netmask.Is4() {
			return Unspecified
		}
		maskBits := binary.BigEndian.Uint32(netmask.AsSlice())
		inverse := ^maskBits
		if inverse&(inverse+1) != 0 {
			return Unspecified
		}
		if netip.PrefixFrom(ip, bits.OnesCount32(maskBits)).Contains(address) {
			return Yes
		}
		return No
	}
	// Keywords such as LocalSubnet and dynamically resolved sets need more evidence.
	return Unspecified
}
