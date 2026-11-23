package pionpeer

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
)

const (
	hostSTUNTimeout = 5 * time.Second
	// Limit retries per hostname so each address can still receive STUN retransmissions.
	maxHostSTUNAddresses = 3
)

type hostPortDiscovery interface {
	DiscoverXORMappedAddr(context.Context, netip.AddrPort, *net.UDPAddr, time.Duration) (netip.AddrPort, error)
}

type stunHostLookup func(context.Context, string, string) ([]netip.Addr, error)

func hostSTUNFamily(ip netip.Addr) string {
	if ip.Unmap().Is4() {
		return "ip4"
	}
	return "ip6"
}

func discoverHostSTUN(ctx context.Context, mux hostPortDiscovery, local netip.AddrPort, server iceconfig.STUNServer, allowLoopback bool) (netip.AddrPort, error) {
	return discoverHostSTUNWithLookup(ctx, mux, local, server, allowLoopback, net.DefaultResolver.LookupNetIP)
}

func discoverHostSTUNWithLookup(ctx context.Context, mux hostPortDiscovery, local netip.AddrPort, server iceconfig.STUNServer, allowLoopback bool, lookup stunHostLookup) (netip.AddrPort, error) {
	ctx, cancel := context.WithTimeout(ctx, hostSTUNTimeout)
	defer cancel()

	local = netip.AddrPortFrom(local.Addr().Unmap(), local.Port())
	answers, err := lookup(ctx, hostSTUNFamily(local.Addr()), server.Host)
	if err != nil {
		return netip.AddrPort{}, err
	}
	targets := hostSTUNAddresses(answers, local.Addr())
	if len(targets) == 0 {
		return netip.AddrPort{}, errors.New("no usable STUN server address for host socket")
	}
	deadline, _ := ctx.Deadline()
	var lastErr error
	for i, address := range targets {
		if err := ctx.Err(); err != nil {
			return netip.AddrPort{}, err
		}
		// A blackholed address must leave time for the remaining DNS answers.
		// Fast failures return their unused share to the following addresses.
		timeout := time.Until(deadline) / time.Duration(len(targets)-i)
		if timeout <= 0 {
			return netip.AddrPort{}, context.DeadlineExceeded
		}
		target := net.UDPAddrFromAddrPort(netip.AddrPortFrom(address, server.Port))
		external, err := mux.DiscoverXORMappedAddr(ctx, local, target, timeout)
		if ctx.Err() != nil {
			return netip.AddrPort{}, ctx.Err()
		}
		if err != nil {
			lastErr = err
			continue
		}
		if !allowLoopback && external.Addr().IsLoopback() {
			lastErr = errors.New("STUN returned a loopback mapped address")
			continue
		}
		return external, nil
	}
	return netip.AddrPort{}, lastErr
}

func hostSTUNAddresses(answers []netip.Addr, local netip.Addr) []netip.Addr {
	targets := make([]netip.Addr, 0, min(len(answers), maxHostSTUNAddresses))
	for _, address := range answers {
		ip := address.Unmap()
		// A local STUN server can report a routable mapping even when loopback
		// candidates are disabled.
		if ip.Is4() != local.Is4() || (!ip.IsGlobalUnicast() && !ip.IsLoopback()) || slices.Contains(targets, ip) {
			continue
		}
		targets = append(targets, ip)
		if len(targets) == maxHostSTUNAddresses {
			break
		}
	}
	return targets
}
