package testrelaynative

import (
	"encoding/json"
	"strings"
	"testing"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

func TestReadinessPreservesPublicEndpoint(t *testing.T) {
	for _, endpoint := range []string{
		"ws://127.0.0.1:12345/v2/ws",
		"ws://[::1]:12345/e2e/v2/ws",
		"wss://relay.example/proxy%20prefix/v2/ws",
	} {
		line, err := json.Marshal(map[string]string{
			"event": "listening", "address": "127.0.0.1:43210", "endpoint": endpoint,
		})
		if err != nil {
			t.Fatal(err)
		}
		// Native stdout may use CRLF; unrelated non-JSON output is not readiness.
		match := readinessLine.FindStringSubmatch("starting\r\n" + string(line) + "\r\n")
		if len(match) != 2 {
			t.Fatalf("listening event not found in %q", line)
		}
		ready, err := parseReadiness(match[1])
		if err != nil || ready.Endpoint != endpoint || ready.BaseURL != strings.TrimSuffix(endpoint, v2.WebSocketPath) {
			t.Fatalf("readiness = %+v, err=%v", ready, err)
		}
	}
	for _, line := range []string{
		`{"event":"listening","address":"127.0.0.1:0","endpoint":"ws://127.0.0.1:12345/v2/ws"}`,
		`{"event":"listening","address":"0.0.0.0:12345","endpoint":"ws://127.0.0.1:12345/v2/ws"}`,
		`{"event":"listening","address":"127.0.0.1:65536","endpoint":"ws://127.0.0.1:12345/v2/ws"}`,
		`{"event":"listening","address":"invalid","endpoint":"ws://127.0.0.1:12345/v2/ws"}`,
		`{"event":"stopped","address":"127.0.0.1:12345","endpoint":"ws://127.0.0.1:12345/v2/ws"}`,
		`{"event":"listening","address":"127.0.0.1:12345"}`,
		`{"event":"listening","address":"127.0.0.1:12345","endpoint":"ws://127.0.0.1:12345"}`,
		`{"event":"listening","address":"127.0.0.1:12345","endpoint":"http://127.0.0.1:12345/v2/ws"}`,
		`{"event":"listening","address":"127.0.0.1:12345","endpoint":"ws://%invalid/v2/ws"}`,
		`{"event":"listening"`,
	} {
		if _, err := parseReadiness(line); err == nil {
			t.Fatalf("accepted invalid native readiness: %s", line)
		}
	}
}
