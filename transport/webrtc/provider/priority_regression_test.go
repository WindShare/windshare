package provider

import (
	"net/netip"
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
	policy := localPreference(endpoints)
	for i, endpoint := range endpoints[:6] {
		candidate, err := ice.NewCandidateHost(&ice.CandidateHostConfig{Network: "udp", Address: endpoint.Addr().String(), Port: int(endpoint.Port()), Component: 1})
		if err != nil {
			t.Fatal(err)
		}
		got, ok := policy(candidate)
		if !ok || got != uint16(65535-i) {
			t.Fatalf("interface opportunity %s: preference=%d, want %d", endpoint, got, 65535-i)
		}
	}
}
