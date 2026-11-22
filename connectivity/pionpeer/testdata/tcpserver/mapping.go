package main

import (
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

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

func mappedOnlySDP(sdp, address string, localPort, externalPort int) string {
	from := fmt.Sprintf(" %s %d ", address, localPort)
	to := fmt.Sprintf(" %s %d ", address, externalPort)
	var lines []string
	for _, line := range strings.Split(sdp, "\r\n") {
		if strings.HasPrefix(line, "a=candidate:") {
			if !strings.Contains(line, from) {
				continue
			}
			line = strings.Replace(line, from, to, 1)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\r\n")
}
