package nativepeer

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/windshare/windshare/connectivity/v2signal"
)

// Idle transfers only a previously reserved attempt's bounded wait to the
// socket authority. DNS failure never turns into a transport retry decision.
func (n *NativePeerConnectivity) Idle(ctx context.Context, session [16]byte, pathID v2signal.PeerPathID, until time.Time) {
	n.mu.Lock()
	path := n.paths[pathKey{session, pathID}]
	if path == nil || path.lease == nil || len(path.lastURLs) == 0 {
		n.mu.Unlock()
		return
	}
	lease := path.lease
	urls := append([]string(nil), path.lastURLs...)
	n.mu.Unlock()
	resolve, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var servers []netip.AddrPort
	for _, raw := range urls {
		host, portText, err := net.SplitHostPort(strings.TrimPrefix(raw, "stun:"))
		if err != nil {
			continue
		}
		port, err := strconv.ParseUint(portText, 10, 16)
		if err != nil || port == 0 {
			continue
		}
		addresses, err := net.DefaultResolver.LookupNetIP(resolve, "ip", host)
		if err != nil {
			continue
		}
		for _, address := range addresses {
			servers = append(servers, netip.AddrPortFrom(address, uint16(port)))
			break
		}
		if len(servers) == 2 {
			break
		}
	}
	if len(servers) > 0 {
		_ = lease.StartIdle(ctx, servers, until)
	}
}
