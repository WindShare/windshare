package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
			app := &App{
				Stdout: &stdout, Stderr: &stderr,
				openBrowser: func(_ context.Context, _ string) error {
					t.Error("headless command opened a browser")
					return nil
				},
				engineConfig: engine.Config{RelaySetup: relaysetup.Dependencies{
					Store: relaysetup.FileStore{Settings: store}, HTTP: server.Client(),
				}},
			}
			t.Cleanup(app.closeTerminalOutput)
			if code := app.Run(t.Context(), []string{"relay", operation, relayURL, "--name", "CLI device", "--no-browser"}); code != ExitOK {
				t.Fatal("setup failed", code, stderr.String())
			}
			saved, err := store.Load()
			if err != nil || len(saved.Profiles) != 2 ||
				saved.Profiles[1].AccessKey != "cli-access-key-canary" || saved.FrontURL != initial.FrontURL {
				t.Fatal("setup did not persist a complete profile", err)
			}
			expectedSelection := append(slices.Clone(initial.ActiveRelays), relayURL)
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
				command, outcome := app.parseRelaySetupRequest(operation, args)
				wantOperation := relaysetup.AddProfile
				if operation == "onboard" {
					wantOperation = relaysetup.AuthorizeDevice
				}
				if outcome != requestParseReady || command.request.URL != relayURL ||
					command.request.DeviceName != "device" || !command.noBrowser || command.request.Operation != wantOperation {
					t.Fatalf("outcome=%v command=%+v", outcome, command)
				}
			})
		}
	}

	app := &App{}
	command, outcome := app.parseRelaySetupRequest("add", []string{"--name", "--help", relayURL})
	if outcome != requestParseReady || command.request.DeviceName != "--help" {
		t.Fatalf("a flag value became a help request: outcome=%v command=%+v", outcome, command)
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
			if code != ExitUsage || stderr.Len() == 0 || stdout.Len() != 0 {
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
			store := &interruptedRelayStore{Store: app.engineConfig.RelaySetup.Store, ctx: ctx}
			app.engineConfig.RelaySetup.Store = store
			if code := app.Run(ctx, []string{"relay", "add", relayURL, "--name", "CLI device", "--no-browser"}); code != ExitFailure {
				t.Fatalf("interrupted setup exit=%d stderr=%q", code, stderr.String())
			}
			if store.loadsAfterInterrupt.Load() != 0 {
				t.Fatal("interruption feedback reacquired configuration storage")
			}
			cancelCommand := "wind relay cancel " + relayURL
			if strings.Contains(stderr.String(), cancelCommand) != (scenario == "approval") {
				t.Fatalf("cancellation guidance does not match saved progress: %q", stderr.String())
			}
			if scenario != "approval" {
				return
			}
			// Explicit cancellation is a new operation with its own storage access.
			app.engineConfig.RelaySetup.Store = store.Store
			stderr.Reset()
			if code := app.Run(t.Context(), []string{"relay", "cancel", relayURL}); code != ExitOK || !cancelled.Load() {
				t.Fatalf("following the hint: exit=%d cancelled=%v stderr=%q", code, cancelled.Load(), stderr.String())
			}
		})
	}
}

func TestRelayRemoveDeletesSavedProfileAndRejectsUnknownRelay(t *testing.T) {
	store := relayconfig.FileStore{Path: filepath.Join(t.TempDir(), "config.json")}
	initial := relayconfig.Config{
		Profiles: []relayprofile.Candidate{
			{PublicURL: "https://one.example", AccessKey: "secret-one"},
			{PublicURL: "https://two.example"},
		},
		ActiveRelays: []string{"https://one.example", "https://two.example"},
	}
	if err := store.Save(initial); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr, engineConfig: engine.Config{RelaySetup: relaysetup.Dependencies{
		Store: relaysetup.FileStore{Settings: store},
	}}}
	t.Cleanup(app.closeTerminalOutput)
	if code := app.Run(t.Context(), []string{"relay", "remove", "https://one.example"}); code != ExitOK {
		t.Fatalf("remove exit=%d stderr=%q", code, stderr.String())
	}
	if stdout.Len() != 0 || strings.Contains(stderr.String(), "secret-one") || !strings.Contains(stderr.String(), "https://two.example") {
		t.Fatalf("unexpected output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	saved, err := store.Load()
	if err != nil || len(saved.Profiles) != 1 || saved.Profiles[0].PublicURL != "https://two.example" || !slices.Equal(saved.ActiveRelays, []string{"https://two.example"}) {
		t.Fatalf("saved config = %+v err=%v", saved, err)
	}
	stderr.Reset()
	if code := app.Run(t.Context(), []string{"relay", "remove", "https://one.example"}); code != ExitFailure {
		t.Fatalf("removing missing relay exit=%d stderr=%q", code, stderr.String())
	}
}

func TestRelayApprovalShowsServiceLoginWithoutAutoOpeningBrowser(t *testing.T) {
	const (
		relayURL   = "https://relay.example"
		approveURL = relayURL + "/?code=ABCD-EFGH"
		loginPage  = "https://accounts.example/device"
		invitation = "aW52aXRl"
	)
	for _, operation := range []string{"add", "onboard"} {
		for _, noBrowser := range []bool{false, true} {
			name := operation
			if noBrowser {
				name += "/no-browser"
			}
			t.Run(name, func(t *testing.T) {
				server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					var response any
					switch request.URL.Path {
					case "/.well-known/windshare":
						response = map[string]any{"version": 1, "access": "restricted", "relay_url": relayURL,
							"console_url": relayURL, "account_login_page": loginPage}
					case "/api/enrollments":
						response = map[string]any{"enrollment_id": "enrollment", "user_code": "ABCD-EFGH",
							"verification_uri_complete": approveURL, "account_invitation": invitation,
							"expires_at": time.Now().Add(time.Minute).UnixMilli(), "interval_ms": 5000}
					case "/api/enrollments/claim":
						response = map[string]string{"credential_id": "credential", "access_key": "service-member-key"}
					case "/api/enrollments/installed":
						response = map[string]bool{"ok": true}
					default:
						t.Errorf("unexpected setup endpoint %s", request.URL.Path)
						writer.WriteHeader(http.StatusNotFound)
						return
					}
					if err := json.NewEncoder(writer).Encode(response); err != nil {
						t.Error(err)
					}
				}))
				app, stdout, stderr := newRelayTestApp(t, server.Client())
				app.openBrowser = func(_ context.Context, target string) error {
					t.Errorf("service-login enrollment auto-opened browser for %q", target)
					return nil
				}
				args := []string{"relay", operation, relayURL, "--name", "CLI device"}
				if noBrowser {
					args = append(args, "--no-browser")
				}
				if code := app.Run(t.Context(), args); code != ExitOK {
					t.Fatalf("setup failed: exit=%d stderr=%q", code, stderr.String())
				}
				out := stderr.String()
				for _, want := range []string{
					"Sign in with accounts.example: " + loginPage + "#invite=" + invitation,
					"Or pair as node owner: " + approveURL,
					"Device code: ABCD-EFGH",
				} {
					if !strings.Contains(out, want) {
						t.Fatalf("stderr missing %q:\n%s", want, out)
					}
				}
				if stdout.Len() != 0 || strings.Contains(out, "service-member-key") {
					t.Fatalf("unexpected stdout or leaked key: stdout=%q stderr=%q", stdout.String(), out)
				}
			})
		}
	}
}

func TestRelayOwnerOnlyApprovalOpensBrowserAndFallsBackOnError(t *testing.T) {
	const (
		relayURL   = "https://relay.example"
		approveURL = relayURL + "/?code=ABCD-EFGH"
	)
	for _, browserErr := range []error{nil, errors.New("no display")} {
		name := "opened"
		if browserErr != nil {
			name = "unavailable"
		}
		t.Run(name, func(t *testing.T) {
			server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var response any
				switch request.URL.Path {
				case "/.well-known/windshare":
					response = map[string]any{"version": 1, "access": "restricted", "relay_url": relayURL, "console_url": relayURL}
				case "/api/enrollments":
					response = map[string]any{"enrollment_id": "enrollment", "user_code": "ABCD-EFGH",
						"verification_uri_complete": approveURL,
						"expires_at":                time.Now().Add(time.Minute).UnixMilli(), "interval_ms": 5000}
				case "/api/enrollments/claim":
					response = map[string]string{"credential_id": "credential", "access_key": "owner-key"}
				case "/api/enrollments/installed":
					response = map[string]bool{"ok": true}
				default:
					t.Errorf("unexpected setup endpoint %s", request.URL.Path)
					writer.WriteHeader(http.StatusNotFound)
					return
				}
				if err := json.NewEncoder(writer).Encode(response); err != nil {
					t.Error(err)
				}
			}))
			app, _, stderr := newRelayTestApp(t, server.Client())
			var opened []string
			app.openBrowser = func(_ context.Context, target string) error {
				opened = append(opened, target)
				return browserErr
			}
			if code := app.Run(t.Context(), []string{"relay", "onboard", relayURL, "--name", "CLI device"}); code != ExitOK {
				t.Fatalf("setup failed: exit=%d stderr=%q", code, stderr.String())
			}
			if !slices.Equal(opened, []string{approveURL}) {
				t.Fatalf("opened=%v, want [%s]", opened, approveURL)
			}
			out := stderr.String()
			if !strings.Contains(out, "Approve this device: "+approveURL) || !strings.Contains(out, "Device code: ABCD-EFGH") {
				t.Fatalf("missing owner approval prompt: %q", out)
			}
			const fallbackHint = "Open the approval URL on another device and confirm the code."
			if strings.Contains(out, fallbackHint) != (browserErr != nil) {
				t.Fatalf("fallback hint mismatch (browserErr=%v): %q", browserErr, out)
			}
		})
	}
}
