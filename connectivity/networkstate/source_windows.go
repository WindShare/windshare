//go:build windows

package networkstate

import (
	"context"
	"errors"
	"net/netip"
	"strconv"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const adapterBufferSize = 32 * 1024
const adapterBufferLimit = 4 * 1024 * 1024

func (SystemSource) Snapshot(ctx context.Context) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	size := uint32(adapterBufferSize)
	var buffer []byte
	var adapters *windows.IpAdapterAddresses
	for {
		buffer = make([]byte, size)
		adapters = (*windows.IpAdapterAddresses)(unsafe.Pointer(&buffer[0]))
		err := windows.GetAdaptersAddresses(windows.AF_UNSPEC, windows.GAA_FLAG_INCLUDE_GATEWAYS, 0, adapters, &size)
		if errors.Is(err, windows.ERROR_BUFFER_OVERFLOW) && size <= adapterBufferLimit {
			continue
		}
		if err != nil {
			return State{}, err
		}
		break
	}
	state := State{}
	now := time.Now()
	metrics := make(map[[2]int]uint32)
	for adapter := adapters; adapter != nil; adapter = adapter.Next {
		if adapter.OperStatus != windows.IfOperStatusUp {
			continue
		}
		class := adapterClass(adapter.IfType)
		metrics[[2]int{int(adapter.IfIndex), 4}] = adapter.Ipv4Metric
		metrics[[2]int{int(adapter.Ipv6IfIndex), 6}] = adapter.Ipv6Metric
		for unicast := adapter.FirstUnicastAddress; unicast != nil; unicast = unicast.Next {
			ip, ok := netip.AddrFromSlice(unicast.Address.IP())
			if !ok {
				continue
			}
			ip = ip.Unmap()
			index := int(adapter.IfIndex)
			if ip.Is6() {
				index = int(adapter.Ipv6IfIndex)
			}
			if ip.IsLinkLocalUnicast() && ip.Is6() {
				ip = ip.WithZone(strconv.Itoa(index))
			}
			state.Addresses = append(state.Addresses, Address{IP: ip, InterfaceIndex: index, InterfaceName: windows.UTF16PtrToString(adapter.FriendlyName), AdapterID: windows.BytePtrToString(adapter.AdapterName), Class: class, Tentative: unicast.DadState < windows.IpDadStateDeprecated, Deprecated: unicast.DadState == windows.IpDadStateDeprecated, PreferredUntil: lifetimeUntil(now, unicast.PreferredLifetime), ValidUntil: lifetimeUntil(now, unicast.ValidLifetime)})
		}
	}
	routes, err := defaultRoutes(metrics)
	if err != nil {
		return State{}, err
	}
	state.Routes = routes
	return state, ctx.Err()
}
func routeAddress(raw *windows.RawSockaddrInet) netip.Addr {
	if raw.Family == windows.AF_INET {
		return netip.AddrFrom4((*windows.RawSockaddrInet4)(unsafe.Pointer(raw)).Addr)
	}
	if raw.Family == windows.AF_INET6 {
		return netip.AddrFrom16((*windows.RawSockaddrInet6)(unsafe.Pointer(raw)).Addr)
	}
	return netip.Addr{}
}
func lifetimeUntil(now time.Time, seconds uint32) time.Time {
	if seconds == ^uint32(0) {
		return time.Time{}
	}
	return now.Add(time.Duration(seconds) * time.Second)
}

func adapterClass(kind uint32) string {
	class := "unknown"
	switch kind {
	case windows.IF_TYPE_SOFTWARE_LOOPBACK:
		class = "loopback"
	case windows.IF_TYPE_IEEE80211:
		class = "wifi"
	case windows.IF_TYPE_ETHERNET_CSMACD:
		class = "ethernet"
	case windows.IF_TYPE_TUNNEL, windows.IF_TYPE_PPP:
		class = "vpn"
	}
	return class
}

func defaultRoutes(metrics map[[2]int]uint32) ([]Route, error) {
	var routes []Route
	var table *windows.MibIpForwardTable2
	if err := windows.GetIpForwardTable2(windows.AF_UNSPEC, &table); err != nil {
		return nil, err
	}
	defer windows.FreeMibTable(unsafe.Pointer(table))
	for _, row := range table.Rows() {
		if row.DestinationPrefix.PrefixLength != 0 || row.ValidLifetime == 0 || row.Loopback != 0 {
			continue
		}
		gateway := routeAddress(&row.NextHop)
		family := 6
		if gateway.Is4() {
			family = 4
		}
		index := int(row.InterfaceIndex)
		if gateway.Is6() && gateway.IsLinkLocalUnicast() {
			gateway = gateway.WithZone(strconv.Itoa(index))
		}
		routes = append(routes, Route{InterfaceIndex: index, Gateway: gateway, Family: family, Metric: row.Metric + metrics[[2]int{index, family}]})
	}
	return routes, nil
}
