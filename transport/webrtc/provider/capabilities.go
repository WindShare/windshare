package provider

import "runtime"

// TCPProfile names exact implementation/platform combinations backed by local
// selected-pair and authenticated-payload evidence. It is sent only through
// authenticated session controls; absent or unknown profiles remain UDP-only.
type TCPProfile string

const (
	TCPNativeWindows   TCPProfile = "pion-4.2.16-ice-4.2.7-windows"
	TCPChromiumWindows TCPProfile = "chromium-151.0.7922.34-windows"
)

type TCPCapability struct {
	IPv4, IPv6  bool
	PassiveOnly bool
}

func Capabilities(profile TCPProfile) TCPCapability {
	return tcpCapabilities(runtime.GOOS, profile)
}
func tcpCapabilities(platform string, profile TCPProfile) TCPCapability {
	if platform != "windows" {
		return TCPCapability{}
	}
	switch profile {
	case TCPNativeWindows:
		return TCPCapability{IPv4: true, IPv6: true}
	case TCPChromiumWindows:
		return TCPCapability{IPv4: true, PassiveOnly: true}
	default:
		return TCPCapability{}
	}
}
func LocalTCPProfile() TCPProfile {
	if runtime.GOOS == "windows" {
		return TCPNativeWindows
	}
	return ""
}
