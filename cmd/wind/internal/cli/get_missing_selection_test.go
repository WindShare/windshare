package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/engine"
)

func TestMissingSelectionNamesTheUnmatchedOnlyPaths(t *testing.T) {
	stderr := &bytes.Buffer{}
	app := &App{
		Stderr: stderr,
		terminalCapabilities: terminalcanvas.CapabilityProviderFunc(func() terminalcanvas.Capabilities {
			return terminalcanvas.Capabilities{Columns: 500}
		}),
	}
	runtime, err := app.newCommandRuntime(clievent.CommandGet, observationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	settled, err := engine.SettleReceive(engine.ReceiveSettlementInput{
		Result: transfer.JobResult{
			Outcome:                    transfer.DirectTreeOutcomePartial,
			Progress:                   transfer.ReceiveProgressSnapshot{Discovery: transfer.DiscoveryComplete, CountersExact: true},
			SelectionResolutionFailure: transfer.ErrSelectionTargetMissing,
			MissingSelectionPaths:      []string{"a b.txt", "d/nope"},
		},
		Destination: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	code := app.reportEngineReceive(settled, getObservation{runtime: runtime})
	runtime.Close()
	output := stderr.String()
	if code != ExitUsage {
		t.Fatalf("exit=%d want=%d stderr=%q", code, ExitUsage, output)
	}
	for _, want := range []string{
		`Not found in this share: --only "a b.txt"`,
		`Not found in this share: --only "d/nope"`,
		"Correct --only and run wind get with the intended selection.",
		"selected content was not found",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("stderr=%q does not contain %q", output, want)
		}
	}
}
