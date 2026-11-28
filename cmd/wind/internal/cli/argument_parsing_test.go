package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestArgumentParsingReportsActionableDiagnostics(t *testing.T) {
	_, completeLink := argumentParsingCanaries(t)
	tests := []struct {
		name       string
		args       []string
		wantPrefix string
		wantExact  bool
	}{
		{
			name:       "unknown command",
			args:       []string{"not-a-command"},
			wantPrefix: "wind: unknown command \"not-a-command\"\nUsage:\n",
		},
		{
			name:       "unknown share option",
			args:       []string{"share", "--not-registered"},
			wantPrefix: "share: unknown option \"--not-registered\"\n",
			wantExact:  true,
		},
		{
			name:       "unknown get option",
			args:       []string{"get", "--not-registered"},
			wantPrefix: "get: unknown option \"--not-registered\"\n",
			wantExact:  true,
		},
		{
			name:       "missing share value",
			args:       []string{"share", "selected", "--block-size"},
			wantPrefix: "share: --block-size requires a value\n",
			wantExact:  true,
		},
		{
			name:       "missing get value",
			args:       []string{"get", "--connectivity"},
			wantPrefix: "get: --connectivity requires a value\n",
			wantExact:  true,
		},
		{
			name:       "malformed share option",
			args:       []string{"share", "---not-an-option"},
			wantPrefix: "share: invalid option syntax \"---not-an-option\"\n",
			wantExact:  true,
		},
		{
			name:       "malformed get option",
			args:       []string{"get", "--=not-an-option"},
			wantPrefix: "get: invalid option syntax \"--=not-an-option\"\n",
			wantExact:  true,
		},
		{
			name:       "invalid share boolean",
			args:       []string{"share", "selected", "--split-key=maybe"},
			wantPrefix: "share: invalid value \"maybe\" for --split-key: parse error\n",
			wantExact:  true,
		},
		{
			name:       "invalid share block size",
			args:       []string{"share", "selected", "--block-size", "abc"},
			wantPrefix: "share: invalid value \"abc\" for --block-size: must be a decimal integer\n",
			wantExact:  true,
		},
		{
			name:       "invalid connectivity",
			args:       []string{"get", completeLink, "--connectivity", "bogus"},
			wantPrefix: "get: invalid value \"bogus\" for --connectivity: must be auto, relay-only, or p2p-only\n",
			wantExact:  true,
		},
		{
			name:       "invalid account login boolean",
			args:       []string{"account", "login", "--password-stdin=maybe", "alice"},
			wantPrefix: "account login: invalid value \"maybe\" for --password-stdin: parse error\n",
			wantExact:  true,
		},
		{
			name:       "invalid config import boolean",
			args:       []string{"config", "import", "--verbose=maybe", "-"},
			wantPrefix: "config import: invalid value \"maybe\" for --verbose: parse error\n",
			wantExact:  true,
		},
		{
			name:       "invalid relay add boolean",
			args:       []string{"relay", "add", "--no-browser=maybe", "https://relay.example"},
			wantPrefix: "relay add: invalid value \"maybe\" for --no-browser: parse error\n",
			wantExact:  true,
		},
		{
			name:       "invalid resume discard item",
			args:       []string{"resume", "discard", "-o", ".", "--item", "bogus"},
			wantPrefix: "resume discard: --item must be one canonical positive decimal number\n",
			wantExact:  true,
		},
		{
			name:       "invalid doctor network boolean",
			args:       []string{"doctor", "network", "--json=maybe"},
			wantPrefix: "doctor network: invalid value \"maybe\" for --json: parse error\nUsage: wind doctor network [--json]\n",
			wantExact:  true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stdout, stderr, code := runArgumentParsingCase(t, test.args)
			if code != ExitUsage || stdout != "" {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if test.wantExact && stderr != test.wantPrefix {
				t.Fatalf("stderr=%q want=%q", stderr, test.wantPrefix)
			}
			if !test.wantExact && !strings.HasPrefix(stderr, test.wantPrefix) {
				t.Fatalf("stderr=%q want prefix %q", stderr, test.wantPrefix)
			}
		})
	}
}

func TestArgumentParsingRedactsURLFragmentsAndProtectsCredentialBoundaries(t *testing.T) {
	canaries, completeLink := argumentParsingCanaries(t)
	scenarios := []struct {
		name string
		args func(string) []string
	}{
		{name: "unknown command", args: func(link string) []string { return []string{link} }},
		{name: "unknown option value", args: func(link string) []string { return []string{"share", "--not-registered=" + link} }},
		{name: "invalid boolean", args: func(link string) []string { return []string{"share", "selected", "--split-key=" + link} }},
		{name: "invalid integer", args: func(link string) []string { return []string{"share", "selected", "--block-size", link} }},
		{name: "invalid connectivity", args: func(link string) []string { return []string{"get", completeLink, "--connectivity", link} }},
		{name: "invalid get capability", args: func(link string) []string { return []string{"get", strings.Replace(link, "://", "://[invalid]/", 1)} }},
		{name: "invalid resume item", args: func(link string) []string { return []string{"resume", "discard", "-o", ".", "--item", link} }},
	}

	for _, scenario := range scenarios {
		for _, canary := range canaries {
			t.Run(scenario.name+"/"+canary.name, func(t *testing.T) {
				stdout, stderr, code := runArgumentParsingCase(t, scenario.args(canary.value))
				if code != ExitUsage || stdout != "" || stderr == "" {
					t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
				}
				assertArgumentCanariesAbsent(t, stdout, stderr, canaries)
			})
		}
	}

	t.Run("account revoke invalid credential never echoes rejected ID", func(t *testing.T) {
		const credentialCanary = "credential_id_CANARY_7dc24c49"
		stdout, stderr, code := runArgumentParsingCase(t, []string{"account", "revoke", credentialCanary})
		if code != ExitUsage || stdout != "" || stderr != "Account: invalid credential ID\n" {
			t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		if strings.Contains(stderr, credentialCanary) {
			t.Fatalf("credential canary leaked: %q", stderr)
		}
	})
}

func TestShareAndGetHelpAreSafeInterleavedParseOutcomes(t *testing.T) {
	canaries, completeLink := argumentParsingCanaries(t)
	tests := []struct {
		name      string
		args      []string
		wantUsage string
	}{
		{name: "share short before positional", args: []string{"share", "-h", completeLink}, wantUsage: "Usage: wind share <path...> [options]\n"},
		{name: "share long before positional", args: []string{"share", "--help", completeLink}, wantUsage: "Usage: wind share <path...> [options]\n"},
		{name: "share short after positional", args: []string{"share", completeLink, "-h"}, wantUsage: "Usage: wind share <path...> [options]\n"},
		{name: "share long after positional", args: []string{"share", completeLink, "--help"}, wantUsage: "Usage: wind share <path...> [options]\n"},
		{name: "get short before positional", args: []string{"get", "-h", completeLink}, wantUsage: "Usage: wind get [options] <link>\n"},
		{name: "get long before positional", args: []string{"get", "--help", completeLink}, wantUsage: "Usage: wind get [options] <link>\n"},
		{name: "get short after positional", args: []string{"get", completeLink, "-h"}, wantUsage: "Usage: wind get [options] <link>\n"},
		{name: "get long after positional", args: []string{"get", completeLink, "--help"}, wantUsage: "Usage: wind get [options] <link>\n"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stdout, stderr, code := runArgumentParsingCase(t, test.args)
			if code != ExitOK || stdout != "" || !strings.HasPrefix(stderr, test.wantUsage) {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if !strings.Contains(stderr, "\nOptions:\n") {
				t.Fatalf("help does not contain options: %q", stderr)
			}
			assertArgumentCanariesAbsent(t, stdout, stderr, canaries)
		})
	}
}

type argumentCanary struct {
	name  string
	value string
}

func argumentParsingCanaries(t *testing.T) ([]argumentCanary, string) {
	t.Helper()
	capability := newSemanticCapability(t, "wss://relay.argument-canary.invalid")
	completeLink, err := capability.URL("https://app.argument-canary.invalid")
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := capability.SplitURL("https://app.argument-canary.invalid")
	if err != nil {
		t.Fatal(err)
	}
	return []argumentCanary{
		{name: "complete-link", value: completeLink},
		{name: "fragment-key", value: "#" + key},
	}, completeLink
}

func runArgumentParsingCase(t *testing.T, args []string) (string, string, int) {
	t.Helper()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := &App{Stdout: stdout, Stderr: stderr, Stdin: strings.NewReader("")}
	code := app.Run(context.Background(), args)
	app.closeTerminalOutput()
	return stdout.String(), stderr.String(), code
}

func assertArgumentCanariesAbsent(t *testing.T, stdout, stderr string, canaries []argumentCanary) {
	t.Helper()
	for _, canary := range canaries {
		secret := strings.TrimPrefix(canary.value, "#")
		if bytes.Contains([]byte(stdout), []byte(canary.value)) ||
			bytes.Contains([]byte(stderr), []byte(canary.value)) ||
			(strings.HasPrefix(canary.value, "#") && (bytes.Contains([]byte(stdout), []byte(secret)) || bytes.Contains([]byte(stderr), []byte(secret)))) {
			t.Fatalf("%s canary reached output: stdout=%q stderr=%q", canary.name, stdout, stderr)
		}
	}
}

func TestAllCommandsHandleHelpAndUsageWithoutStartingEngine(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
	}{
		{"top-level -help", []string{"-help"}, ExitOK},
		{"doctor --help", []string{"doctor", "--help"}, ExitOK},
		{"doctor help", []string{"doctor", "help"}, ExitOK},
		{"setup --help", []string{"setup", "--help"}, ExitOK},
		{"setup help", []string{"setup", "help"}, ExitOK},
		{"relay list --help", []string{"relay", "list", "--help"}, ExitOK},
		{"relay mode --help", []string{"relay", "mode", "--help"}, ExitOK},
		{"relay frontend -h", []string{"relay", "frontend", "-h"}, ExitOK},
		{"relay remove positional --help", []string{"relay", "remove", "https://relay.example", "--help"}, ExitOK},
		{"account login positional --help", []string{"account", "login", "alice", "--help"}, ExitOK},
		{"account revoke --help", []string{"account", "revoke", "--help"}, ExitOK},
		{"account revoke dash positional --help", []string{"account", "revoke", "-AAAAAAAAAAAAAAAAAAAAA", "--help"}, ExitOK},
		{"account unknown", []string{"account", "unknown"}, ExitUsage},
		{"resume --help", []string{"resume", "--help"}, ExitOK},
		{"resume help", []string{"resume", "help"}, ExitOK},
		{"resume list --help", []string{"resume", "list", "--help"}, ExitOK},
		{"resume discard --help", []string{"resume", "discard", "--help"}, ExitOK},
		{"resume missing action", []string{"resume"}, ExitUsage},
		{"resume unknown action", []string{"resume", "unknown"}, ExitUsage},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			app := &App{Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader("")}
			// Inject invalid engine config so any premature engine startup fails visibly.
			app.engineConfig.CleanupTimeout = -1
			code := app.Run(context.Background(), tc.args)
			app.closeTerminalOutput()
			if code != tc.wantCode || stdout.Len() != 0 || stderr.Len() == 0 {
				t.Fatalf("args=%v exit=%d want=%d stdout=%q stderr=%q", tc.args, code, tc.wantCode, stdout.String(), stderr.String())
			}
			if app.application != nil {
				t.Fatalf("args=%v unexpectedly initialized application engine", tc.args)
			}
		})
	}
}
