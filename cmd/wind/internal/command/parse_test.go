package command

import (
	"bytes"
	"flag"
	"slices"
	"strings"
	"testing"
)

type recordedLines struct {
	lines []string
}

func (output *recordedLines) WriteLines(records ...string) {
	output.lines = append(output.lines, records...)
}

func (output *recordedLines) String() string {
	return strings.Join(output.lines, "\n")
}

func TestHelpTokenClassification(t *testing.T) {
	for _, token := range []string{"-h", "-help", "--help"} {
		if !IsHelpFlag(token) || !IsHelpToken(token) {
			t.Fatalf("%q was not recognized as a help flag", token)
		}
	}
	if IsHelpFlag("help") || !IsHelpToken("help") {
		t.Fatal("help verb classification was wrong")
	}
	for _, token := range []string{"", "status", "-v", "--json", "--"} {
		if IsHelpFlag(token) || IsHelpToken(token) {
			t.Fatalf("%q was misclassified as help", token)
		}
	}
}

func TestParseInterleavedHandlesMixedPositionalsTerminatorAndErrors(t *testing.T) {
	t.Run("interleaved options and positionals", func(t *testing.T) {
		fs := NewFlagSet("example")
		verbose := fs.Bool("v", false, "verbose")
		target := fs.String("target", "", "target")
		positionals, outcome := ParseInterleaved(fs, []string{"first", "-v", "--target", "--", "second", "--", "-literal", "--help"})
		if outcome != ParseReady || !*verbose || *target != "--" || !slices.Equal(positionals, []string{"first", "second", "-literal", "--help"}) {
			t.Fatalf("outcome=%v verbose=%v target=%q positionals=%q", outcome, *verbose, *target, positionals)
		}
	})

	t.Run("stdin dash stays positional", func(t *testing.T) {
		fs := NewFlagSet("example")
		verbose := fs.Bool("v", false, "verbose")
		positionals, outcome := ParseInterleaved(fs, []string{"-", "-v"})
		if outcome != ParseReady || !*verbose || !slices.Equal(positionals, []string{"-"}) {
			t.Fatalf("outcome=%v verbose=%v positionals=%q", outcome, *verbose, positionals)
		}
	})

	t.Run("help before and after positional", func(t *testing.T) {
		for _, args := range [][]string{{"-h"}, {"--help"}, {"-help"}, {"--h"}, {"--help=1"}, {"pos", "--help"}, {"--help", "pos"}} {
			fs := NewFlagSet("example")
			if _, outcome := ParseInterleaved(fs, args); outcome != ParseHelp {
				t.Fatalf("args=%q outcome=%v want=%v", args, outcome, ParseHelp)
			}
		}
	})

	t.Run("closed failure classifications", func(t *testing.T) {
		tests := []struct {
			name        string
			args        []string
			wantOutcome ParseOutcome
			wantExit    ExitCode
			wantDiag    string
		}{
			{"unknown option", []string{"--nope"}, ParseUnknownOption, ExitUsage, "unknown option"},
			{"missing value", []string{"--count"}, ParseMissingOptionValue, ExitUsage, "option value is required"},
			{"invalid int value", []string{"--count", "secret"}, ParseInvalidOptionValue, ExitUsage, "option value is invalid"},
			{"invalid equals value", []string{"--count=secret"}, ParseInvalidOptionValue, ExitUsage, "option value is invalid"},
			{"invalid bool value", []string{"--flag=secret"}, ParseInvalidOptionValue, ExitUsage, "option value is invalid"},
			{"triple dash", []string{"---flag"}, ParseInvalidOptionSyntax, ExitUsage, "option syntax is invalid"},
			{"leading equals", []string{"--=flag"}, ParseInvalidOptionSyntax, ExitUsage, "option syntax is invalid"},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				fs := NewFlagSet("example")
				fs.Int("count", 0, "count")
				fs.Bool("flag", false, "flag")
				_, outcome := ParseInterleaved(fs, tc.args)
				if outcome != tc.wantOutcome || outcome.ExitCode() != tc.wantExit || outcome.Diagnostic() != tc.wantDiag {
					t.Fatalf("outcome=%v exit=%v diag=%q", outcome, outcome.ExitCode(), outcome.Diagnostic())
				}
			})
		}
	})

	t.Run("nil flag set fails closed", func(t *testing.T) {
		_, outcome := ParseInterleaved(nil, []string{"-h"})
		if outcome != ParseInternalFailure || outcome.ExitCode() != ExitFailure || outcome.Diagnostic() == "" {
			t.Fatalf("outcome=%v exit=%v diag=%q", outcome, outcome.ExitCode(), outcome.Diagnostic())
		}
	})
}

func TestParseNoPositionalsRejectsPositionalsBeforeTrailingHelp(t *testing.T) {
	for _, args := range [][]string{
		{"-o", "dir", "--", "-h"},
		{"-o", "dir", "--", "literal", "--help"},
		{"-o", "dir", "literal", "-h"},
		{"-o", "dir", "literal", "--help"},
	} {
		fs := NewFlagSet("example")
		fs.String("o", "", "output")
		outcome := ParseNoPositionals(fs, args)
		if outcome != ParseUnexpectedPositional || outcome.ExitCode() != ExitUsage || outcome.Diagnostic() != "positional arguments are not accepted" {
			t.Fatalf("args=%q outcome=%v", args, outcome)
		}
	}

	for _, value := range []string{"--", "-h", "--help"} {
		fs := NewFlagSet("example")
		out := fs.String("o", "", "output")
		if outcome := ParseNoPositionals(fs, []string{"-o", value, "--"}); outcome != ParseReady || *out != value {
			t.Fatalf("value=%q outcome=%v got=%q", value, outcome, *out)
		}
	}

	fs := NewFlagSet("example")
	fs.String("o", "", "output")
	if outcome := ParseNoPositionals(fs, []string{"-o", "dir", "--help"}); outcome != ParseHelp || outcome.ExitCode() != ExitOK {
		t.Fatalf("outcome=%v", outcome)
	}
}

func TestFlagDefaultsAndHelpRenderRegistrationMetadataOnly(t *testing.T) {
	const registeredValue = "registered-default"
	const parsedValue = "parsed-value-CANARY"
	var output bytes.Buffer
	flags := flag.NewFlagSet("example", flag.ContinueOnError)
	flags.SetOutput(&output)
	flags.String("name", registeredValue, "label")
	flags.Bool("v", false, "switch")
	if err := flags.Set("name", parsedValue); err != nil {
		t.Fatal(err)
	}

	defaults := FlagDefaults(flags)
	if flags.Output() != &output || output.Len() != 0 {
		t.Fatal("rendering defaults changed the parser's output authority")
	}
	if strings.ContainsAny(defaults, "\t") || strings.Contains(defaults, `\t`) {
		t.Fatalf("unexpanded layout tabs: %q", defaults)
	}
	if !strings.Contains(defaults, registeredValue) || strings.Contains(defaults, parsedValue) {
		t.Fatalf("defaults reflected parsed values instead of registration metadata: %q", defaults)
	}

	var lines recordedLines
	WriteFlagHelp(&lines, flags, "example [options]")
	if !strings.HasPrefix(lines.String(), "Usage: "+Name+" example [options]\n\nOptions:\n") ||
		strings.Contains(lines.String(), parsedValue) {
		t.Fatalf("unexpected help output: %q", lines.String())
	}
	if FlagDefaults(nil) != "" {
		t.Fatal("nil FlagSet produced defaults")
	}
}

func TestExpandTabStopsUsesEightColumnStops(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{name: "short boolean", text: "  -v\tshow diagnostic milestones\n", want: "  -v    show diagnostic milestones\n"},
		{name: "usage line", text: "    \tretained operation ID\n", want: "        retained operation ID\n"},
		{
			name: "multi-line usage",
			text: "  -o string\n    \tfirst line\n    \tsecond line\n",
			want: "  -o string\n        first line\n        second line\n",
		},
		{name: "tab inside description", text: "    \ta\tb\n", want: "        a       b\n"},
		{name: "no tabs", text: "  -name string\n", want: "  -name string\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := expandTabStops(test.text); got != test.want {
				t.Fatalf("expandTabStops(%q) = %q, want %q", test.text, got, test.want)
			}
		})
	}
}

func TestRepeatedFlagAndWriteHelpers(t *testing.T) {
	var repeated RepeatedFlag
	if err := repeated.Set("a"); err != nil {
		t.Fatal(err)
	}
	if err := repeated.Set("b"); err != nil {
		t.Fatal(err)
	}
	if repeated.String() != "a,b" {
		t.Fatalf("repeated = %q", repeated.String())
	}
	// Nil LineOutput helpers must be safe no-ops.
	WriteLines(nil, "ignored")
	WriteLinef(nil, "ignored %d", 1)
	var out recordedLines
	WriteLinef(&out, "exit=%d", ExitDrift.Int())
	if out.String() != "exit=4" {
		t.Fatalf("out=%q", out.String())
	}
}
