package publicdial

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestDialPinsEachLookupBeforeConnecting(t *testing.T) {
	public := netip.MustParseAddr("1.1.1.1")
	answer := public
	var dialed []string
	p := Policy{
		Lookup: func(context.Context, string, string) ([]netip.Addr, error) { return []netip.Addr{answer}, nil },
		Dial: func(_ context.Context, _ string, address string) (net.Conn, error) {
			dialed = append(dialed, address)
			return nil, errors.New("test dial")
		},
	}
	_, _ = p.dial(t.Context(), "tcp", "relay.example:443")
	answer = netip.MustParseAddr("127.0.0.1")
	_, err := p.dial(t.Context(), "tcp", "relay.example:443")
	if !errors.Is(err, ErrAddress) || len(dialed) != 1 || dialed[0] != "1.1.1.1:443" {
		t.Fatal(dialed, err)
	}
	if p.Permits(netip.Addr{}) {
		t.Fatal("zero Addr must never be permitted")
	}
	customCalled := false
	custom := Policy{Permit: func(netip.Addr) bool { customCalled = true; return true }}
	if custom.Permits(netip.Addr{}) || custom.Permits(netip.MustParseAddr("::ffff:1.1.1.1%eth0")) || customCalled {
		t.Fatal("invalid or zoned addresses must be rejected before custom Permit")
	}
}

func TestOriginAndRelayOriginValidation(t *testing.T) {
	p := Policy{}
	validOrigins := map[string]string{
		"https://relay.example":                "https://relay.example",
		"https://RELAY.Example:443/":           "https://relay.example",
		"https://relay.example:8443/":          "https://relay.example:8443",
		"https://1.1.1.1":                      "https://1.1.1.1",
		"https://[::ffff:1.1.1.1]":             "https://1.1.1.1",
		"https://[2606:4700:4700::1111]:8443/": "https://[2606:4700:4700::1111]:8443",
		"https://cn1.ourworld.top.":            "https://cn1.ourworld.top.",
	}
	for raw, want := range validOrigins {
		got, err := p.Origin(raw)
		if err != nil || got.String() != want {
			t.Fatalf("Origin(%q) = %v, %v; want %q", raw, got, err, want)
		}
		gotRelay, err := p.RelayOrigin(raw)
		if err != nil || gotRelay.String() != want {
			t.Fatalf("RelayOrigin(%q) = %v, %v; want %q", raw, gotRelay, err, want)
		}
	}

	invalidBoth := []string{
		"",
		"http://1.1.1.1",
		"https://127.0.0.1",
		"https://[::ffff:127.0.0.1]",
		"https://[2001:db8::1]",
		"https://192.0.2.1",
		"https://[fe80::1%25eth0]",
		"https://user@example.org",
		"https://localhost",
		"https://localhost.",
		"https://..",
		"https://.example",
		"https://a..b",
		"https://-bad.example",
		"https://127.1",
		"https://0x7f.0.0.1",
		"https://1.1",
		"https://relay.123",
		"https://[1.1.1.1]",
		"https://[not-an-ip]",
		"https://example.org?",
		"https://example.org?k=v",
		"https://example.org#",
		"https://example.org#frag",
		"https://example.org:",
		"https://[2606:4700:4700::1111]:",
		"https://example.org:0",
		"https://example.org:65536",
		"https://example.org:abc",
		" https://example.org",
		"https://example.org\n",
		"\ufeffhttps://example.org",
		`https://example.org\foo`,
		"https://example.org/base/../secret",
		"https://example.org/.",
	}
	for _, raw := range invalidBoth {
		if _, err := p.Origin(raw); !errors.Is(err, ErrAddress) {
			t.Fatalf("Origin(%q) unexpectedly accepted: %v", raw, err)
		}
		if _, err := p.RelayOrigin(raw); !errors.Is(err, ErrAddress) {
			t.Fatalf("RelayOrigin(%q) unexpectedly accepted: %v", raw, err)
		}
	}

	subpathCases := map[string]string{
		"https://relay.example/base":           "https://relay.example",
		"https://relay.example/team/mount/":    "https://relay.example",
		"https://RELAY.Example:8443/windshare": "https://relay.example:8443",
		"https://relay.example//":              "https://relay.example",
		"https://relay.example/%2f":            "https://relay.example",
	}
	for raw, want := range subpathCases {
		if _, err := p.Origin(raw); !errors.Is(err, ErrAddress) {
			t.Fatalf("Origin(%q) must reject non-root path, got err=%v", raw, err)
		}
		got, err := p.RelayOrigin(raw)
		if err != nil || got.String() != want {
			t.Fatalf("RelayOrigin(%q) = %v, %v; want %q", raw, got, err, want)
		}
	}
}

func TestSelectDialCandidatesBalancesFamiliesAndCapsFanout(t *testing.T) {
	p := Policy{}
	v6 := []netip.Addr{
		netip.MustParseAddr("2606:4700:4700::1"),
		netip.MustParseAddr("2606:4700:4700::2"),
		netip.MustParseAddr("2606:4700:4700::3"),
		netip.MustParseAddr("2606:4700:4700::4"),
		netip.MustParseAddr("2606:4700:4700::5"),
	}
	v4 := []netip.Addr{
		netip.MustParseAddr("1.1.1.1"),
		netip.MustParseAddr("1.0.0.1"),
		netip.MustParseAddr("8.8.8.8"),
		netip.MustParseAddr("8.8.4.4"),
	}
	input := []netip.Addr{
		netip.MustParseAddr("127.0.0.1"),
		netip.MustParseAddr("2001:db8::1"),
		netip.MustParseAddr("::ffff:1.1.1.1%eth0"),
		v6[0], v6[0], v6[1], v6[2], v6[3], v6[4],
		netip.MustParseAddr("::ffff:1.1.1.1"), // deduplicates with v4[0] after unmap
		v4[0], v4[1], v4[2], v4[3],
	}
	got := selectDialCandidates(input, p.Permits)
	want := []netip.Addr{v6[0], v4[0], v6[1], v4[1], v6[2], v4[2]}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	v4First := selectDialCandidates([]netip.Addr{v4[0], v4[1], v6[0]}, p.Permits)
	wantV4First := []netip.Addr{v4[0], v6[0], v4[1]}
	if !slices.Equal(v4First, wantV4First) {
		t.Fatalf("v4-first got %v, want %v", v4First, wantV4First)
	}
}

func TestDialStaggeredFallbackRecoversFromBlackholedIPv6(t *testing.T) {
	v6 := netip.MustParseAddr("2606:4700:4700::1111")
	v4 := netip.MustParseAddr("1.1.1.1")
	v6Canceled := make(chan struct{}, 1)
	var attempts []string
	var mu sync.Mutex

	p := Policy{
		FallbackDelay: 15 * time.Millisecond,
		Lookup: func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{v6, v4}, nil
		},
		Dial: func(ctx context.Context, _ string, address string) (net.Conn, error) {
			mu.Lock()
			attempts = append(attempts, address)
			mu.Unlock()
			if strings.HasPrefix(address, "[") {
				<-ctx.Done()
				v6Canceled <- struct{}{}
				return nil, ctx.Err()
			}
			c1, c2 := net.Pipe()
			_ = c2.Close()
			return c1, nil
		},
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	conn, err := p.dial(ctx, "tcp", "dual.example:443")
	if err != nil {
		t.Fatalf("expected IPv4 fallback to succeed, got %v", err)
	}
	_ = conn.Close()

	select {
	case <-v6Canceled:
	case <-time.After(time.Second):
		t.Fatal("blackholed IPv6 dial was not canceled after IPv4 won")
	}

	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(attempts, []string{"[2606:4700:4700::1111]:443", "1.1.1.1:443"}) {
		t.Fatalf("unexpected dial sequence: %v", attempts)
	}
}

func TestDialStaggeredAdvancesImmediatelyOnFastFailureAndClosesLoser(t *testing.T) {
	v6 := netip.MustParseAddr("2606:4700:4700::1111")
	v4 := netip.MustParseAddr("1.1.1.1")

	// Fast failure on primary must advance immediately without waiting for FallbackDelay.
	fast := Policy{
		FallbackDelay: 5 * time.Second,
		Lookup: func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{v6, v4}, nil
		},
		Dial: func(_ context.Context, _ string, address string) (net.Conn, error) {
			if strings.HasPrefix(address, "[") {
				return nil, errors.New("network unreachable")
			}
			c1, c2 := net.Pipe()
			_ = c2.Close()
			return c1, nil
		},
	}
	start := time.Now()
	conn, err := fast.dial(t.Context(), "tcp", "fast.example:443")
	if err != nil {
		t.Fatalf("fast fallback failed: %v", err)
	}
	_ = conn.Close()
	if time.Since(start) > time.Second {
		t.Fatalf("fast failure waited for fallback delay: %v", time.Since(start))
	}

	// If a losing dial returns a connection after a winner claims victory, close the loser.
	var loserClosed atomic.Bool
	loserDone := make(chan struct{})
	releaseLoser := make(chan struct{})
	race := Policy{
		FallbackDelay: 10 * time.Millisecond,
		Lookup: func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{v6, v4}, nil
		},
		Dial: func(_ context.Context, _ string, address string) (net.Conn, error) {
			c1, c2 := net.Pipe()
			_ = c2.Close()
			if strings.HasPrefix(address, "[") {
				<-releaseLoser
				return &closeTrackingConn{Conn: c1, onClose: func() {
					loserClosed.Store(true)
					close(loserDone)
				}}, nil
			}
			return c1, nil
		},
	}
	winner, err := race.dial(t.Context(), "tcp", "race.example:443")
	if err != nil {
		t.Fatalf("race dial failed: %v", err)
	}
	_ = winner.Close()
	close(releaseLoser)
	select {
	case <-loserDone:
	case <-time.After(time.Second):
		t.Fatal("losing connection was not closed")
	}
	if !loserClosed.Load() {
		t.Fatal("expected loserClosed to be true")
	}
}

type closeTrackingConn struct {
	net.Conn
	once    sync.Once
	onClose func()
}

func (c *closeTrackingConn) Close() error {
	c.once.Do(c.onClose)
	return c.Conn.Close()
}

func TestClientEnforcesHTTPSOnlyAndSupportsTLSConfigAndIdleClose(t *testing.T) {
	var dialed atomic.Int32
	p := Policy{
		Lookup: func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("1.1.1.1")}, nil
		},
		Dial: func(context.Context, string, string) (net.Conn, error) {
			dialed.Add(1)
			return nil, errors.New("unexpected dial")
		},
	}
	client := p.Client()
	if _, err := client.Get("http://1.1.1.1/discovery/v1/peers"); !errors.Is(err, ErrAddress) {
		t.Fatalf("expected ErrAddress for http://, got %v", err)
	}
	if _, _, err := websocket.Dial(t.Context(), "ws://1.1.1.1/v2/ws", &websocket.DialOptions{HTTPClient: client}); !errors.Is(err, ErrAddress) {
		t.Fatalf("expected ErrAddress for ws://, got %v", err)
	}
	if dialed.Load() != 0 {
		t.Fatalf("cleartext request reached dialer %d times", dialed.Load())
	}

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "https://"))
	local := netip.MustParseAddr("127.0.0.1")
	tlsClient := Policy{
		Permit:          func(a netip.Addr) bool { return a == local },
		Lookup:          func(context.Context, string, string) ([]netip.Addr, error) { return []netip.Addr{local}, nil },
		TLSClientConfig: server.Client().Transport.(*http.Transport).TLSClientConfig,
	}.Client()
	resp, err := tlsClient.Get("https://example.com:" + port + "/health")
	if err != nil {
		t.Fatalf("HTTPS request failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("unexpected status %d", resp.StatusCode)
	}
	if _, ok := tlsClient.Transport.(interface{ CloseIdleConnections() }); !ok {
		t.Fatal("wrapped transport must forward CloseIdleConnections")
	}
	tlsClient.CloseIdleConnections()
}
