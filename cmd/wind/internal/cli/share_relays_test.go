package cli

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/engine/relayconfig"
)

func TestShareLoadsSavedConfigurationAndOverridesChoicesIndependently(t *testing.T) {
	saved := relayconfig.Config{
		FrontURL:     "https://front.example/app",
		ActiveRelays: []string{"https://one.example", "https://two.example"},
		Profiles: []senderrelay.Candidate{
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
		relays []senderrelay.Candidate
	}{
		{"saved", nil, saved.FrontURL, saved.Profiles},
		{"frontend override", []string{"--front-url", "https://another.example"}, "https://another.example", saved.Profiles},
		{"relay override", []string{"--relay", "wss://TWO.example:443/"}, saved.FrontURL, []senderrelay.Candidate{{PublicURL: "wss://TWO.example:443/", AccessKey: "two-private-token"}}},
		{"both overrides", []string{"--front-url", "http://localhost:38384", "--relay", "http://localhost:38384"}, "http://localhost:38384", []senderrelay.Candidate{{PublicURL: "http://localhost:38384"}}},
		{"overrides are transient", nil, saved.FrontURL, saved.Profiles},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, outcome := app.parseShareRequest(append([]string{"root"}, test.args...))
			if outcome != requestParseReady || !reflect.DeepEqual(request.relays, test.relays) || request.link.frontURL != test.front {
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
		{name: "unconfigured"},
		{name: "relay-only saved configuration", config: relayconfig.Config{Profiles: []senderrelay.Candidate{{PublicURL: "https://relay.example"}}}},
		{name: "frontend alone does not imply a relay", args: []string{"--front-url", "https://front.example"}},
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

func TestShareRelayConfigurationIsPluralBoundedAndDeduplicated(t *testing.T) {
	app := testApp("")
	t.Cleanup(app.closeTerminalOutput)
	request, outcome := app.parseShareRequest([]string{"root", "--front-url", "https://front.example", "--relay", "https://a.example", "--relay", "https://b.example", "--relay", "https://a.example"})
	if outcome != requestParseReady || !reflect.DeepEqual(request.relays, []senderrelay.Candidate{{PublicURL: "https://a.example"}, {PublicURL: "https://b.example"}}) {
		t.Fatalf("relays=%v outcome=%v", request.relays, outcome)
	}
	args := []string{"root", "--front-url", "https://front.example"}
	for index := range relayset.MaximumEndpoints + 1 {
		args = append(args, "--relay", fmt.Sprintf("https://relay-%d.example", index))
	}
	if _, outcome = app.parseShareRequest(args); outcome != requestParseUsageFailure {
		t.Fatal("unbounded relay configuration accepted")
	}
}
