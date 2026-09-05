package gateway

import (
	"net/netip"
	"slices"

	r "github.com/windshare/windshare/connectivity/reachability"
)

const MaxPCPServers = 8
const MaxPCPServerAddresses = 8

// Each group is one logical PCP server; addresses inside it are alternatives.
// Multiple groups are independent servers and must never be flattened together.
type PCPServer struct {
	Egress    string
	Addresses []netip.Addr
}
type PCPDiscovery struct {
	Configured     []PCPServer
	DHCPv4         []byte
	DHCPv6         [][]byte
	DefaultRouters []netip.Addr
	Egress         string
}

func DiscoverPCP(input PCPDiscovery) ([]PCPServer, error) {
	if len(input.Configured) > 0 {
		return configuredPCPServers(input.Configured, input.Egress)
	}
	result, err := dhcpV4PCPServers(input.DHCPv4, input.Egress)
	if err != nil {
		return nil, err
	}
	for _, data := range input.DHCPv6 {
		if len(data) == 0 || len(data)%16 != 0 {
			return nil, r.ErrInvalidResponse
		}
		group := PCPServer{Egress: input.Egress}
		for offset := 0; offset < len(data); offset += 16 {
			address := netip.AddrFrom16([16]byte(data[offset : offset+16])).Unmap()
			if validServer(address) {
				group.Addresses = append(group.Addresses, address)
			}
		}
		if len(group.Addresses) > MaxPCPServerAddresses {
			return nil, r.ErrCapacity
		}
		if len(group.Addresses) > 0 {
			result = append(result, group)
		}
	}
	if len(result) == 0 {
		for _, router := range input.DefaultRouters {
			if validServer(router) {
				result = append(result, PCPServer{Egress: input.Egress, Addresses: []netip.Addr{router}})
			}
		}
	}
	if len(result) > MaxPCPServers {
		return nil, r.ErrCapacity
	}
	return result, nil
}
func validServer(address netip.Addr) bool {
	return address.IsValid() && !address.IsUnspecified() && !address.IsMulticast() && !address.IsLoopback()
}

func configuredPCPServers(configured []PCPServer, egress string) ([]PCPServer, error) {
	if len(configured) > MaxPCPServers {
		return nil, r.ErrInvalid
	}
	result := make([]PCPServer, 0, len(configured))
	for _, server := range configured {
		if server.Egress != egress || len(server.Addresses) == 0 || len(server.Addresses) > MaxPCPServerAddresses {
			return nil, r.ErrInvalid
		}
		server.Addresses = slices.Clone(server.Addresses)
		for _, address := range server.Addresses {
			if !validServer(address) {
				return nil, r.ErrInvalid
			}
		}
		result = append(result, server)
	}
	return result, nil
}

func dhcpV4PCPServers(data []byte, egress string) ([]PCPServer, error) {
	var result []PCPServer
	for len(data) > 0 {
		length := int(data[0])
		data = data[1:]
		if length == 0 || length%4 != 0 || length > len(data) {
			return nil, r.ErrInvalidResponse
		}
		group := PCPServer{Egress: egress}
		for offset := 0; offset < length; offset += 4 {
			address := netip.AddrFrom4([4]byte(data[offset : offset+4]))
			if validServer(address) {
				group.Addresses = append(group.Addresses, address)
			}
		}
		if len(group.Addresses) > MaxPCPServerAddresses {
			return nil, r.ErrCapacity
		}
		if len(group.Addresses) > 0 {
			result = append(result, group)
		}
		data = data[length:]
	}
	return result, nil
}
