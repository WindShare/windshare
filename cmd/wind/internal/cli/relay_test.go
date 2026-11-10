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
	"sync/atomic"
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

func TestRelayHelpCompletesBeforeOperation(t *testing.T) {
	const relayURL = "https://relay.example"
	for _, operation := range []string{"add", "onboard", "cancel"} {
		for _, help := range []string{"-h", "-help", "--h", "--help", "--help=1"} {
			for _, args := range [][]string{
				{operation, help},
				{operation, relayURL, help},
				{operation, help, relayURL},
			} {
				t.Run(strings.Join(args, " "), func(t *testing.T) {
					var stdout, stderr bytes.Buffer
					// There is deliberately no engine: help must finish before
					// enrollment or cancellation can acquire external authority.
					app := &App{Stdout: &stdout, Stderr: &stderr}
					t.Cleanup(app.closeTerminalOutput)
					code := app.runRelay(t.Context(), args)
					if code != ExitOK || stdout.Len() != 0 || stderr.Len() == 0 {
						t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
					}
				})
			}
		}
	}
}

func TestRelaySetupOptionsAreInterleaved(t *testing.T) {
	const relayURL = "https://relay.example"
	for _, operation := range []string{"add", "onboard"} {
		for _, args := range [][]string{
			{relayURL, "--name", "device", "--no-browser"},
			{"--name", "device", relayURL, "--no-browser"},
			{"--no-browser", "--name=device", relayURL},
		} {
			t.Run(operation+"/"+strings.Join(args, " "), func(t *testing.T) {
				app := &App{}
				request, outcome := app.parseRelaySetupRequest(operation, args)
				wantOperation := relaysetup.AddProfile
				if operation == "onboard" {
					wantOperation = relaysetup.PrivateOnboarding
				}
				if outcome != requestParseReady || request.URL != relayURL ||
					request.DeviceName != "device" || !request.NoBrowser || request.Operation != wantOperation {
					t.Fatalf("outcome=%v request=%+v", outcome, request)
				}
			})
		}
	}

	app := &App{}
	request, outcome := app.parseRelaySetupRequest("add", []string{"--name", "--help", relayURL})
	if outcome != requestParseReady || request.DeviceName != "--help" {
		t.Fatalf("a flag value became a help request: outcome=%v request=%+v", outcome, request)
	}
}

func TestRelayArgumentErrorsCompleteBeforeOperation(t *testing.T) {
	const rejectedValue = "relay_argument_CANARY"
	for _, args := range [][]string{
		{"add"},
		{"onboard", ""},
		{"cancel"},
		{"cancel", "https://relay.example", "extra"},
		{"add", "--name"},
		{"add", "--no-browser=" + rejectedValue},
		{"onboard", "--" + rejectedValue},
		{"cancel", "--no-browser"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stderr bytes.Buffer
			app := &App{Stderr: &stderr}
			t.Cleanup(app.closeTerminalOutput)
			if code := app.runRelay(t.Context(), args); code != ExitUsage || stderr.Len() == 0 {
				t.Fatalf("exit=%d stderr=%q", code, stderr.String())
			}
			if strings.Contains(stderr.String(), rejectedValue) {
				t.Fatalf("parser projected a rejected value: %q", stderr.String())
			}
		})
	}
}

func newRelayTestApp(t *testing.T, client *http.Client) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	store := relayconfig.FileStore{Path: filepath.Join(t.TempDir(), "config.json")}
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr, engineConfig: engine.Config{RelaySetup: relaysetup.Dependencies{
		Store: relaysetup.FileStore{Settings: store}, HTTP: client,
	}}}
	t.Cleanup(app.closeTerminalOutput)
	return app, &stdout, &stderr
}

func TestRelayCancelWithNothingPendingIsAUsageError(t *testing.T) {
	for _, address := range []string{"https://relay.example", "not a relay"} {
		t.Run(address, func(t *testing.T) {
			app, stdout, stderr := newRelayTestApp(t, nil)
			code := app.Run(t.Context(), []string{"relay", "cancel", address})
			const want = "relay cancel: no relay setup is pending; nothing was cancelled\n"
			if code != ExitUsage || stderr.String() != want || stdout.Len() != 0 {
				t.Fatalf("exit=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
			}
		})
	}
}

func TestInterruptedRelaySetupHintMatchesWhatWasSaved(t *testing.T) {
	const relayURL = "https://relay.example"
	for _, scenario := range []string{"discovery", "approval"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, interrupt := context.WithCancel(t.Context())
			defer interrupt()
			var cancelled atomic.Bool
			released := make(chan struct{})
			server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var response any
				switch {
				case request.URL.Path == "/.well-known/windshare" && scenario == "discovery",
					request.URL.Path == "/api/enrollments/claim":
					// Ctrl-C arrives while this request is outstanding. The client
					// may abandon the request without closing its connection, so the
					// test releases the handler before the server shuts down.
					interrupt()
					select {
					case <-request.Context().Done():
					case <-released:
					}
					return
				case request.URL.Path == "/.well-known/windshare":
					response = map[string]any{"version": 1, "access": "restricted", "relay_url": relayURL, "console_url": relayURL}
				case request.URL.Path == "/api/enrollments":
					response = map[string]any{"enrollment_id": "enrollment", "user_code": "ABCD-EFGH",
						"verification_uri_complete": relayURL + "/?code=ABCD-EFGH",
						"expires_at":                time.Now().Add(time.Minute).UnixMilli(), "interval_ms": 5000}
				case request.URL.Path == "/api/enrollments/cancel":
					cancelled.Store(true)
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
			t.Cleanup(func() { close(released) })
			app, _, stderr := newRelayTestApp(t, server.Client())
			if code := app.Run(ctx, []string{"relay", "add", relayURL, "--name", "CLI device", "--no-browser"}); code != ExitFailure {
				t.Fatalf("interrupted setup exit=%d stderr=%q", code, stderr.String())
			}
			hint := "Nothing was saved; run the same command to start again."
			if scenario == "approval" {
				hint = "Setup was saved. Run the same command to continue, or wind relay cancel " + relayURL + "."
			}
			if !strings.HasSuffix(stderr.String(), hint+"\n") {
				t.Fatalf("stderr=%q, want hint %q", stderr.String(), hint)
			}
			if scenario != "approval" {
				return
			}
			// The saved-setup hint must be actionable as printed.
			stderr.Reset()
			if code := app.Run(t.Context(), []string{"relay", "cancel", relayURL}); code != ExitOK || !cancelled.Load() {
				t.Fatalf("following the hint: exit=%d cancelled=%v stderr=%q", code, cancelled.Load(), stderr.String())
			}
		})
	}
}
