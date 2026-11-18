package cli

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
)

func TestShareLoadsSavedConfigurationAndOverridesChoicesIndependently(t *testing.T) {
	saved := relayconfig.Config{
		FrontURL:     "https://front.example/app",
		ActiveRelays: []string{"https://one.example", "https://two.example"},
		Profiles: []relayprofile.Candidate{
			{PublicURL: "https://one.example", AccessKey: "one-private-token"},
			{PublicURL: "https://two.example", AccessKey: "two-private-token"},
		},
	}
	app := testApp("")
	app.loadShareConfig = func() (relayconfig.Config, error) { return saved, nil }
	t.Cleanup(app.closeTerminalOutput)
	for _, test := range []struct {
		name   string
		args   []string
		front  string
		relays []relayprofile.Candidate
	}{
		{"saved", nil, saved.FrontURL, saved.Profiles},
		{"frontend override", []string{"--front-url", "https://another.example"}, "https://another.example", saved.Profiles},
		{"relay override", []string{"--relay", "wss://TWO.example:443/"}, saved.FrontURL, []relayprofile.Candidate{{PublicURL: "wss://TWO.example:443/", AccessKey: "two-private-token"}}},
		{"both overrides", []string{"--front-url", "http://localhost:38384", "--relay", "http://localhost:38384"}, "http://localhost:38384", []relayprofile.Candidate{{PublicURL: "http://localhost:38384"}}},
		{"overrides are transient", nil, saved.FrontURL, saved.Profiles},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, outcome := app.parseShareRequest(append([]string{"root"}, test.args...))
			if outcome != requestParseReady || !reflect.DeepEqual(request.relayIntent.Candidates(), test.relays) || request.link.frontURL != test.front {
				t.Fatalf("selection changed: outcome=%v frontend=%q", outcome, request.link.frontURL)
			}
		})
	}
	if _, outcome := app.parseShareRequest([]string{"root", "--front-url", ""}); outcome != requestParseUsageFailure {
		t.Fatal("empty explicit frontend silently used the saved value")
	}
}

func TestShareConfigurationFailuresDoNotFallBackToLocalhost(t *testing.T) {
	for _, test := range []struct {
		name   string
		config relayconfig.Config
		err    error
		args   []string
	}{
		{name: "invalid saved configuration", err: errors.New("private-error-token"), args: []string{"--front-url", "https://front.example", "--relay", "https://relay.example"}},
		{name: "invalid frontend override", args: []string{"--front-url", "wss://front.example", "--relay", "https://relay.example"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
			app := &App{Stdout: stdout, Stderr: stderr,
				loadShareConfig: func() (relayconfig.Config, error) { return test.config, test.err },
			}
			t.Cleanup(app.closeTerminalOutput)
			if _, outcome := app.parseShareRequest(append([]string{"root"}, test.args...)); outcome != requestParseUsageFailure {
				t.Fatal("incomplete configuration was accepted")
			}
			if stderr.Len() == 0 || bytes.Contains(stderr.Bytes(), []byte("private-error-token")) || stdout.Len() != 0 {
				t.Fatal("configuration failure exposed private input or a capability")
			}
		})
	}
}

func TestShareNamesRepeatedMemberInSavedConfiguration(t *testing.T) {
	stderr := new(bytes.Buffer)
	app := &App{Stdout: new(bytes.Buffer), Stderr: stderr, loadShareConfig: func() (relayconfig.Config, error) {
		return relayconfig.Decode(strings.NewReader(`{"profiles":[{"url":"https://a.example","access_key":"private-error-token","url":"https://b.example"}]}`))
	}}
	t.Cleanup(app.closeTerminalOutput)
	if _, outcome := app.parseShareRequest([]string{"root", "--front-url", "https://front.example"}); outcome != requestParseUsageFailure {
		t.Fatal("configuration with a repeated member was accepted")
	}
	if !strings.Contains(stderr.String(), `member "url" appears more than once in profiles[0]`) || strings.Contains(stderr.String(), "private-error-token") {
		t.Fatalf("share did not name only the repeated member: %q", stderr.String())
	}
}

func TestShareReportsNumericHostAndSavedFieldBeforeStartingWork(t *testing.T) {
	for _, saved := range []bool{false, true} {
		stderr := new(bytes.Buffer)
		app := &App{Stdout: new(bytes.Buffer), Stderr: stderr}
		t.Cleanup(app.closeTerminalOutput)
		args := []string{"root", "--relay", "https://relay.123"}
		if saved {
			app.loadShareConfig = func() (relayconfig.Config, error) {
				return relayconfig.Decode(strings.NewReader(`{"profiles":[{"url":"https://good.example"},{"url":"https://relay.123"}],"active_relays":["https://good.example"]}`))
			}
			args = []string{"root", "--relay", "https://other.example"}
		}
		if _, outcome := app.parseShareRequest(args); outcome != requestParseUsageFailure || app.application != nil {
			t.Fatal("invalid address reached a sharing workflow")
		}
		details := []string{"relay.123", "canonical IP address"}
		if saved {
			details = append(details, "profiles[1].url")
		}
		for _, detail := range details {
			if !strings.Contains(stderr.String(), detail) {
				t.Errorf("share lost repair detail %q: %s", detail, stderr.String())
			}
		}
	}
}

func TestShareRelayConfigurationIsPluralBoundedAndDeduplicated(t *testing.T) {
	app := testApp("")
	t.Cleanup(app.closeTerminalOutput)
	request, outcome := app.parseShareRequest([]string{"root", "--front-url", "https://front.example", "--relay", "https://a.example", "--relay", "https://b.example", "--relay", "https://a.example"})
	if outcome != requestParseReady || !reflect.DeepEqual(request.relayIntent.Candidates(), []relayprofile.Candidate{{PublicURL: "https://a.example"}, {PublicURL: "https://b.example"}}) {
		t.Fatalf("relays=%v outcome=%v", request.relayIntent.Candidates(), outcome)
	}
	args := []string{"root", "--front-url", "https://front.example"}
	for index := range relayprofile.MaximumEndpoints + 1 {
		args = append(args, "--relay", fmt.Sprintf("https://relay-%d.example", index))
	}
	if _, outcome = app.parseShareRequest(args); outcome != requestParseUsageFailure {
		t.Fatal("unbounded relay configuration accepted")
	}
}

func TestShareRejectsRelayEndpointCopiedFromStartupOutput(t *testing.T) {
	for _, args := range [][]string{
		{"root", "--relay", "ws://127.0.0.1:41871/v2/ws?token=secret-canary"},
		{"root", "--relay", "ws://127.0.0.1:41871", "--relay", "ws://127.0.0.1:41871/v2/ws"},
	} {
		stderr := new(bytes.Buffer)
		app := &App{Stdout: new(bytes.Buffer), Stderr: stderr}
		t.Cleanup(app.closeTerminalOutput)
		if _, outcome := app.parseShareRequest(args); outcome != requestParseUsageFailure || app.application != nil {
			t.Fatal("an address that dials /v2/ws/v2/ws reached a sharing workflow")
		}
		const expected = `share: invalid relay URL: address "ws://127.0.0.1:41871/v2/ws" ends in /v2/ws`
		if !strings.Contains(stderr.String(), expected) || strings.Contains(stderr.String(), "secret-canary") {
			t.Fatalf("share did not identify the copied endpoint: %q", stderr.String())
		}
	}
}
