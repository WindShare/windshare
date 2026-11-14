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

func TestShareWithoutConfiguredCandidatesIsUsageFailure(t *testing.T) {
	for _, test := range []struct {
		name  string
		saved relayconfig.Config
		args  []string
	}{
		{"command policy", relayconfig.Config{}, []string{"--configured-relays-only"}},
		{"saved policy", relayconfig.Config{ConfiguredOnly: true}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
			app := &App{Stdout: stdout, Stderr: stderr, Stdin: strings.NewReader(""),
				loadShareConfig: func() (relayconfig.Config, error) { return test.saved, nil }}
			t.Cleanup(app.closeTerminalOutput)
			code := app.Run(context.Background(), append([]string{"share", t.TempDir()}, test.args...))
			if code != ExitUsage || stdout.Len() != 0 {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

// Process status and trace are one contract, even when startup fails before the
// engine can create a task. Neither command is allowed to lose the final event.
func TestNetworkUnavailableStartupPreservesStatusAndTrace(t *testing.T) {
	capability := newGetOutputPreparationCapability(t, "https://relay.example")
	capability.Relays = nil
	capability.NodeAliases = []string{"jp1"}
	encoded, err := capability.URL("https://front.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		command string
		args    []string
	}{
		{"share", []string{t.TempDir()}},
		{"get", []string{encoded, "-o", t.TempDir()}},
	} {
		t.Run(test.command, func(t *testing.T) {
			tracePath := filepath.Join(t.TempDir(), "trace.ndjson")
			stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
			app := &App{Stdout: stdout, Stderr: stderr, Stdin: strings.NewReader("")}
			t.Cleanup(app.closeTerminalOutput)
			args := append([]string{test.command}, test.args...)
			args = append(args, "--trace", tracePath)
			if code := app.Run(context.Background(), args); code != ExitNetwork || stdout.Len() != 0 {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			raw, err := os.ReadFile(tracePath)
			if err != nil {
				t.Fatal(err)
			}
			type traceRecord struct {
				Event   string `json:"event"`
				Command string `json:"command"`
				Payload struct {
					ExitCode int `json:"exit_code"`
					Failure  struct {
						Code       string `json:"code"`
						MessageKey string `json:"message_key"`
					} `json:"failure"`
				} `json:"payload"`
			}
			var failed []traceRecord
			for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
				var record traceRecord
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatalf("trace line %q: %v", line, err)
				}
				if record.Event == "command_failed" {
					failed = append(failed, record)
				}
			}
			if len(failed) != 1 || failed[0].Command != test.command || failed[0].Payload.ExitCode != ExitNetwork ||
				failed[0].Payload.Failure.Code != "network_unavailable" || failed[0].Payload.Failure.MessageKey != "network_unavailable" {
				t.Fatalf("trace lost the startup failure: %+v\n%s", failed, raw)
			}
		})
	}
}
