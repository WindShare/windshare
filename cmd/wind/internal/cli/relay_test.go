package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
	"github.com/windshare/windshare/engine/relaysetup"
)

func TestRelayCommandsInstallThroughEngineWithoutAdministratorAuthority(t *testing.T) {
	for _, operation := range []string{"add", "onboard"} {
		t.Run(operation, func(t *testing.T) {
			const relayURL = "https://relay.example"
			server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" {
					t.Error("client setup sent administrator authority")
				}
				var response any
				switch request.URL.Path {
				case "/.well-known/windshare":
					response = map[string]any{"version": 1, "access": "restricted", "relay_url": relayURL,
						"console_url": relayURL, "frontend_url": relayURL}
				case "/api/enrollments":
					var body map[string]string
					if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body["device_name"] != "CLI device" {
						t.Error("command did not pass the device label", err)
					}
					response = map[string]any{"enrollment_id": "enrollment", "user_code": "ABCD-EFGH",
						"verification_uri_complete": relayURL + "/?code=ABCD-EFGH",
						"expires_at":                time.Now().Add(time.Minute).UnixMilli(), "interval_ms": 5000}
				case "/api/enrollments/claim":
					response = map[string]string{"credential_id": "credential", "access_key": "cli-access-key-canary"}
				case "/api/enrollments/installed":
					response = map[string]bool{"ok": true}
				default:
					t.Error("unexpected setup endpoint", request.URL.Path)
					writer.WriteHeader(http.StatusNotFound)
					return
				}
				if err := json.NewEncoder(writer).Encode(response); err != nil {
					t.Error(err)
				}
			}))
			store := relayconfig.FileStore{Path: filepath.Join(t.TempDir(), "config.json")}
			initial := relayconfig.Config{FrontURL: "https://chosen.example",
				Profiles:     []relayprofile.Candidate{{PublicURL: "https://public.example"}},
				ActiveRelays: []string{"https://public.example"}}
			if err := store.Save(initial); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			app := &App{Stdout: &stdout, Stderr: &stderr, engineConfig: engine.Config{RelaySetup: relaysetup.Dependencies{
				Store: relaysetup.FileStore{Settings: store}, HTTP: server.Client(),
				OpenBrowser: func(_ context.Context, _ string) error {
					t.Error("headless command opened a browser")
					return nil
				},
			}}}
			t.Cleanup(app.closeTerminalOutput)
			if code := app.Run(t.Context(), []string{"relay", operation, relayURL, "--name", "CLI device", "--no-browser"}); code != ExitOK {
				t.Fatal("setup failed", code, stderr.String())
			}
			saved, err := store.Load()
			if err != nil || len(saved.Profiles) != 2 ||
				saved.Profiles[1].AccessKey != "cli-access-key-canary" || saved.FrontURL != initial.FrontURL {
				t.Fatal("setup did not persist a complete profile", err)
			}
			expectedSelection := initial.ActiveRelays
			if operation == "onboard" {
				expectedSelection = []string{relayURL}
			}
			if !slices.Equal(saved.ActiveRelays, expectedSelection) {
				t.Fatalf("selection = %v, expected %v", saved.ActiveRelays, expectedSelection)
			}
			if stdout.Len() != 0 || strings.Contains(stderr.String(), "cli-access-key-canary") {
				t.Fatal("command exposed credentials or wrote to the capability stream")
			}
		})
	}
}

func TestRelayFlagHelpAlignsOptionsWithoutEscapedTabs(t *testing.T) {
	for _, operation := range []string{"add", "onboard"} {
		t.Run(operation, func(t *testing.T) {
			for _, test := range []struct {
				name string
				args []string
			}{
				{name: "url --help", args: []string{"https://relay.example", "--help"}},
				{name: "url -h", args: []string{"https://relay.example", "-h"}},
				{name: "--help", args: []string{"--help"}},
				{name: "-h", args: []string{"-h"}},
			} {
				t.Run(test.name, func(t *testing.T) {
					help := runRelayFlagHelp(t, append([]string{"relay", operation}, test.args...))
					if strings.Contains(help, `\t`) || strings.Contains(help, "\t") ||
						!strings.Contains(help, "\n  -no-browser\n        display the approval URL without opening a browser\n") ||
						!strings.Contains(help, "\n  -name string\n        device name displayed for browser approval (default ") {
						t.Fatalf("relay help does not align options: %q", help)
					}
					// Build the <url> baseline here so the comparison also runs under -run filters.
					if withURL := runRelayFlagHelp(t, []string{"relay", operation, "https://relay.example", "--help"}); help != withURL {
						t.Fatalf("help = %q, want the <url> form %q", help, withURL)
					}
				})
			}
		})
	}
}

func runRelayFlagHelp(t *testing.T, args []string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	code := app.Run(t.Context(), args)
	app.closeTerminalOutput()
	if code != ExitUsage || stdout.Len() != 0 {
		t.Fatalf("%q: exit=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
	}
	return stderr.String()
}
