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

func (SystemSource) Snapshot(ctx context.Context) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return State{}, err
	}
	byIndex := make(map[int]net.Interface)
	for _, iface := range interfaces {
		byIndex[iface.Index] = iface
	}
	state := State{}
	now := time.Now()
	data, err := syscall.NetlinkRIB(syscall.RTM_GETADDR, syscall.AF_UNSPEC)
	if err != nil {
		return State{}, err
	}
	messages, err := syscall.ParseNetlinkMessage(data)
	if err != nil {
		return State{}, err
	}
	for _, message := range messages {
		if message.Header.Type != syscall.RTM_NEWADDR || len(message.Data) < 8 {
			continue
		}
		index := int(binary.NativeEndian.Uint32(message.Data[4:8]))
		iface, ok := byIndex[index]
		if !ok || iface.Flags&net.FlagUp == 0 {
			continue
		}
		attrs, parseErr := syscall.ParseNetlinkRouteAttr(&message)
		if parseErr != nil {
			return State{}, parseErr
		}
		address := Address{InterfaceIndex: index, InterfaceName: iface.Name, Class: "unknown"}
		if iface.Flags&net.FlagLoopback != 0 {
			address.Class = "loopback"
		} else if iface.Flags&net.FlagPointToPoint != 0 {
			address.Class = "vpn"
		}
		flags := uint32(message.Data[2])
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
			case 8:
				if len(attr.Value) >= 4 {
					flags = binary.NativeEndian.Uint32(attr.Value[:4])
				}
			}
		}
		address.Tentative = flags&(syscall.IFA_F_TENTATIVE|syscall.IFA_F_DADFAILED) != 0
		address.Deprecated = flags&syscall.IFA_F_DEPRECATED != 0
		address.IP = address.IP.Unmap()
		if address.IP.Is6() && address.IP.IsLinkLocalUnicast() {
			address.IP = address.IP.WithZone(strconv.Itoa(index))
		}
		state.Addresses = append(state.Addresses, address)
	}
	data, err = syscall.NetlinkRIB(syscall.RTM_GETROUTE, syscall.AF_UNSPEC)
	if err != nil {
		return State{}, err
	}
	messages, err = syscall.ParseNetlinkMessage(data)
	if err != nil {
		return State{}, err
	}
	for _, message := range messages {
		if message.Header.Type != syscall.RTM_NEWROUTE || len(message.Data) < 12 || message.Data[1] != 0 || message.Data[7] != syscall.RTN_UNICAST {
			continue
		}
		attrs, parseErr := syscall.ParseNetlinkRouteAttr(&message)
		if parseErr != nil {
			return State{}, parseErr
		}
		route := Route{Family: 6}
		if message.Data[0] == syscall.AF_INET {
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
		if route.Gateway.Is6() && route.Gateway.IsLinkLocalUnicast() {
			route.Gateway = route.Gateway.WithZone(strconv.Itoa(route.InterfaceIndex))
		}
		state.Routes = append(state.Routes, route)
	}
	return state, ctx.Err()
}
func linuxLifetime(now time.Time, seconds uint32) time.Time {
	if seconds == ^uint32(0) {
		return time.Time{}
	}
	return now.Add(time.Duration(seconds) * time.Second)
}
