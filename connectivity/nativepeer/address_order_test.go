package nativepeer

import (
	"net/netip"
	"slices"
	"testing"

	"github.com/windshare/windshare/connectivity/networkstate"
)

func TestAddressSelectionInterleavesInterfacesBeforeExtraAddresses(t *testing.T) {
	inputs := []networkstate.Address{
		{InterfaceIndex: 1, IP: netip.MustParseAddr("10.0.0.2")},
		{InterfaceIndex: 2, IP: netip.MustParseAddr("192.168.1.1")},
		{InterfaceIndex: 1, IP: netip.MustParseAddr("2001:db8:1::2")},
		{InterfaceIndex: 1, IP: netip.MustParseAddr("10.0.0.1")},
		{InterfaceIndex: 2, IP: netip.MustParseAddr("2001:db8:2::1")},
		{InterfaceIndex: 1, IP: netip.MustParseAddr("2001:db8:1::1")},
	}
	want := []netip.Addr{
		netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("2001:db8:1::1"),
		netip.MustParseAddr("192.168.1.1"), netip.MustParseAddr("2001:db8:2::1"),
		netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("2001:db8:1::2"),
	}
	if got := selectAddresses(inputs); !slices.Equal(got, want) {
		t.Fatalf("interface opportunity=%v, want %v", got, want)
	}
}
