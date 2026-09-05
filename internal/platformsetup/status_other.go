//go:build !windows

package platformsetup

func Read() Status { return unavailable("platform-firewall-setup-unsupported") }
