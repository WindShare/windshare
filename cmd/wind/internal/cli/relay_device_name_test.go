package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relaysetup"
)

func TestRelaySetupRejectsInvalidNamesBeforeOperation(t *testing.T) {
	for _, operation := range []string{"add", "onboard"} {
		for _, test := range []struct{ name, input string }{
			{"empty", ""},
			{"blank", "   "},
			{"too long", strings.Repeat("a", 129)},
			{"control", "CLI\tdevice"},
			{"invalid UTF-8", "\xff"},
		} {
			t.Run(operation+"/"+test.name, func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				// No engine exists: an argument error must complete without
				// acquiring configuration storage or contacting discovery.
				app := &App{Stdout: &stdout, Stderr: &stderr}
				t.Cleanup(app.closeTerminalOutput)
				code := app.runRelay(t.Context(), []string{operation, "https://unreachable.example",
					"--name", test.input, "--no-browser"})
				if code != ExitUsage || stdout.Len() != 0 {
					t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
				if !strings.Contains(stderr.String(), "device name") || !strings.Contains(stderr.String(), "--name") {
					t.Fatalf("missing actionable argument diagnostic: %q", stderr.String())
				}
			})
		}
	}
}

func TestRelaySetupPreservesExplicitDeviceName(t *testing.T) {
	const name = "  Ricky's 笔记本  "
	app := &App{}
	request, outcome := app.parseRelaySetupRequest("add", []string{"https://relay.example", "--name", name})
	if outcome != requestParseReady || request.DeviceName != name {
		t.Fatalf("explicit name was rewritten: outcome=%v name=%q", outcome, request.DeviceName)
	}
}

func TestRelaySetupDeviceNameFailureTakesPrecedenceOverCancellation(t *testing.T) {
	const relayURL = "https://relay.example"
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	app, stdout, stderr := newRelayTestApp(t, nil)
	app.engineConfig.RelaySetup.HTTP = relayNameHTTP(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/.well-known/windshare" {
			t.Errorf("unexpected request: %s", request.URL)
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("{}"))}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: relayDiscoveryBody{
			Reader: strings.NewReader(`{"version":1,"access":"restricted","relay_url":"https://relay.example","console_url":"https://relay.example"}`),
			cancel: cancel,
		}}, nil
	})
	application, err := app.newApplication()
	if err != nil {
		t.Fatal(err)
	}
	app.application = application
	t.Cleanup(func() {
		if err := application.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	// Exercise the wrapped engine error independently of CLI preflight.
	code := app.runRelaySetup(ctx, engine.RelaySetupRequest{
		URL: relayURL, DeviceName: "   ", Operation: relaysetup.AddProfile,
	})
	if code != ExitUsage || ctx.Err() == nil || stdout.Len() != 0 {
		t.Fatalf("exit=%d cancelled=%v stdout=%q", code, ctx.Err(), stdout.String())
	}
	if !strings.Contains(stderr.String(), "--name") || strings.Contains(stderr.String(), "same command") {
		t.Fatalf("input failure has conflicting recovery advice: %q", stderr.String())
	}
}

type relayNameHTTP func(*http.Request) (*http.Response, error)

func (client relayNameHTTP) Do(request *http.Request) (*http.Response, error) {
	return client(request)
}

type relayDiscoveryBody struct {
	io.Reader
	cancel context.CancelFunc
}

func (body relayDiscoveryBody) Close() error {
	// Cancel after the response is consumed, without relying on timing sleeps.
	body.cancel()
	return nil
}
