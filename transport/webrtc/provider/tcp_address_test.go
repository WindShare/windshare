package provider

import (
	"net"
	"testing"
)

// Pion excludes IPv6 loopback during ordinary TCP host gathering. Probe a real
// local address without reaching an external endpoint or altering host routes.
func localTCPIPv6(t *testing.T) string {
	t.Helper()
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err != nil || ip.To4() != nil || !ip.IsGlobalUnicast() || ip.IsLoopback() {
				continue
			}
			listener, err := net.Listen("tcp6", net.JoinHostPort(ip.String(), "0"))
			if err != nil {
				continue
			}
			_ = listener.Close()
			return ip.String()
		}
	}
	t.Log("TCP6 evidence unavailable: no locally bindable non-loopback IPv6 address")
	return ""
}
