// Two independent loopback TURN listeners expose only aggregate transfer evidence.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/pion/turn/v5"
)

type countedSocket struct {
	net.PacketConn
	received atomic.Uint64
	burst    int
	next     time.Time
}

func (c *countedSocket) ReadFrom(b []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(b)
	// Shape each independent uplink in bursts so a fast loopback transfer spans
	// the scheduler's normal five-second lane exploration interval.
	const rate = 3 << 20
	const burst = 64 << 10
	c.burst += n
	if c.burst >= burst {
		if delay := time.Until(c.next); delay > 0 {
			time.Sleep(delay)
		}
		c.next = time.Now().Add(time.Duration(c.burst) * time.Second / rate)
		c.burst = 0
	}
	c.received.Add(uint64(n))
	return n, addr, err
}

type endpoint struct {
	server     *turn.Server
	socket     *countedSocket
	credential map[string]any
}

func start(id string, local net.IP) (endpoint, error) {
	const realm = "windshare.local-test"
	packet, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		return endpoint{}, err
	}
	socket := &countedSocket{PacketConn: packet}
	var secret [24]byte
	rand.Read(secret[:])
	password := base64.RawURLEncoding.EncodeToString(secret[:])
	server, err := turn.NewServer(turn.ServerConfig{Realm: realm,
		AuthHandler: func(req *turn.RequestAttributes) (string, []byte, bool) {
			return id, turn.GenerateAuthKey(id, realm, password), req.Username == id
		},
		PacketConnConfigs: []turn.PacketConnConfig{{PacketConn: socket,
			PermissionHandler:     func(_ net.Addr, peer net.IP) bool { return peer.Equal(local) },
			RelayAddressGenerator: &turn.RelayAddressGeneratorStatic{RelayAddress: local, Address: local.String()}}}})
	if err != nil {
		packet.Close()
		return endpoint{}, err
	}
	return endpoint{server, socket, map[string]any{"id": id, "kind": "shared_static", "urls": []string{"turn:" + socket.LocalAddr().String() + "?transport=udp"}, "username": id, "password": password}}, nil
}
func run() error {
	// Ask the kernel for a routable local interface without sending any packets.
	probe, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 9})
	if err != nil {
		return err
	}
	local := probe.LocalAddr().(*net.UDPAddr).IP
	probe.Close()
	var services []endpoint
	var configs []map[string]any
	for _, id := range []string{"west", "east"} {
		s, err := start(id, local)
		if err != nil {
			return err
		}
		defer s.server.Close()
		services = append(services, s)
		configs = append(configs, s.credential)
	}
	evidence, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"services": configs, "stats": "http://" + evidence.Addr().String()}); err != nil {
		return err
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var result []map[string]any
		for _, s := range services {
			result = append(result, map[string]any{"bytes": s.socket.received.Load(), "allocations": s.server.AllocationCount()})
		}
		_ = json.NewEncoder(w).Encode(result)
	})
	server := http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	return server.Serve(evidence)
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
