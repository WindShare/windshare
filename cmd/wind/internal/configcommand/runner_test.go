package configcommand

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
)

const importedConfiguration = `{"front_url":"https://front.example/app","profiles":[{"url":"https://private.example?route=one","access_key":"secret-canary"}]}`

type bufferLines struct{ buffer *bytes.Buffer }

func (output bufferLines) WriteLines(records ...string) {
	for _, record := range records {
		_, _ = fmt.Fprintln(output.buffer, strings.TrimSuffix(record, "\n"))
	}
}

func TestImportFromFileAndStdin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "downloaded configuration.json")
	if err := os.WriteFile(path, []byte(importedConfiguration), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{path, "-"} {
		t.Run(source, func(t *testing.T) {
			var output bytes.Buffer
			var saved relayconfig.Config
			runner := Runner{
				Input: strings.NewReader(importedConfiguration), Output: bufferLines{buffer: &output},
				TraceHandler: slog.NewJSONHandler(&output, nil),
				Save: func(config relayconfig.Config) error {
					saved = config
					return nil
				},
			}
			if result := runner.Run(context.Background(), []string{"import", source, "--verbose"}); result != ResultOK {
				t.Fatal("import failed:", result, output.String())
			}
			want := []relayprofile.Candidate{{PublicURL: "https://private.example?route=one", AccessKey: "secret-canary"}}
			if !reflect.DeepEqual(saved.Profiles, want) || saved.FrontURL != "https://front.example/app" {
				t.Fatal("import lost address-bound credentials")
			}
			assertPrivateOutput(t, output.String())
			assertImportTrace(t, output.String(), []string{"started", "validated", "committed"})
		})
	}
}

func TestImportFailureDoesNotSaveOrExposePrivateInput(t *testing.T) {
	for _, scenario := range []struct {
		name, input string
		args        []string
		saveError   error
		want        Result
		saves       int
	}{
		{name: "unknown action", args: []string{"secret-canary"}, want: ResultUsage},
		{name: "missing action", want: ResultUsage},
		{name: "missing file", args: []string{"import"}, want: ResultUsage},
		{name: "multiple files", args: []string{"import", "-", "secret-canary"}, want: ResultUsage},
		{name: "unknown option", args: []string{"import", "--secret-canary"}, want: ResultUsage},
		{name: "unreadable file", args: []string{"import", filepath.Join(t.TempDir(), "secret-canary")}, want: ResultFailure},
		{name: "invalid input", args: []string{"import", "-"}, input: "secret-canary", want: ResultUsage},
		{name: "empty set", args: []string{"import", "-"}, input: `{"profiles":null}`, want: ResultUsage},
		{name: "empty frontend override", args: []string{"import", "-", "--front-url", ""}, input: importedConfiguration, want: ResultUsage},
		{name: "invalid frontend override", args: []string{"import", "-", "--front-url", "wss://secret-canary"}, input: importedConfiguration, want: ResultUsage},
		{name: "invalid frontend document", args: []string{"import", "-"}, input: `{"front_url":"https://front.example#secret-canary","profiles":[{"url":"https://relay.example"}]}`, want: ResultUsage},
		{name: "trailing document", args: []string{"import", "-"}, input: importedConfiguration + "{}", want: ResultUsage},
		{name: "save failure", args: []string{"import", "-", "-v"}, input: importedConfiguration,
			saveError: errors.New("secret-canary"), want: ResultFailure, saves: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var output bytes.Buffer
			saves := 0
			runner := Runner{
				Input: strings.NewReader(scenario.input), Output: bufferLines{buffer: &output},
				TraceHandler: slog.NewJSONHandler(&output, nil),
				Save: func(relayconfig.Config) error {
					saves++
					return scenario.saveError
				},
			}
			if result := runner.Run(context.Background(), scenario.args); result != scenario.want || saves != scenario.saves {
				t.Fatalf("result=%v saves=%d", result, saves)
			}
			assertPrivateOutput(t, output.String())
		})
	}
}

// jq and python -m json.tool accept repeated members, so the message must name
// the member for the user to find it.
func TestImportNamesRepeatedMember(t *testing.T) {
	var output, trace bytes.Buffer
	runner := Runner{
		Input:        strings.NewReader(`{"profiles":[{"url":"https://a.example","access_key":"secret-canary","URL":"https://private.example"}]}`),
		Output:       bufferLines{buffer: &output},
		TraceHandler: slog.NewJSONHandler(&trace, nil),
		Save:         func(relayconfig.Config) error { t.Fatal("rejected import was saved"); return nil },
	}
	if result := runner.Run(context.Background(), []string{"import", "-", "--verbose"}); result != ResultUsage {
		t.Fatal("repeated member was not a usage error:", result)
	}
	want := `config import: invalid sharing configuration; member "URL" appears more than once in profiles[0]`
	if !strings.HasPrefix(output.String(), want) || !strings.Contains(trace.String(), `"reason":"repeated_member"`) {
		t.Fatalf("output=%q trace=%q", output.String(), trace.String())
	}
	assertPrivateOutput(t, output.String()+trace.String())
}

func TestImportParsePreservesFrontendValidationCause(t *testing.T) {
	for _, raw := range []string{"", "https://front.example/app\u00a0", "https://front.example/app\u0085", "wss://front.example"} {
		if _, err := parse([]string{"import", "-", "--front-url", raw}); !errors.Is(err, relayconfig.ErrFrontURL) {
			t.Fatalf("parse lost frontend validation cause for %q: %v", raw, err)
		}
	}
	if _, err := parse([]string{"import", "-"}); err != nil {
		t.Fatalf("omitted frontend override was rejected: %v", err)
	}
}

func TestImportFrontendOverrideReplacesTheDocumentChoice(t *testing.T) {
	var saved relayconfig.Config
	runner := Runner{
		Input: strings.NewReader(importedConfiguration),
		Save:  func(config relayconfig.Config) error { saved = config; return nil },
	}
	if result := runner.Run(context.Background(), []string{"import", "--front-url", "https://another.example", "-"}); result != ResultOK {
		t.Fatal("frontend override failed:", result)
	}
	if saved.FrontURL != "https://another.example" || len(saved.Profiles) != 1 || saved.Profiles[0].AccessKey != "secret-canary" {
		t.Fatal("frontend override changed relay authority or failed to replace the document choice")
	}
}

func TestImportHelpAndCancellationHaveNoSideEffects(t *testing.T) {
	saves := 0
	runner := Runner{
		Input: strings.NewReader(importedConfiguration),
		Save:  func(relayconfig.Config) error { saves++; return nil },
	}
	for _, args := range [][]string{{"help"}, {"--help"}, {"import", "--help"}} {
		if result := runner.Run(context.Background(), args); result != ResultOK {
			t.Fatal("help failed")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if result := runner.Run(ctx, []string{"import", "-"}); result != ResultFailure {
		t.Fatal("cancelled import succeeded")
	}
	if saves != 0 {
		t.Fatal("help or cancellation saved a selection")
	}
}

func TestImportRequiresInjectedStorageAndInput(t *testing.T) {
	runner := Runner{Input: strings.NewReader(importedConfiguration)}
	if result := runner.Run(context.Background(), []string{"import", "-"}); result != ResultFailure {
		t.Fatal("import succeeded without destination authority")
	}
	runner.Input = nil
	if result := runner.Run(context.Background(), []string{"import", "-"}); result != ResultFailure {
		t.Fatal("import succeeded without stdin")
	}
}

func assertPrivateOutput(t *testing.T, output string) {
	t.Helper()
	if strings.Contains(output, "secret-canary") || strings.Contains(output, "private.example") {
		t.Fatal("import diagnostics exposed private configuration")
	}
}

func assertImportTrace(t *testing.T, output string, want []string) {
	t.Helper()
	var messages []string
	var operationID string
	for line := range strings.SplitSeq(output, "\n") {
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var event struct {
			Message     string `json:"msg"`
			Operation   string `json:"operation"`
			OperationID string `json:"operation_id"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if operationID == "" {
			operationID = event.OperationID
		}
		if event.Operation != "config_import" || operationID == "" || event.OperationID != operationID {
			t.Fatal("import trace lost operation correlation")
		}
		messages = append(messages, event.Message)
	}
	if !reflect.DeepEqual(messages, want) {
		t.Fatal("import trace lost a workflow milestone:", messages)
	}
}

func TestImportParseTreatsArgumentsAfterTerminatorAsPaths(t *testing.T) {
	// "--" ends option parsing for every later argument, as it does for share and get.
	if _, err := parse([]string{"import", "--", "relay.json", "--verbose"}); err == nil {
		t.Fatal("parse accepted an argument after -- as the --verbose option")
	}
	if _, err := parse([]string{"import", "--", "relay.json", "--front-url", "https://example.com"}); err == nil {
		t.Fatal("parse accepted arguments after -- as the --front-url option")
	}
	request, err := parse([]string{"import", "-v", "--", "-profile.json"})
	if err != nil || request.source != "-profile.json" || !request.verbose {
		t.Fatalf("parse(-v -- -profile.json) = %+v, %v", request, err)
	}
	request, err = parse([]string{"import", "relay.json", "--verbose"})
	if err != nil || request.source != "relay.json" || !request.verbose {
		t.Fatalf("parse(relay.json --verbose) = %+v, %v", request, err)
	}
}

func TestImportParseRejectsOptionsAfterTerminatorFollowingOptions(t *testing.T) {
	// Options before "--" must not hide the terminator from later arguments.
	for _, args := range [][]string{
		{"import", "-v", "--", "relay.json", "--verbose"},
		{"import", "--front-url", "https://a.example", "--", "relay.json", "--front-url", "https://b.example"},
		// The second "--" is a literal second path, not another terminator.
		{"import", "--", "relay.json", "--"},
	} {
		if request, err := parse(args); err == nil {
			t.Fatalf("parse(%q) = %+v, want rejection", args[1:], request)
		}
	}
	request, err := parse([]string{"import", "--", "--verbose"})
	if err != nil || request.source != "--verbose" || request.verbose {
		t.Fatalf("parse(-- --verbose) = %+v, %v", request, err)
	}
}

func TestImportParseKeepsTerminatorAsOptionValue(t *testing.T) {
	// A "--" value belongs to --front-url, so -v stays an option and the
	// frontend validation reports the specific cause, as share and get do.
	_, err := parse([]string{"import", "--front-url", "--", "relay.json", "-v"})
	if !errors.Is(err, relayconfig.ErrFrontURL) {
		t.Fatalf("parse(--front-url -- relay.json -v) = %v, want frontend URL error", err)
	}
	for _, args := range [][]string{
		{"import", "--front-url=--", "relay.json", "-v"},
		{"import", "--front-url", "--", "relay.json", "--verbose"},
	} {
		if _, err := parse(args); !errors.Is(err, relayconfig.ErrFrontURL) {
			t.Fatalf("parse(%q) = %v, want frontend URL error", args[1:], err)
		}
	}
}

func TestImportParseKeepsAcceptedForms(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		source   string
		verbose  bool
		frontURL string
	}{
		{[]string{"import", "relay.json"}, "relay.json", false, ""},
		{[]string{"import", "relay.json", "--verbose"}, "relay.json", true, ""},
		{[]string{"import", "-v", "--", "-profile.json"}, "-profile.json", true, ""},
		{[]string{"import", "--front-url", "https://front.example", "relay.json"}, "relay.json", false, "https://front.example"},
		{[]string{"import", "--front-url", "https://front.example", "--", "relay.json"}, "relay.json", false, "https://front.example"},
		{[]string{"import", "--", "-"}, "-", false, ""},
		{[]string{"import", "relay.json", "--"}, "relay.json", false, ""},
	} {
		request, err := parse(tc.args)
		if err != nil || request.source != tc.source || request.verbose != tc.verbose || request.frontURL != tc.frontURL {
			t.Fatalf("parse(%q) = %+v, %v", tc.args[1:], request, err)
		}
	}
	if _, err := parse([]string{"import", "--help"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("parse(import --help) = %v, want flag.ErrHelp", err)
	}
}
