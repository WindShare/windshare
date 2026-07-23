package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/testnetwork"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

func TestRunServesOnlyV2AndShutsDownGracefully(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{
			"-listen", "127.0.0.1:0", "-state-dir", t.TempDir(),
		}, func(address net.Addr) { ready <- address }, t.Logf)
	}()
	var address net.Addr
	select {
	case address = <-ready:
	case err := <-done:
		t.Fatalf("run exited before ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("relay did not become ready")
	}

	response, err := http.Get(fmt.Sprintf("http://%s/healthz", address))
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || response.StatusCode != http.StatusOK || string(body) != "ok\n" {
		t.Fatalf("health response = %d %q, read=%v close=%v", response.StatusCode, body, readErr, closeErr)
	}
	response, err = http.Get(fmt.Sprintf("http://%s/v1/config", address))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("retired v1 route status = %d", response.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("graceful shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("graceful shutdown did not complete")
	}
}

func TestServerPolicyBuildsHardenedServer(t *testing.T) {
	policy := serverPolicy{
		readHeaderTimeout: 2 * time.Second, readTimeout: 3 * time.Second,
		idleTimeout: 4 * time.Second, maximumHeader: 1234,
	}
	if err := policy.validate(); err != nil {
		t.Fatal(err)
	}
	server := policy.newServer(http.NewServeMux())
	if server.ReadHeaderTimeout != policy.readHeaderTimeout || server.ReadTimeout != policy.readTimeout ||
		server.IdleTimeout != policy.idleTimeout || server.MaxHeaderBytes != policy.maximumHeader {
		t.Fatalf("server = %+v", server)
	}
}

func TestPublicRelayEndpointUsesActualListenerAndConfiguredIdentity(t *testing.T) {
	derived, err := publicRelayEndpoint("", stringAddress("127.0.0.1:49231"))
	if err != nil {
		t.Fatal(err)
	}
	if derived.DialURL != "ws://127.0.0.1:49231/v2/ws" {
		t.Fatalf("derived endpoint = %+v", derived)
	}
	configured, err := publicRelayEndpoint("https://Relay.Example:443/base?token=one", stringAddress("127.0.0.1:1"))
	if err != nil {
		t.Fatal(err)
	}
	if configured.DialURL != "wss://relay.example/base/v2/ws?token=one" ||
		configured.IdentityURL != "wss://relay.example/base/v2/ws" {
		t.Fatalf("configured endpoint = %+v", configured)
	}
}

func TestResolveStateDirectorySeparatesRelayIdentities(t *testing.T) {
	first := v2.RelayIdentity{1}
	second := v2.RelayIdentity{2}
	left, err := resolveStateDirectory("", first)
	if err != nil {
		t.Fatal(err)
	}
	right, err := resolveStateDirectory("", second)
	if err != nil {
		t.Fatal(err)
	}
	if left == right || filepath.Base(left) == filepath.Base(right) {
		t.Fatalf("identity directories collided: %q %q", left, right)
	}
	explicit := filepath.Join(t.TempDir(), "state")
	resolved, err := resolveStateDirectory(explicit, first)
	if err != nil || !filepath.IsAbs(resolved) {
		t.Fatalf("explicit state directory = %q, %v", resolved, err)
	}
}

func TestRunRejectsInvalidAndRetiredFlags(t *testing.T) {
	tests := [][]string{
		{"-definitely-not-a-flag"},
		{"-listen", "999.999.999.999:1"},
		{"-listen", "127.0.0.1:0", "-relay-base-url", "not a URL"},
		{"-listen", "127.0.0.1:0", "-max-routes", "0"},
		{"-listen", "127.0.0.1:0", "-max-sessions", "2", "-max-sessions-per-share", "3"},
		{"-listen", "127.0.0.1:0", "-max-connections", "1", "-max-connections-per-source", "2"},
		{"-listen", "127.0.0.1:0", "-http-read-header-timeout", "5s", "-http-read-timeout", "1s"},
		{"-listen", "127.0.0.1:0", "-max-manifest-bytes", "1"},
		{"-listen", "127.0.0.1:0", "-sender-grace", "1s"},
	}
	for _, arguments := range tests {
		if err := run(context.Background(), arguments, nil, t.Logf); err == nil {
			t.Fatalf("invalid policy was accepted: %v", arguments)
		}
	}
}

type stringAddress string

func (address stringAddress) Network() string { return "tcp" }
func (address stringAddress) String() string  { return string(address) }

func TestProductionSourceContainsNoV1RouteOrManifestFlag(t *testing.T) {
	for _, retired := range []string{"/v1", "max-manifest", "sender-grace"} {
		if strings.Contains(strings.ToLower(sourceText(t)), retired) {
			t.Fatalf("production relay source still contains %q", retired)
		}
	}
}

func sourceText(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
