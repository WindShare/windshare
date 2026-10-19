package configcommand

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/shareconfig"
	"github.com/windshare/windshare/connectivity/senderrelay"
)

const importedConfiguration = `{"front_url":"https://front.example/app","relays":[{"url":"https://private.example?route=one","registration_token":"secret-canary"}]}`

func TestImportFromFileAndStdin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "downloaded configuration.json")
	if err := os.WriteFile(path, []byte(importedConfiguration), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{path, "-"} {
		t.Run(source, func(t *testing.T) {
			var output bytes.Buffer
			var saved shareconfig.Config
			runner := Runner{
				Input: strings.NewReader(importedConfiguration), Output: &output,
				Save: func(config shareconfig.Config) error {
					saved = config
					return nil
				},
			}
			if result := runner.Run(context.Background(), []string{"import", source, "--verbose"}); result != ResultOK {
				t.Fatal("import failed:", result, output.String())
			}
			want := []senderrelay.Candidate{{PublicURL: "https://private.example?route=one", RegistrationToken: "secret-canary"}}
			if !reflect.DeepEqual(saved.Relays, want) || saved.FrontURL != "https://front.example/app" {
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
		{name: "empty set", args: []string{"import", "-"}, input: `{"relays":[]}`, want: ResultUsage},
		{name: "empty frontend override", args: []string{"import", "-", "--front-url", ""}, input: importedConfiguration, want: ResultUsage},
		{name: "invalid frontend override", args: []string{"import", "-", "--front-url", "wss://secret-canary"}, input: importedConfiguration, want: ResultUsage},
		{name: "invalid frontend document", args: []string{"import", "-"}, input: `{"front_url":"https://front.example#secret-canary","relays":[{"url":"https://relay.example"}]}`, want: ResultUsage},
		{name: "trailing document", args: []string{"import", "-"}, input: importedConfiguration + "{}", want: ResultUsage},
		{name: "save failure", args: []string{"import", "-", "-v"}, input: importedConfiguration,
			saveError: errors.New("secret-canary"), want: ResultFailure, saves: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var output bytes.Buffer
			saves := 0
			runner := Runner{
				Input: strings.NewReader(scenario.input), Output: &output,
				Save: func(shareconfig.Config) error {
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

func TestImportFrontendOverrideReplacesTheDocumentChoice(t *testing.T) {
	var saved shareconfig.Config
	runner := Runner{
		Input: strings.NewReader(importedConfiguration),
		Save:  func(config shareconfig.Config) error { saved = config; return nil },
	}
	if result := runner.Run(context.Background(), []string{"import", "--front-url", "https://another.example", "-"}); result != ResultOK {
		t.Fatal("frontend override failed:", result)
	}
	if saved.FrontURL != "https://another.example" || len(saved.Relays) != 1 || saved.Relays[0].RegistrationToken != "secret-canary" {
		t.Fatal("frontend override changed relay authority or failed to replace the document choice")
	}
}

func TestImportHelpAndCancellationHaveNoSideEffects(t *testing.T) {
	saves := 0
	runner := Runner{
		Input: strings.NewReader(importedConfiguration),
		Save:  func(shareconfig.Config) error { saves++; return nil },
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
