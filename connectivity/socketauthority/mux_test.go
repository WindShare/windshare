package socketauthority

import (
	"net"
	"net/netip"
	"testing"
)

func TestSTUNMappedEndpoint(t *testing.T) {
	for _, test := range []struct {
		local, mapped, want string
	}{
		{"::ffff:192.0.2.1", "::ffff:10.0.0.9", "10.0.0.9"},
		{"2001:db8::1", "fd00::9", "fd00::9"},
		{"127.0.0.1", "127.0.0.1", "127.0.0.1"},
		{"::1", "::1", "::1"},
		{"192.0.2.1", "0.0.0.0", ""},
		{"2001:db8::1", "::", ""},
		{"192.0.2.1", "224.0.0.1", ""},
		{"2001:db8::1", "ff02::1", ""},
		{"192.0.2.1", "169.254.1.1", ""},
		{"2001:db8::1", "fe80::1", ""},
		{"192.0.2.1", "255.255.255.255", ""},
		{"2001:db8::1", "192.0.2.1", ""},
	} {
		t.Run(test.local+"_"+test.mapped, func(t *testing.T) {
			const port = 41123
			got, err := stunMappedEndpoint(netip.MustParseAddr(test.local), net.ParseIP(test.mapped), port)
			if test.want == "" {
				if err == nil {
					t.Fatalf("accepted %v", got)
				}
				return
			}
			want := netip.AddrPortFrom(netip.MustParseAddr(test.want), port)
			if err != nil || got != want {
				t.Fatalf("mapped endpoint = %v, error = %v, want %v", got, err, want)
			}
		})
	}
}
