//go:build linux

package networkstate

import (
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"strconv"
	"syscall"
	"time"
)

// IFA_FLAGS carries the full flags word; if present it supersedes ifaddrmsg's byte.
const linuxAddressFlags = 8

func (SystemSource) Snapshot(ctx context.Context) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return State{}, err
	}
	now := time.Now()
	messages, err := linuxNetlinkMessages(syscall.RTM_GETADDR)
	if err != nil {
		return State{}, err
	}
	addresses, err := linuxAddresses(messages, interfaces, now)
	if err != nil {
		return State{}, err
	}
	messages, err = linuxNetlinkMessages(syscall.RTM_GETROUTE)
	if err != nil {
		return State{}, err
	}
	routes, err := linuxRoutes(messages)
	if err != nil {
		return State{}, err
	}
	return State{Addresses: addresses, Routes: routes}, ctx.Err()
}

func linuxNetlinkMessages(kind int) ([]syscall.NetlinkMessage, error) {
	data, err := syscall.NetlinkRIB(kind, syscall.AF_UNSPEC)
	if err != nil {
		return nil, err
	}
	return syscall.ParseNetlinkMessage(data)
}

func linuxAddresses(messages []syscall.NetlinkMessage, interfaces []net.Interface, now time.Time) ([]Address, error) {
	byIndex := make(map[int]net.Interface, len(interfaces))
	for _, iface := range interfaces {
		byIndex[iface.Index] = iface
	}
	var addresses []Address
	for _, message := range messages {
		if message.Header.Type != syscall.RTM_NEWADDR || len(message.Data) < syscall.SizeofIfAddrmsg {
			continue
		}
		index := int(binary.NativeEndian.Uint32(message.Data[4:8]))
		iface, ok := byIndex[index]
		if !ok || iface.Flags&net.FlagUp == 0 {
			continue
		}
		attrs, err := syscall.ParseNetlinkRouteAttr(&message)
		if err != nil {
			return nil, err
		}
		addresses = append(addresses, linuxAddress(iface, uint32(message.Data[2]), attrs, now))
	}
	return addresses, nil
}

func linuxAddress(iface net.Interface, flags uint32, attrs []syscall.NetlinkRouteAttr, now time.Time) Address {
	address := Address{InterfaceIndex: iface.Index, InterfaceName: iface.Name, Class: linuxInterfaceClass(iface.Flags)}
	for _, attr := range attrs {
		switch attr.Attr.Type {
		case syscall.IFA_ADDRESS:
			if !address.IP.IsValid() {
				address.IP, _ = netip.AddrFromSlice(attr.Value)
			}
		case syscall.IFA_LOCAL:
			address.IP, _ = netip.AddrFromSlice(attr.Value)
		case syscall.IFA_CACHEINFO:
			if len(attr.Value) >= 8 {
				address.PreferredUntil = linuxLifetime(now, binary.NativeEndian.Uint32(attr.Value[:4]))
				address.ValidUntil = linuxLifetime(now, binary.NativeEndian.Uint32(attr.Value[4:8]))
			}
		case linuxAddressFlags:
			if len(attr.Value) >= 4 {
				flags = binary.NativeEndian.Uint32(attr.Value[:4])
			}
		}
	}
	address.Tentative = flags&(syscall.IFA_F_TENTATIVE|syscall.IFA_F_DADFAILED) != 0
	address.Deprecated = flags&syscall.IFA_F_DEPRECATED != 0
	address.IP = linuxAddressZone(address.IP.Unmap(), iface.Index)
	return address
}

func linuxInterfaceClass(flags net.Flags) string {
	if flags&net.FlagLoopback != 0 {
		return "loopback"
	}
	if flags&net.FlagPointToPoint != 0 {
		return "vpn"
	}
	return "unknown"
}

func linuxRoutes(messages []syscall.NetlinkMessage) ([]Route, error) {
	var routes []Route
	for _, message := range messages {
		if message.Header.Type != syscall.RTM_NEWROUTE || len(message.Data) < syscall.SizeofRtMsg || message.Data[1] != 0 || message.Data[7] != syscall.RTN_UNICAST {
			continue
		}
		attrs, err := syscall.ParseNetlinkRouteAttr(&message)
		if err != nil {
			return nil, err
		}
		routes = append(routes, linuxRoute(message.Data[0], attrs))
	}
	return routes, nil
}

func linuxRoute(family byte, attrs []syscall.NetlinkRouteAttr) Route {
	route := Route{Family: 6}
	if family == syscall.AF_INET {
		route.Family = 4
	}
	for _, attr := range attrs {
		switch attr.Attr.Type {
		case syscall.RTA_GATEWAY:
			route.Gateway, _ = netip.AddrFromSlice(attr.Value)
		case syscall.RTA_OIF:
			if len(attr.Value) >= 4 {
				route.InterfaceIndex = int(binary.NativeEndian.Uint32(attr.Value[:4]))
			}
		case syscall.RTA_PRIORITY:
			if len(attr.Value) >= 4 {
				route.Metric = binary.NativeEndian.Uint32(attr.Value[:4])
			}
		}
	}
	route.Gateway = linuxAddressZone(route.Gateway, route.InterfaceIndex)
	return route
}

func linuxAddressZone(ip netip.Addr, index int) netip.Addr {
	if ip.Is6() && ip.IsLinkLocalUnicast() {
		return ip.WithZone(strconv.Itoa(index))
	}
	return ip
}

func linuxLifetime(now time.Time, seconds uint32) time.Time {
	if seconds == ^uint32(0) {
		return time.Time{}
	}
	return now.Add(time.Duration(seconds) * time.Second)
}
