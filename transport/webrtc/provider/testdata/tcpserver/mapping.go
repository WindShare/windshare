package main

import (
	"io"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/pion/ice/v4"
)

type mappedListener struct {
	mux   *ice.TCPMuxDefault
	local netip.AddrPort
}

func (m mappedListener) GetConnForEndpoint(ufrag string, local netip.AddrPort) (net.PacketConn, error) {
	return m.mux.GetConnByUfrag(ufrag, local.Addr().Is6(), net.IP(local.Addr().AsSlice()))
}
func forwardTCP(listener net.Listener, internal net.Addr) {
	for {
		incoming, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer incoming.Close()
			outgoing, err := net.DialTimeout("tcp", internal.String(), time.Second)
			if err != nil {
				return
			}
			defer outgoing.Close()
			_ = incoming.SetDeadline(time.Now().Add(2 * operationLimit))
			_ = outgoing.SetDeadline(time.Now().Add(2 * operationLimit))
			done := make(chan struct{})
			go func() { _, _ = io.Copy(outgoing, incoming); _ = outgoing.Close(); close(done) }()
			_, _ = io.Copy(incoming, outgoing)
			_ = incoming.Close()
			<-done
		}()
	}
}
func mappedOnlySDP(sdp string) string {
	var lines []string
	for _, line := range strings.Split(sdp, "\r\n") {
		if strings.HasPrefix(line, "a=candidate:") && !strings.Contains(line, " typ srflx") {
			continue
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\r\n")
}
