package provider

import (
	"net/netip"
	"slices"
	"testing"

	"github.com/pion/ice/v4"
)

// A many-address interface must not jump ahead of the next interface merely
// because all of its addresses sort below that interface's address.
func TestPriorityPreservesInterfaceOpportunityWithinEachFamily(t *testing.T) {
	endpoints := []netip.AddrPort{
		netip.MustParseAddrPort("10.0.0.1:1"),
		netip.MustParseAddrPort("[2001:db8:1::1]:1"),
		netip.MustParseAddrPort("192.168.1.1:1"),
		netip.MustParseAddrPort("[2001:db8:2::1]:1"),
		netip.MustParseAddrPort("10.0.0.2:1"),
		netip.MustParseAddrPort("[2001:db8:1::2]:1"),
		netip.MustParseAddrPort("10.0.0.1:2"),
	}
	want := make([]netip.Addr, 0, 6)
	for _, endpoint := range endpoints[:6] {
		want = append(want, endpoint.Addr())
	}
	if got := localAddressOrder(endpoints); !slices.Equal(got, want) {
		t.Fatalf("interface opportunities=%v, want %v", got, want)
	}
}

func TestAddressOrderInterleavesUnevenFamiliesAndDeduplicatesBases(t *testing.T) {
	for _, test := range []struct {
		name      string
		endpoints []string
		want      []string
	}{
		{"empty", nil, nil},
		{"uneven", []string{"192.168.1.1:1", "192.168.2.1:2", "[2001:db8::1]:3"}, []string{"192.168.1.1", "2001:db8::1", "192.168.2.1"}},
		{"ipv6", []string{"[2001:db8::2]:1", "[2001:db8::1]:2"}, []string{"2001:db8::2", "2001:db8::1"}},
		{"mapped-ipv4", []string{"[::ffff:192.168.1.1]:1", "192.168.1.1:2"}, []string{"192.168.1.1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var endpoints []netip.AddrPort
			for _, raw := range test.endpoints {
				endpoints = append(endpoints, netip.MustParseAddrPort(raw))
			}
			var want []netip.Addr
			for _, raw := range test.want {
				want = append(want, netip.MustParseAddr(raw))
			}
			if got := localAddressOrder(endpoints); !slices.Equal(got, want) {
				t.Fatalf("address order=%v, want %v", got, want)
			}
		})
	}
}

func TestProviderRejectsAmbiguousAddressRanks(t *testing.T) {
	ip := netip.MustParseAddr("127.0.0.1")
	for _, order := range [][]netip.Addr{
		{{}}, {ip, ip}, {netip.MustParseAddr("::ffff:127.0.0.1")}, make([]netip.Addr, 8193),
	} {
		agent, err := ice.NewAgentWithOptions(ice.WithProviderConfig(ice.ProviderConfig{LocalAddressOrder: order}))
		if err == nil {
			_ = agent.Close()
			t.Fatal("invalid address order accepted")
		}
	}
}
