package command

import (
	"bytes"
	"errors"
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

func TestRedactTokenStripsURLFragments(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"shrae", "shrae"},
		{"--block-size", "--block-size"},
		{"0x1000", "0x1000"},
		{"https://ws.example/id#secret", "https://ws.example/id#<redacted>"},
		{"#secret", "#<redacted>"},
		{"https://ws.example/id#first#second", "https://ws.example/id#<redacted>"},
	}
	for _, tc := range tests {
		if got := RedactToken(tc.input); got != tc.want {
			t.Fatalf("RedactToken(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestParseInterleavedHandlesMixedPositionalsTerminatorAndErrors(t *testing.T) {
	t.Run("interleaved options and positionals", func(t *testing.T) {
		fs := NewFlagSet("example")
		verbose := fs.Bool("v", false, "verbose")
		target := fs.String("target", "", "target")
		positionals, err := ParseInterleaved(fs, []string{"first", "-v", "--target", "--", "second", "--", "-literal", "--help"})
		if err != nil || !*verbose || *target != "--" || !slices.Equal(positionals, []string{"first", "second", "-literal", "--help"}) {
			t.Fatalf("err=%v verbose=%v target=%q positionals=%q", err, *verbose, *target, positionals)
		}
	})

	t.Run("stdin dash stays positional", func(t *testing.T) {
		fs := NewFlagSet("example")
		verbose := fs.Bool("v", false, "verbose")
		positionals, err := ParseInterleaved(fs, []string{"-", "-v"})
		if err != nil || !*verbose || !slices.Equal(positionals, []string{"-"}) {
			t.Fatalf("err=%v verbose=%v positionals=%q", err, *verbose, positionals)
		}
	})

	t.Run("help before and after positional", func(t *testing.T) {
		for _, args := range [][]string{{"-h"}, {"--help"}, {"-help"}, {"--h"}, {"--help=1"}, {"pos", "--help"}, {"--help", "pos"}} {
			fs := NewFlagSet("example")
			if _, err := ParseInterleaved(fs, args); !errors.Is(err, ErrHelp) || ParseExitCode(err) != ExitOK {
				t.Fatalf("args=%q err=%v exit=%v", args, err, ParseExitCode(err))
			}
		}
	})

	t.Run("actionable parse diagnostics", func(t *testing.T) {
		tests := []struct {
			name     string
			args     []string
			wantExit ExitCode
			wantErr  string
		}{
			{"unknown long option", []string{"--nope"}, ExitUsage, `unknown option "--nope"`},
			{"unknown short option", []string{"-x"}, ExitUsage, `unknown option "-x"`},
			{"unknown option drops attached value", []string{"--nope=secret"}, ExitUsage, `unknown option "--nope"`},
			{"missing long value", []string{"--count"}, ExitUsage, `--count requires a value`},
			{"missing short value", []string{"-c"}, ExitUsage, `-c requires a value`},
			{"invalid int value", []string{"--count", "abc"}, ExitUsage, `invalid value "abc" for --count: parse error`},
			{"invalid equals value", []string{"--count=abc"}, ExitUsage, `invalid value "abc" for --count: parse error`},
			{"invalid bool value", []string{"--flag=abc"}, ExitUsage, `invalid value "abc" for --flag: parse error`},
			{"redacts fragment in invalid value", []string{"--count", "https://ws.example/id#secret"}, ExitUsage, `invalid value "https://ws.example/id#<redacted>" for --count: parse error`},
			{"custom error already naming option", []string{"--item", "0"}, ExitUsage, `--item must be positive`},
			{"custom error with incidental substring still names option", []string{"-o", "bad"}, ExitUsage, `invalid value "bad" for -o: read-only destination`},
			{"triple dash", []string{"---flag"}, ExitUsage, `invalid option syntax "---flag"`},
			{"leading equals", []string{"--=flag"}, ExitUsage, `invalid option syntax "--=flag"`},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				fs := NewFlagSet("example")
				fs.Int("count", 0, "count")
				fs.Int("c", 0, "short count")
				fs.Bool("flag", false, "flag")
				fs.Func("item", "item", func(string) error { return errors.New("--item must be positive") })
				fs.Func("o", "output", func(string) error { return errors.New("read-only destination") })
				_, err := ParseInterleaved(fs, tc.args)
				if err == nil || ParseExitCode(err) != tc.wantExit || err.Error() != tc.wantErr {
					t.Fatalf("err=%v exit=%v want=%q", err, ParseExitCode(err), tc.wantErr)
				}
			})
		}
	})

	t.Run("nil flag set fails as internal error", func(t *testing.T) {
		_, err := ParseInterleaved(nil, []string{"-h"})
		if !errors.Is(err, ErrInternal) || ParseExitCode(err) != ExitFailure || err.Error() == "" {
			t.Fatalf("err=%v exit=%v", err, ParseExitCode(err))
		}
	})
}

func TestParseNoPositionalsRejectsPositionalsBeforeTrailingHelp(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		wantErr string
	}{
		{[]string{"-o", "dir", "--", "-h"}, `unexpected positional argument "-h"`},
		{[]string{"-o", "dir", "--", "literal", "--help"}, `unexpected positional argument "literal"`},
		{[]string{"-o", "dir", "literal", "-h"}, `unexpected positional argument "literal"`},
		{[]string{"-o", "dir", "literal", "--help"}, `unexpected positional argument "literal"`},
		{[]string{"-o", "dir", "https://ws.example/id#secret"}, `unexpected positional argument "https://ws.example/id#<redacted>"`},
	} {
		fs := NewFlagSet("example")
		fs.String("o", "", "output")
		err := ParseNoPositionals(fs, tc.args)
		if err == nil || ParseExitCode(err) != ExitUsage || err.Error() != tc.wantErr {
			t.Fatalf("args=%q err=%v want=%q", tc.args, err, tc.wantErr)
		}
	}

	for _, value := range []string{"--", "-h", "--help"} {
		fs := NewFlagSet("example")
		out := fs.String("o", "", "output")
		if err := ParseNoPositionals(fs, []string{"-o", value, "--"}); err != nil || *out != value {
			t.Fatalf("value=%q err=%v got=%q", value, err, *out)
		}
	}

	fs := NewFlagSet("example")
	fs.String("o", "", "output")
	if err := ParseNoPositionals(fs, []string{"-o", "dir", "--help"}); !errors.Is(err, ErrHelp) || ParseExitCode(err) != ExitOK {
		t.Fatalf("err=%v exit=%v", err, ParseExitCode(err))
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
