package testrelaynative

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

const maximumPort = 65_535

var readinessLine = regexp.MustCompile(`(?m)^(\{[^\r\n]*\})\r?$`)

type Readiness struct {
	Address  string `json:"address"`
	Endpoint string `json:"endpoint"`
	BaseURL  string `json:"-"`
}

func parseReadiness(line string) (Readiness, error) {
	var event struct {
		Event string `json:"event"`
		Readiness
	}
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		return Readiness{}, fmt.Errorf("decode listening JSON: %w", err)
	}
	if event.Event != "listening" {
		return Readiness{}, fmt.Errorf("unexpected relay event %q", event.Event)
	}
	if err := ValidateListenerAddress(event.Address); err != nil {
		return Readiness{}, err
	}
	baseURL, err := relayBaseURL(event.Endpoint)
	if err != nil {
		return Readiness{}, err
	}
	event.BaseURL = baseURL
	return event.Readiness, nil
}

func relayBaseURL(endpoint string) (string, error) {
	base, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse relay endpoint: %w", err)
	}
	if !strings.HasSuffix(base.EscapedPath(), v2.WebSocketPath) {
		return "", fmt.Errorf("relay endpoint has no WebSocket path: %q", endpoint)
	}
	// Clients append the protocol path. Preserve proxy prefixes and escapes so
	// the dial URL round-trips to the exact endpoint used by signed proofs.
	base.Path = strings.TrimSuffix(base.Path, v2.WebSocketPath)
	base.RawPath = strings.TrimSuffix(base.RawPath, v2.WebSocketPath)
	baseURL := base.String()
	normalized, err := v2.NormalizeRelayEndpoint(baseURL)
	if err != nil || normalized.DialURL != endpoint {
		return "", fmt.Errorf("relay endpoint is not canonical: %q (%v)", endpoint, err)
	}
	return baseURL, nil
}

// ValidateListenerAddress also confines test proxies to an actual loopback port.
func ValidateListenerAddress(address string) error {
	host, rawPort, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("listener-ready address %q: %w", address, err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > maximumPort {
		return fmt.Errorf("listener-ready address %q has invalid port", address)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("listener-ready address %q is not loopback", address)
	}
	return nil
}
