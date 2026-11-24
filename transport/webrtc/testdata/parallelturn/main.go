// Independent loopback TURN listeners (UDP parallel and TCP-only) expose only aggregate transfer evidence.
package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/turn/v5"
)

const (
	realm        = "windshare.local-test"
	fastRate     = 3 << 20
	fastBurst    = 64 << 10
	renewalRate  = 256 << 10
	renewalBurst = 16 << 10
	renewalTTL   = 60
)

type burstPacer struct {
	mu    sync.Mutex
	rate  int64
	burst int
	acc   int
	next  time.Time
}

func (p *burstPacer) pace(n int) {
	if p == nil || p.rate <= 0 || n <= 0 {
		return
	}
	p.mu.Lock()
	p.acc += n
	if p.acc < p.burst {
		p.mu.Unlock()
		return
	}
	step := time.Duration(p.acc) * time.Second / time.Duration(p.rate)
	delay := time.Until(p.next)
	if delay > 0 {
		p.next = p.next.Add(step)
	} else {
		p.next = time.Now().Add(step)
	}
	p.acc = 0
	p.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
}

type countedSocket struct {
	net.PacketConn
	received *atomic.Uint64
	pacer    *burstPacer
}

func (c *countedSocket) ReadFrom(b []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(b)
	c.pacer.pace(n)
	if n > 0 {
		c.received.Add(uint64(n))
	}
	return n, addr, err
}

type countedListener struct {
	net.Listener
	received *atomic.Uint64
	pacer    *burstPacer
}

func (l *countedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &countedConn{Conn: conn, received: l.received, pacer: l.pacer}, nil
}

type countedConn struct {
	net.Conn
	received *atomic.Uint64
	pacer    *burstPacer
}

func (c *countedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.pacer.pace(n)
	if n > 0 {
		c.received.Add(uint64(n))
	}
	return n, err
}

type endpoint struct {
	server     *turn.Server
	received   *atomic.Uint64
	credential map[string]any
	authMu     sync.Mutex
	authed     map[string]struct{}
}

func (e *endpoint) recordAuth(username string) {
	e.authMu.Lock()
	e.authed[username] = struct{}{}
	e.authMu.Unlock()
}

func (e *endpoint) stats(includeCredentials bool) map[string]any {
	entry := map[string]any{
		"bytes":       e.received.Load(),
		"allocations": e.server.AllocationCount(),
	}
	if includeCredentials {
		e.authMu.Lock()
		entry["credentials"] = len(e.authed)
		e.authMu.Unlock()
	}
	return entry
}

func randomSecret() (string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func verifyCoturn(username, secret string) ([]byte, bool) {
	expiryText, nonce, ok := strings.Cut(username, ":")
	if !ok || nonce == "" {
		return nil, false
	}
	expiry, err := strconv.ParseInt(expiryText, 10, 64)
	if err != nil || time.Now().Unix() >= expiry {
		return nil, false
	}
	mac := hmac.New(sha1.New, []byte(secret))
	_, _ = mac.Write([]byte(username))
	password := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return turn.GenerateAuthKey(username, realm, password), true
}

func startUDP(id string, local net.IP) (*endpoint, error) {
	packet, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	password, err := randomSecret()
	if err != nil {
		_ = packet.Close()
		return nil, err
	}
	ep := &endpoint{
		received: new(atomic.Uint64),
		authed:   make(map[string]struct{}),
	}
	// Shape each independent uplink in bursts so a fast loopback transfer spans
	// the scheduler's normal five-second lane exploration interval.
	socket := &countedSocket{
		PacketConn: packet,
		received:   ep.received,
		pacer:      &burstPacer{rate: fastRate, burst: fastBurst},
	}
	server, err := turn.NewServer(turn.ServerConfig{
		Realm: realm,
		AuthHandler: func(req *turn.RequestAttributes) (string, []byte, bool) {
			if req.Username != id {
				return "", nil, false
			}
			ep.recordAuth(req.Username)
			return id, turn.GenerateAuthKey(id, realm, password), true
		},
		PacketConnConfigs: []turn.PacketConnConfig{{
			PacketConn:            socket,
			PermissionHandler:     func(_ net.Addr, peer net.IP) bool { return peer.Equal(local) },
			RelayAddressGenerator: &turn.RelayAddressGeneratorStatic{RelayAddress: local, Address: local.String()},
		}},
	})
	if err != nil {
		_ = packet.Close()
		return nil, err
	}
	ep.server = server
	ep.credential = map[string]any{
		"id":       id,
		"kind":     "shared_static",
		"urls":     []string{"turn:" + socket.LocalAddr().String() + "?transport=udp"},
		"username": id,
		"password": password,
	}
	return ep, nil
}

func startTCP(id string, local net.IP, rate int64, burst, coturnTTL int) (*endpoint, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	secret, err := randomSecret()
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	ep := &endpoint{
		received: new(atomic.Uint64),
		authed:   make(map[string]struct{}),
	}
	counted := &countedListener{
		Listener: listener,
		received: ep.received,
		pacer:    &burstPacer{rate: rate, burst: burst},
	}
	authHandler := func(req *turn.RequestAttributes) (string, []byte, bool) {
		if coturnTTL > 0 {
			key, ok := verifyCoturn(req.Username, secret)
			if !ok {
				return "", nil, false
			}
			ep.recordAuth(req.Username)
			return req.Username, key, true
		}
		if req.Username != id {
			return "", nil, false
		}
		ep.recordAuth(req.Username)
		return id, turn.GenerateAuthKey(id, realm, secret), true
	}
	// Bind only a TCP TURN listener (no UDP PacketConnConfig) so clients cannot
	// silently fall back to UDP TURN during TCP-only verification.
	server, err := turn.NewServer(turn.ServerConfig{
		Realm:       realm,
		AuthHandler: authHandler,
		ListenerConfigs: []turn.ListenerConfig{{
			Listener:              counted,
			PermissionHandler:     func(_ net.Addr, peer net.IP) bool { return peer.Equal(local) },
			RelayAddressGenerator: &turn.RelayAddressGeneratorStatic{RelayAddress: local, Address: local.String()},
		}},
	})
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	ep.server = server
	url := "turn:" + listener.Addr().String() + "?transport=tcp"
	if coturnTTL > 0 {
		ep.credential = map[string]any{
			"id":            id,
			"kind":          "coturn",
			"urls":          []string{url},
			"shared_secret": secret,
			"ttl_seconds":   coturnTTL,
		}
	} else {
		ep.credential = map[string]any{
			"id":       id,
			"kind":     "shared_static",
			"urls":     []string{url},
			"username": id,
			"password": secret,
		}
	}
	return ep, nil
}

func run() error {
	// Ask the kernel for a routable local interface without sending any packets.
	probe, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 9})
	if err != nil {
		return err
	}
	local := probe.LocalAddr().(*net.UDPAddr).IP
	_ = probe.Close()

	services := make([]*endpoint, 0, 2)
	configs := make([]map[string]any, 0, 2)
	for _, id := range []string{"west", "east"} {
		s, err := startUDP(id, local)
		if err != nil {
			return err
		}
		defer s.server.Close()
		services = append(services, s)
		configs = append(configs, s.credential)
	}
	tcpOnly, err := startTCP("tcp", local, fastRate, fastBurst, 0)
	if err != nil {
		return err
	}
	defer tcpOnly.server.Close()

	// Pace the expiring TCP-only endpoint so a moderate transfer stays active across
	// the 45s renewal threshold of a 60s coturn grant without starving control frames.
	tcpRenew, err := startTCP("tcp-renew", local, renewalRate, renewalBurst, renewalTTL)
	if err != nil {
		return err
	}
	defer tcpRenew.server.Close()

	evidence, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	base := "http://" + evidence.Addr().String()
	readiness := map[string]any{
		"services":        configs,
		"stats":           base + "/",
		"tcpService":      tcpOnly.credential,
		"tcpStats":        base + "/tcp",
		"tcpRenewService": tcpRenew.credential,
		"tcpRenewStats":   base + "/tcp-renew",
	}
	if err := json.NewEncoder(os.Stdout).Encode(readiness); err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		result := make([]map[string]any, 0, len(services))
		for _, s := range services {
			result = append(result, s.stats(false))
		}
		_ = json.NewEncoder(w).Encode(result)
	})
	mux.HandleFunc("/tcp", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(tcpOnly.stats(true))
	})
	mux.HandleFunc("/tcp-renew", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(tcpRenew.stats(true))
	})
	server := http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	return server.Serve(evidence)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
