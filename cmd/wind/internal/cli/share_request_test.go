package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayprofile"
	"github.com/windshare/windshare/internal/diaglog"
)

func TestShareRequestOwnsVerboseAndTraceOptionsAcrossInterleavedArguments(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "share.ndjson")
	app := &App{Stderr: io.Discard}
	t.Cleanup(app.closeTerminalOutput)
	request, parse := app.parseShareRequest([]string{
		"selected-one", "-v", "selected-two", "--trace", tracePath,
		"--front-url", "https://front.example", "--relay", "https://relay.example",
	})
	target, targetErr := request.observation.traceTarget()
	expected, _ := diaglog.ExactFile(tracePath)
	if parse != requestParseReady || !request.observation.verbose || targetErr != nil || target != expected ||
		len(request.paths) != 2 || request.paths[0] != "selected-one" || request.paths[1] != "selected-two" {
		t.Fatalf("share request = %#v, parse = %d", request, parse)
	}
}

func TestShareRequestRejectsTraceOnCapabilityStream(t *testing.T) {
	app := &App{Stderr: io.Discard}
	t.Cleanup(app.closeTerminalOutput)
	if _, parse := app.parseShareRequest([]string{"selected", "--trace=-"}); parse != requestParseUsageFailure {
		t.Fatalf("trace stdout parse = %d, want %d", parse, requestParseUsageFailure)
	}
	if _, parse := app.parseShareRequest([]string{"selected", "--trace-dir=-"}); parse != requestParseUsageFailure {
		t.Fatalf("trace directory stdout parse = %d, want %d", parse, requestParseUsageFailure)
	}
	if _, parse := app.parseShareRequest([]string{
		"selected", "--trace", "one.ndjson", "--trace-dir", "traces",
	}); parse != requestParseUsageFailure {
		t.Fatalf("conflicting target parse = %d, want %d", parse, requestParseUsageFailure)
	}
}

func TestShareAliasPublishesCapabilityLink(t *testing.T) {
	workDir := t.TempDir()
	for _, name := range []string{"notes.txt", "config", "share", "-weird"} {
		if err := os.WriteFile(filepath.Join(workDir, name), []byte("hello"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(workDir)

	tests := []struct {
		name       string
		args       []string
		wantPrefix string
		wantKey    bool
	}{
		{
			name:       "bare file",
			args:       []string{"s", "notes.txt", "--relay", "https://relay.example", "--front-url", "https://front.example"},
			wantPrefix: "Link: https://front.example/",
		},
		{
			name:       "leading and interleaved options",
			args:       []string{"s", "--split-key", "-v", "notes.txt", "--relay", "https://relay.example", "--front-url", "https://front.example"},
			wantPrefix: "Bare link: https://front.example/",
			wantKey:    true,
		},
		{
			name:       "subcommand-named paths",
			args:       []string{"s", "share", "config", "--relay", "https://relay.example", "--front-url", "https://front.example"},
			wantPrefix: "Link: https://front.example/",
		},
		{
			name:       "dash-prefixed path after option terminator",
			args:       []string{"s", "--relay", "https://relay.example", "--front-url", "https://front.example", "--", "-weird"},
			wantPrefix: "Link: https://front.example/",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			relays := &cliShareRelays{
				stopped:        make(chan struct{}),
				cleanupStarted: make(chan context.Context, 1),
				cleanupRelease: make(chan struct{}),
			}
			close(relays.cleanupRelease)

			var stdout bytes.Buffer
			stderr := &lockedTestBuffer{}
			app := &App{
				Stdout: &cancelOnWriteBuffer{buf: &stdout, cancel: cancel},
				Stderr: stderr,
				Stdin:  strings.NewReader(""),
				engineConfig: engine.Config{Share: engine.ShareDependencies{
					Relays:      func(context.Context, []relayprofile.Registration) (engine.ShareRelays, error) { return relays, nil },
					PollNetwork: func(ctx context.Context, _ func()) { <-ctx.Done() },
				}},
			}

			code := app.Run(ctx, test.args)
			app.closeTerminalOutput()
			if code != ExitOK {
				t.Fatalf("args=%v exit=%d stderr=%q stdout=%q", test.args, code, stderr.String(), stdout.String())
			}
			published := stdout.String()
			if !strings.HasPrefix(published, test.wantPrefix) {
				t.Fatalf("stdout=%q want prefix %q", published, test.wantPrefix)
			}
			if test.wantKey && !strings.Contains(published, "\nKey: ") {
				t.Fatalf("split-key stdout=%q missing key line", published)
			}
		})
	}
}
