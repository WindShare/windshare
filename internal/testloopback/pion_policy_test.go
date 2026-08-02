package testloopback

import (
	"net"
	"testing"
)

type stringNetworkAddress string

func (address stringNetworkAddress) Network() string { return "test" }
func (address stringNetworkAddress) String() string  { return string(address) }

func TestPionAddressPolicyAllowsOnlyTheExactIPv4LoopbackEndpoint(t *testing.T) {
	for _, test := range []struct {
		name    string
		address net.IP
		allowed bool
	}{
		{name: "exact", address: net.ParseIP("127.0.0.1"), allowed: true},
		{name: "other loopback", address: net.ParseIP("127.0.0.2")},
		{name: "IPv6 loopback", address: net.ParseIP("::1")},
		{name: "private", address: net.ParseIP("192.168.1.1")},
		{name: "absent"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := isExactLoopbackIPv4(test.address); got != test.allowed {
				t.Fatalf("isExactLoopbackIPv4(%v) = %t, want %t", test.address, got, test.allowed)
			}
		})
	}
}

func TestNetworkAddressIPHandlesInterfaceAddressShapes(t *testing.T) {
	for _, test := range []struct {
		name    string
		address net.Addr
		want    string
	}{
		{name: "IP network", address: &net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)}, want: "127.0.0.1"},
		{name: "IP address", address: &net.IPAddr{IP: net.ParseIP("127.0.0.1")}, want: "127.0.0.1"},
		{name: "CIDR text", address: stringNetworkAddress("127.0.0.1/8"), want: "127.0.0.1"},
		{name: "IP text", address: stringNetworkAddress("127.0.0.1"), want: "127.0.0.1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := networkAddressIP(test.address); got == nil || got.String() != test.want {
				t.Fatalf("networkAddressIP(%q) = %v, want %s", test.address, got, test.want)
			}
		})
	}
}
