package cli

import (
	"bufio"
	"bytes"
	"context"
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

// sourceDriftJob is a complete job whose only unfinished file changed at the source.
func sourceDriftJob(t *testing.T) transfer.JobResult {
	t.Helper()
	drift, err := transferfault.NewSource(transferfault.ScopeFileLocal, transferfault.SourceRevisionChanged)
	if err != nil {
		t.Fatal(err)
	}
	return transfer.JobResult{
		Outcome: transfer.DirectTreeOutcomePartial, SourceDriftFault: drift,
		Files: []transfer.FileJobFailure{{Path: "e/b.txt", Stage: transfer.FailureRevisionOpen, Fault: drift}},
		Progress: transfer.ReceiveProgressSnapshot{
			Discovery: transfer.DiscoveryComplete, CountersExact: true, DiscoveredFiles: 2, PublishedFiles: 1,
			RestartReusableBytes: 4096, FileOutcomes: transfer.FileOutcomeSummary{DownloadedFiles: 1, FailedFiles: 1},
		},
	}
}

func reportSourceDrift(t *testing.T, job transfer.JobResult, contextErr error, retention engine.ReceiveProgressRetention) (int, string, receivecontract.OperationID) {
	t.Helper()
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
		Result: job, ContextError: contextErr, Destination: t.TempDir(), Operation: operation,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Retained-output settlement happens in the engine's operation loop.
	settled.Value.Recovery.Retention = retention
	code := app.reportEngineReceive(settled, receiveSelection{}, getObservation{runtime: runtime})
	runtime.Close()
	return code, stderr.String(), operation
}

func requireLines(t *testing.T, output string, present, absent []string) {
	t.Helper()
	for _, want := range present {
		if !strings.Contains(output, want) {
			t.Fatalf("stderr=%q lacks %q", output, want)
		}
	}
	for _, unwanted := range absent {
		if strings.Contains(output, unwanted) {
			t.Fatalf("stderr=%q contains %q", output, unwanted)
		}
	}
}

// The sender never re-admits a changed source under the same link, so when
// nothing else is unfinished the receiver points at a new link, not a retry.
func TestSourceDriftGuidesToNewLinkInsteadOfSameLinkRetry(t *testing.T) {
	code, output, operation := reportSourceDrift(t, sourceDriftJob(t), nil, engine.ReceiveRetentionCheckpoints)
	if code != ExitDrift {
		t.Fatalf("exit=%d stderr=%q", code, output)
	}
	requireLines(t, output, []string{
		`"e/b.txt": changed at the source`,
		changedSourceGuidance,
		fmt.Sprintf("Look for operation ID %x in wind resume list -o <original-directory>.", operation.Bytes()),
		"The source content changed during transfer.",
	}, []string{"wind resume retry", "Saved partials", "transfer or output failure"})
}

// Unchanged files interrupted beside the drift can still finish from this link.
func TestSourceDriftBesideInterruptionKeepsRetryHint(t *testing.T) {
	job := sourceDriftJob(t)
	job.TerminationCause, job.TerminationInterruption = context.Canceled, transfer.TransferInterruptionCanceled
	job.Progress.DiscoveredFiles++
	job.Progress.FileOutcomes.PausedFiles++
	code, output, operation := reportSourceDrift(t, job, context.Canceled, engine.ReceiveRetentionCheckpoints)
	if code != ExitDrift {
		t.Fatalf("exit=%d stderr=%q", code, output)
	}
	requireLines(t, output, []string{
		`"e/b.txt": changed at the source`,
		"Saved partials: 4096 bytes.",
		"Items changed at the source need a new link from the sender.\n",
		fmt.Sprintf("Retry with wind resume retry -o <original-directory> --operation %x", operation.Bytes()),
	}, []string{"cannot be finished from this link"})
}

// Discard is the first offered action, so its report still names the new link.
func TestSourceDriftDiscardStillGuidesToNewLink(t *testing.T) {
	code, output, _ := reportSourceDrift(t, sourceDriftJob(t), nil, engine.ReceiveRetentionDiscarded)
	if code != ExitDrift {
		t.Fatalf("exit=%d stderr=%q", code, output)
	}
	requireLines(t, output, []string{
		"Unfinished owned output was discarded; completed files were kept.\n" + changedSourceGuidance,
	}, []string{"wind resume"})
}

// Without retained checkpoints a retry restarts the download, so the report
// names the changed items without promising that this link finishes the rest.
func TestSourceDriftBesideOtherWorkWithoutCheckpointsOmitsRetry(t *testing.T) {
	job := sourceDriftJob(t)
	job.Progress.DiscoveredFiles++
	code, output, _ := reportSourceDrift(t, job, nil, engine.ReceiveRetentionLiveReleased)
	if code != ExitDrift {
		t.Fatalf("exit=%d stderr=%q", code, output)
	}
	requireLines(t, output, []string{
		"Download stopped; unfinished progress could not be retained after exit.",
		"Items changed at the source need a new link from the sender.\n",
	}, []string{"Retry", "wind resume retry", "cannot be finished from this link"})
}

func TestSourceDriftPauseOffersOnlyDiscardOrExit(t *testing.T) {
	app, stderr := newSourceDriftReportingApp(t)
	pause := engine.ReceivePause{
		Recovery: engine.ReceiveRecoveryOutcome{Retention: engine.ReceiveRetentionCheckpoints, Reason: engine.ReceiveRecoverySourceChanged},
		Actions:  []engine.ReceiveRecoveryAction{engine.ReceiveRecoveryDiscard, engine.ReceiveRecoveryExit},
		Transfer: sourceDriftJob(t),
	}
	action, err := app.promptReceiveRecovery(t.Context(), bufio.NewReader(strings.NewReader("r\nd\n")), pause)
	output := stderr.String()
	if err != nil || action != engine.ReceiveRecoveryDiscard {
		t.Fatalf("action=%d err=%v stderr=%q", action, err, output)
	}
	requireLines(t, output, []string{
		"Download stopped. The changed items cannot be finished from this link.",
		`"e/b.txt": changed at the source`,
		"Choose discard unfinished output, exit [d/e]:",
	}, []string{"transfer or output failure", "retry"})
}

func TestSourceDriftBesideOtherWorkPauseKeepsRetry(t *testing.T) {
	app, stderr := newSourceDriftReportingApp(t)
	pause := engine.ReceivePause{
		Recovery: engine.ReceiveRecoveryOutcome{Retention: engine.ReceiveRetentionCheckpoints, Reason: engine.ReceiveRecoveryTransferFailure},
		Actions:  []engine.ReceiveRecoveryAction{engine.ReceiveRecoveryRetry, engine.ReceiveRecoveryDiscard, engine.ReceiveRecoveryExit},
		Transfer: sourceDriftJob(t),
	}
	action, err := app.promptReceiveRecovery(t.Context(), bufio.NewReader(strings.NewReader("r\n")), pause)
	output := stderr.String()
	if err != nil || action != engine.ReceiveRecoveryRetry {
		t.Fatalf("action=%d err=%v stderr=%q", action, err, output)
	}
	requireLines(t, output, []string{
		"Download paused: a transfer or output failure.",
		"Items changed at the source need a new link from the sender. Retry can finish the rest.",
		"Choose retry, discard unfinished output, exit [r/d/e]:",
	}, []string{"cannot be finished from this link"})
}

// A stopped share offers no retry, so the prompt must not promise one.
func TestSourceDriftAfterShareStopPausePromisesNoRetry(t *testing.T) {
	app, stderr := newSourceDriftReportingApp(t)
	job := sourceDriftJob(t)
	job.Progress.DiscoveredFiles++
	pause := engine.ReceivePause{
		Recovery: engine.ReceiveRecoveryOutcome{Retention: engine.ReceiveRetentionCheckpoints, Reason: engine.ReceiveRecoveryTransferFailure, ShareStop: engine.ReceiveShareStopSender},
		Actions:  []engine.ReceiveRecoveryAction{engine.ReceiveRecoveryDiscard, engine.ReceiveRecoveryExit},
		Transfer: job,
	}
	action, err := app.promptReceiveRecovery(t.Context(), bufio.NewReader(strings.NewReader("e\n")), pause)
	output := stderr.String()
	if err != nil || action != engine.ReceiveRecoveryExit {
		t.Fatalf("action=%d err=%v stderr=%q", action, err, output)
	}
	requireLines(t, output, []string{
		"Download stopped. The share has ended.",
		`"e/b.txt": changed at the source`,
		"Choose discard unfinished output, exit [d/e]:",
	}, []string{"Retry", "retry", "new link"})
}
