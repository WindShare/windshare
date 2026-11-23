package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine"
)

func newSourceDriftReportingApp(t *testing.T) (*App, *bytes.Buffer) {
	t.Helper()
	stderr := &bytes.Buffer{}
	return &App{Stderr: stderr, terminalCapabilities: terminalcanvas.CapabilityProviderFunc(func() terminalcanvas.Capabilities {
		return terminalcanvas.Capabilities{Columns: 500}
	})}, stderr
}

// The sender never re-admits a changed source under the same link, so the
// receiver must point at a new link instead of a retry that repeats the drift.
func TestSourceDriftGuidesToNewLinkInsteadOfSameLinkRetry(t *testing.T) {
	drift, err := transferfault.NewSource(transferfault.ScopeFileLocal, transferfault.SourceRevisionChanged)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := receivecontract.OperationIDFromBytes(bytes.Repeat([]byte{7}, 16))
	if err != nil {
		t.Fatal(err)
	}
	app, stderr := newSourceDriftReportingApp(t)
	runtime, err := app.newCommandRuntime(clievent.CommandGet, observationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	settled, err := engine.SettleReceive(engine.ReceiveSettlementInput{
		Result: transfer.JobResult{
			Outcome: transfer.DirectTreeOutcomePartial, SourceDriftFault: drift,
			Progress: transfer.ReceiveProgressSnapshot{
				PreviouslyPublishedBytes: 4, FileOutcomes: transfer.FileOutcomeSummary{PreviouslyPublishedFiles: 1},
			},
		},
		Destination: t.TempDir(), Operation: operation,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Retained-output settlement happens in the engine's operation loop.
	settled.Value.Recovery.Retention = engine.ReceiveRetentionCheckpoints
	code := app.reportEngineReceive(settled, receiveSelection{}, getObservation{runtime: runtime})
	runtime.Close()
	output := stderr.String()
	for _, want := range []string{
		"The source content changed during transfer.",
		"Ask the sender to share it again and use the new link.",
		fmt.Sprintf("Look for operation ID %x in wind resume list -o <original-directory>.", operation.Bytes()),
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("stderr=%q lacks %q", output, want)
		}
	}
	if code != ExitDrift || strings.Contains(output, "wind resume retry") {
		t.Fatalf("exit=%d stderr=%q", code, output)
	}
}

func TestSourceDriftPauseOffersOnlyDiscardOrExit(t *testing.T) {
	app, stderr := newSourceDriftReportingApp(t)
	pause := engine.ReceivePause{
		Recovery: engine.ReceiveRecoveryOutcome{Retention: engine.ReceiveRetentionCheckpoints, Reason: engine.ReceiveRecoverySourceChanged},
		Actions:  []engine.ReceiveRecoveryAction{engine.ReceiveRecoveryDiscard, engine.ReceiveRecoveryExit},
	}
	action, err := app.promptReceiveRecovery(t.Context(), bufio.NewReader(strings.NewReader("r\nd\n")), pause)
	output := stderr.String()
	if err != nil || action != engine.ReceiveRecoveryDiscard {
		t.Fatalf("action=%d err=%v stderr=%q", action, err, output)
	}
	if !strings.Contains(output, "Ask the sender to share it again and use the new link.") ||
		!strings.Contains(output, "Choose discard unfinished output, exit [d/e]:") ||
		strings.Contains(output, "transfer or output failure") || strings.Contains(output, "retry") {
		t.Fatalf("stderr=%q", output)
	}
}
