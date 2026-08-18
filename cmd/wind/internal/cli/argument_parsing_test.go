package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestArgumentParsingNeverProjectsRejectedSecretValues(t *testing.T) {
	canaries, completeLink := argumentParsingCanaries(t)
	_, baselineUnknownStderr, baselineUnknownCode := runArgumentParsingCase(t, []string{"not-a-command"})
	if baselineUnknownCode != ExitUsage || !strings.HasPrefix(baselineUnknownStderr, "wind: unknown command\nUsage:\n") {
		t.Fatalf("unknown-command baseline exit=%d stderr=%q", baselineUnknownCode, baselineUnknownStderr)
	}

	scenarios := []struct {
		name       string
		args       func(string) []string
		wantCode   int
		wantStderr string
	}{
		{
			name:       "unknown command",
			args:       func(canary string) []string { return []string{canary} },
			wantCode:   ExitUsage,
			wantStderr: baselineUnknownStderr,
		},
		{
			name: "invalid boolean",
			args: func(canary string) []string {
				return []string{"share", "selected", "--split-key=" + canary}
			},
			wantCode:   ExitUsage,
			wantStderr: "share: option value is invalid\n",
		},
		{
			name: "invalid integer",
			args: func(canary string) []string {
				return []string{"share", "selected", "--block-size", canary}
			},
			wantCode:   ExitUsage,
			wantStderr: "share: option value is invalid\n",
		},
		{
			name: "invalid enum",
			args: func(canary string) []string {
				return []string{"get", completeLink, "--connectivity", canary}
			},
			wantCode:   ExitUsage,
			wantStderr: "get: connectivity must be auto, relay-only, or p2p-only\n",
		},
	}

	for _, scenario := range scenarios {
		for _, canary := range canaries {
			t.Run(scenario.name+"/"+canary.name, func(t *testing.T) {
				stdout, stderr, code := runArgumentParsingCase(t, scenario.args(canary.value))
				if code != scenario.wantCode {
					t.Fatalf("exit=%d want=%d stderr=%q", code, scenario.wantCode, stderr)
				}
				if stdout != "" || stderr != scenario.wantStderr {
					t.Fatalf("stdout=%q stderr=%q want_stderr=%q", stdout, stderr, scenario.wantStderr)
				}
				assertArgumentCanariesAbsent(t, stdout, stderr, canaries)
			})
		}
	}
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

func TestFlagParseFailuresUseClosedDiagnostics(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantStderr string
	}{
		{name: "unknown share option", args: []string{"share", "--not-registered"}, wantStderr: "share: unknown option\n"},
		{name: "unknown get option", args: []string{"get", "--not-registered"}, wantStderr: "get: unknown option\n"},
		{name: "missing share value", args: []string{"share", "selected", "--block-size"}, wantStderr: "share: option value is required\n"},
		{name: "missing get value", args: []string{"get", "--connectivity"}, wantStderr: "get: option value is required\n"},
		{name: "malformed share option", args: []string{"share", "---not-an-option"}, wantStderr: "share: option syntax is invalid\n"},
		{name: "malformed get option", args: []string{"get", "--=not-an-option"}, wantStderr: "get: option syntax is invalid\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stdout, stderr, code := runArgumentParsingCase(t, test.args)
			if code != ExitUsage || stdout != "" || stderr != test.wantStderr {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
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
		{name: "authentication-token", value: "windshare_auth_token_CANARY_7dc24c49"},
		{name: "private-key", value: "-----BEGIN_PRIVATE_KEY-----CANARY_94d112f0-----END_PRIVATE_KEY-----"},
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
		if bytes.Contains([]byte(stdout), []byte(canary.value)) || bytes.Contains([]byte(stderr), []byte(canary.value)) {
			t.Fatalf("%s canary reached output: stdout=%q stderr=%q", canary.name, stdout, stderr)
		}
	}
}
