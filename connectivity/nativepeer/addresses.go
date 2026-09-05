package nativepeer

import (
	"cmp"
	"net/netip"
	"slices"

	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/socketauthority"
)

// Allocation is bounded before opening sockets. Round-robin interface/family
// buckets retain IPv6 and additional interfaces when one interface has many IPs.
func selectAddresses(addresses []networkstate.Address) []netip.Addr {
	addresses = slices.Clone(addresses)
	addresses = slices.DeleteFunc(addresses, func(a networkstate.Address) bool { return !a.IP.IsValid() })
	slices.SortFunc(addresses, func(a, b networkstate.Address) int {
		rank := func(a networkstate.Address) int {
			if a.IP.IsLoopback() {
				return 1
			}
			return 0
		}
		if n := cmp.Compare(rank(a), rank(b)); n != 0 {
			return n
		}
		if n := cmp.Compare(a.InterfaceIndex, b.InterfaceIndex); n != 0 {
			return n
		}
		return a.IP.Compare(b.IP)
	})
	type key struct {
		index int
		v6    bool
	}
	groups := make(map[key][]netip.Addr)
	var families [2][]key
	for _, address := range addresses {
		family := 0
		if address.IP.Is6() {
			family = 1
		}
		k := key{address.InterfaceIndex, family == 1}
		if len(groups[k]) == 0 {
			families[family] = append(families[family], k)
		}
		groups[k] = append(groups[k], address.IP)
	}
	var buckets []key
	for i := 0; i < max(len(families[0]), len(families[1])); i++ {
		for _, family := range families {
			if i < len(family) {
				buckets = append(buckets, family[i])
			}
		}
	}
	result := make([]netip.Addr, 0, min(len(addresses), socketauthority.MaxAddressesPerPath))
	for round := 0; len(result) < socketauthority.MaxAddressesPerPath; round++ {
		added := false
		for _, key := range buckets {
			if round < len(groups[key]) {
				result = append(result, groups[key][round])
				added = true
			}
			if len(result) == socketauthority.MaxAddressesPerPath {
				return result
			}
		}
		if !added {
			return result
		}
	}
	return result
}
