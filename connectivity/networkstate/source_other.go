//go:build !windows && !linux

package networkstate

import (
	"context"
	"net"
	"net/netip"
)

// Unsupported platform route/lifetime metadata remains unknown rather than
// inventing public-IPv6 suitability from interface enumeration alone.
func (SystemSource) Snapshot(ctx context.Context) (State, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return State{}, err
	}
	state := State{}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			return State{}, err
		}
		for _, address := range addresses {
			prefix, err := netip.ParsePrefix(address.String())
			if err != nil {
				continue
			}
			state.Addresses = append(state.Addresses, Address{IP: prefix.Addr(), InterfaceIndex: iface.Index, InterfaceName: iface.Name, Class: "unknown"})
		}
	}
	return state, ctx.Err()
}
