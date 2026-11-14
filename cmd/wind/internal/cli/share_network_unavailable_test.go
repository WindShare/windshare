package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/engine/relayconfig"
)

// A share without any usable relay is a setup condition the user can act on.
// It must not collapse into the generic "unexpected error" message.
func TestShareWithoutUsableRelayExplainsHowToRecover(t *testing.T) {
	root := filepath.Join(t.TempDir(), "selected.txt")
	if err := os.WriteFile(root, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		saved    relayconfig.Config
		args     []string
		wantExit int
	}{
		{"no relays configured", relayconfig.Config{FrontURL: "https://front.example"}, nil, ExitNetwork},
		{"configured relays only without any relay", relayconfig.Config{FrontURL: "https://front.example"}, []string{"--configured-relays-only"}, ExitUsage},
	} {
		t.Run(test.name, func(t *testing.T) {
			stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
			app := &App{Stdout: stdout, Stderr: stderr, Stdin: strings.NewReader(""),
				loadShareConfig: func() (relayconfig.Config, error) { return test.saved, nil }}
			t.Cleanup(app.closeTerminalOutput)
			code := app.Run(context.Background(), append([]string{"share", root}, test.args...))
			message := stderr.String()
			if code != test.wantExit || stdout.Len() != 0 {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), message)
			}
			if strings.Contains(message, "unexpected") || !strings.Contains(message, "--relay") || !strings.Contains(message, "wind config import") {
				t.Fatalf("diagnostic does not name a recovery path: %q", message)
			}
		})
	}
}

// The trace is the machine-readable record of a failed command: the network
// diagnostic must not suppress the terminal command_failed event.
func TestShareWithoutUsableRelayRecordsNetworkFailureInTrace(t *testing.T) {
	root := filepath.Join(t.TempDir(), "selected.txt")
	if err := os.WriteFile(root, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	tracePath := filepath.Join(t.TempDir(), "trace.ndjson")
	app := &App{Stdout: new(bytes.Buffer), Stderr: new(bytes.Buffer), Stdin: strings.NewReader(""),
		loadShareConfig: func() (relayconfig.Config, error) {
			return relayconfig.Config{FrontURL: "https://front.example"}, nil
		}}
	t.Cleanup(app.closeTerminalOutput)
	if code := app.Run(context.Background(), []string{"share", root, "--trace", tracePath}); code != ExitNetwork {
		t.Fatalf("exit = %d", code)
	}
	raw, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	type traceRecord struct {
		Event   string `json:"event"`
		Payload struct {
			ExitCode int `json:"exit_code"`
			Failure  struct {
				Code string `json:"code"`
			} `json:"failure"`
		} `json:"payload"`
	}
	var failed []traceRecord
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var record traceRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("trace line %q: %v", line, err)
		}
		if record.Event == "command_failed" {
			failed = append(failed, record)
		}
	}
	if len(failed) != 1 || failed[0].Payload.ExitCode != ExitNetwork || failed[0].Payload.Failure.Code != "relay_transport" {
		t.Fatalf("trace lacks the network command_failed event: %+v\n%s", failed, raw)
	}
}
