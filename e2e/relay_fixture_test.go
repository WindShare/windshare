package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/windshare/windshare/internal/testprocess"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

const (
	v2RelayComponent         = "relay_native"
	v2RelayListenAddress     = "127.0.0.1:0"
	v2RelayProxyEndpointPath = "/e2e/v2/ws"
)

var v2RelayReadinessLine = regexp.MustCompile(`(?m)^(\{[^\r\n]*\})\r?$`)

type v2RelayFixture struct {
	scenario       *v2Scenario
	database       string
	listenAddress  string
	publicEndpoint string
}

type v2RelayProcess struct {
	*v2Process
	address string
	baseURL string
}

type v2RelayReadiness struct {
	Event    string `json:"event"`
	Address  string `json:"address"`
	Endpoint string `json:"endpoint"`
}

type v2RelayReadyContext struct {
	Component string `json:"component"`
	Database  string `json:"database"`
	Address   string `json:"address"`
	Endpoint  string `json:"endpoint"`
}

func newV2RelayFixture(t *testing.T, scenario *v2Scenario, publicEndpoint string) *v2RelayFixture {
	t.Helper()
	return &v2RelayFixture{
		scenario:       scenario,
		database:       filepath.Join(t.TempDir(), "relay.sqlite"),
		listenAddress:  v2RelayListenAddress,
		publicEndpoint: publicEndpoint,
	}
}

func startV2Relay(t *testing.T, scenario *v2Scenario) *v2RelayProcess {
	t.Helper()
	return newV2RelayFixture(t, scenario, "").start(t)
}

func (fixture *v2RelayFixture) start(t *testing.T) *v2RelayProcess {
	t.Helper()
	arguments := []string{
		"--listen", fixture.listenAddress, "--database", fixture.database,
		// Every listener belongs to this scenario; default STUN ports would let
		// concurrent suites interfere even though file transfers use local peers.
		"--stun-udp", v2RelayListenAddress, "--stun-admin", v2RelayListenAddress,
	}
	if fixture.publicEndpoint != "" {
		arguments = append(arguments, "--public-endpoint", fixture.publicEndpoint)
	}
	process := startV2Process(t, fixture.scenario, v2RelayComponent, loadE2EBinaries(t).relay, arguments...)
	phase := fixture.scenario.startPhase(t, v2ProcessReadinessMilestone, v2ProcessPhaseContext{Component: v2RelayComponent})
	ctx, cancel := context.WithTimeout(t.Context(), v2ProcessTimeout)
	defer cancel()
	match, err := process.owned.WaitForOutput(ctx, testprocess.Stdout, v2RelayReadinessLine)
	if err != nil {
		t.Fatalf("native relay readiness: %v", err)
	}
	ready, baseURL, err := parseV2RelayReadiness(match[1])
	if err != nil {
		t.Fatalf("native relay readiness: %v; stdout=%q stderr=%q",
			err, process.stdout.diagnosticString(), process.stderr.diagnosticString())
	}
	if fixture.publicEndpoint == "" {
		// Direct restarts need the same socket as well as endpoint identity.
		// Proxied restarts may bind a fresh socket behind the stable public URL.
		fixture.listenAddress = ready.Address
		fixture.publicEndpoint = ready.Endpoint
	} else if ready.Endpoint != fixture.publicEndpoint {
		t.Fatalf("native relay endpoint changed: got %q, want %q", ready.Endpoint, fixture.publicEndpoint)
	}
	fixture.scenario.succeedPhase(t, phase, v2RelayReadyContext{
		Component: v2RelayComponent, Database: fixture.database,
		Address: ready.Address, Endpoint: ready.Endpoint,
	})
	return &v2RelayProcess{v2Process: process, address: ready.Address, baseURL: baseURL}
}

func parseV2RelayReadiness(line string) (v2RelayReadiness, string, error) {
	var ready v2RelayReadiness
	if err := json.Unmarshal([]byte(line), &ready); err != nil {
		return ready, "", fmt.Errorf("decode listening JSON: %w", err)
	}
	if ready.Event != "listening" {
		return ready, "", fmt.Errorf("unexpected relay event %q", ready.Event)
	}
	if err := validateV2RelayAddress(ready.Address); err != nil {
		return ready, "", err
	}
	baseURL, err := v2RelayBaseURL(ready.Endpoint)
	return ready, baseURL, err
}

func v2RelayBaseURL(endpoint string) (string, error) {
	base, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse relay endpoint: %w", err)
	}
	if !strings.HasSuffix(base.EscapedPath(), v2.WebSocketPath) {
		return "", fmt.Errorf("relay endpoint has no WebSocket path: %q", endpoint)
	}
	// The CLI appends the protocol path to its base URL. Preserve any proxy
	// prefix (including escapes) and verify the exact signed endpoint round trip.
	base.Path = strings.TrimSuffix(base.Path, v2.WebSocketPath)
	base.RawPath = strings.TrimSuffix(base.RawPath, v2.WebSocketPath)
	baseURL := base.String()
	normalized, err := v2.NormalizeRelayEndpoint(baseURL)
	if err != nil || normalized.DialURL != endpoint {
		return "", fmt.Errorf("relay endpoint is not canonical: %q (%v)", endpoint, err)
	}
	return baseURL, nil
}

func validateV2RelayAddress(address string) error {
	host, rawPort, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("listener-ready address %q: %w", address, err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65_535 {
		return fmt.Errorf("listener-ready address %q has invalid port", address)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("listener-ready address %q is not loopback", address)
	}
	return nil
}

func TestV2RelayReadinessPreservesPublicEndpoint(t *testing.T) {
	for _, endpoint := range []string{
		"ws://127.0.0.1:12345/v2/ws",
		"ws://[::1]:12345/e2e/v2/ws",
		"wss://relay.example/proxy%20prefix/v2/ws",
	} {
		line, err := json.Marshal(v2RelayReadiness{Event: "listening", Address: "127.0.0.1:43210", Endpoint: endpoint})
		if err != nil {
			t.Fatal(err)
		}
		ready, base, err := parseV2RelayReadiness(string(line))
		if err != nil || ready.Endpoint != endpoint || base != strings.TrimSuffix(endpoint, v2.WebSocketPath) {
			t.Fatalf("readiness = %+v, base=%q, err=%v", ready, base, err)
		}
	}
	for _, line := range []string{
		`{"event":"listening","address":"127.0.0.1:0","endpoint":"ws://127.0.0.1:12345/v2/ws"}`,
		`{"event":"listening","address":"0.0.0.0:12345","endpoint":"ws://127.0.0.1:12345/v2/ws"}`,
		`{"event":"stopped","address":"127.0.0.1:12345","endpoint":"ws://127.0.0.1:12345/v2/ws"}`,
		`{"event":"listening","address":"127.0.0.1:12345"}`,
		`{"event":"listening","address":"127.0.0.1:12345","endpoint":"ws://127.0.0.1:12345"}`,
		`{"event":"listening","address":"127.0.0.1:12345","endpoint":"http://127.0.0.1:12345/v2/ws"}`,
		`{"event":"listening"`,
	} {
		if _, _, err := parseV2RelayReadiness(line); err == nil {
			t.Fatalf("accepted invalid native readiness: %s", line)
		}
	}
}
