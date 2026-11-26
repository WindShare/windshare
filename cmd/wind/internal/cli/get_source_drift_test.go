package cli

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

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
			RestartReusableBytes: 4096, FileOutcomes: transfer.FileOutcomeSummary{DownloadedFiles: 1},
		},
	}
}

func reportSourceDrift(t *testing.T, job transfer.JobResult, contextErr error, retention engine.ReceiveProgressRetention) (int, string, receivecontract.OperationID) {
	t.Helper()
	operation, jobID := newGetReportingIDs(t)
	app, stderr := newSourceDriftReportingApp(t)
	runtime, err := app.newCommandRuntime(commandGet, observationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	settled, err := engine.SettleReceive(engine.ReceiveSettlementInput{
		Result: job, ContextError: contextErr, Destination: t.TempDir(), Operation: operation, Job: jobID,
	})
	if err != nil {
		t.Fatal(err)
	}
	settled.Value.Recovery.Retention = retention
	code := app.reportEngineReceive(settled, receiveSelection{}, runtime, nil)
	runtime.Close()
	return code, stderr.String(), operation
}

func assertContainsAndExcludes(t *testing.T, output string, present, absent []string) {
	t.Helper()
	for _, want := range present {
		if !strings.Contains(output, want) {
			t.Fatalf("stderr=%q missing %q", output, want)
		}
	}
	for _, unwanted := range absent {
		if strings.Contains(output, unwanted) {
			t.Fatalf("stderr=%q unexpectedly contains %q", output, unwanted)
		}
	}
}

func TestSourceDriftReportingMatchesSameLinkViability(t *testing.T) {
	t.Run("pure drift with checkpoints stops and guides to new link", func(t *testing.T) {
		code, output, operation := reportSourceDrift(t, sourceDriftJob(t), nil, engine.ReceiveRetentionCheckpoints)
		if code != ExitDrift {
			t.Fatalf("exit=%d stderr=%q", code, output)
		}
		assertContainsAndExcludes(t, output, []string{
			`"e/b.txt"`,
			changedSourceLabel,
			"new link",
			fmt.Sprintf("%x", operation.Bytes()),
			"wind resume list",
			"Download stopped",
			"because the source changed",
		}, []string{"wind resume retry", "Saved partials", "finished partially"})
	})

	t.Run("pure drift after discard omits resume commands and keeps new link guidance", func(t *testing.T) {
		code, output, _ := reportSourceDrift(t, sourceDriftJob(t), nil, engine.ReceiveRetentionDiscarded)
		if code != ExitDrift {
			t.Fatalf("exit=%d stderr=%q", code, output)
		}
		assertContainsAndExcludes(t, output, []string{
			"new link",
			"Download stopped",
			"because the source changed",
		}, []string{"wind resume", "finished partially"})
	})

	t.Run("mixed drift with unfinished sibling keeps same-link retry", func(t *testing.T) {
		job := sourceDriftJob(t)
		job.Outcome = transfer.DirectTreeOutcomePaused
		job.TerminationCause, job.TerminationInterruption = context.Canceled, transfer.TransferInterruptionCanceled
		job.Progress.DiscoveredFiles++
		job.Progress.FileOutcomes.PausedFiles++
		code, output, operation := reportSourceDrift(t, job, context.Canceled, engine.ReceiveRetentionCheckpoints)
		if code != ExitDrift {
			t.Fatalf("exit=%d stderr=%q", code, output)
		}
		assertContainsAndExcludes(t, output, []string{
			`"e/b.txt"`,
			changedSourceLabel,
			"Saved partials",
			"new link",
			fmt.Sprintf("wind resume retry -o <original-directory> --operation %x", operation.Bytes()),
			"Download paused",
		}, []string{"Download stopped"})
	})
}

func TestSourceDriftPromptMatchesOfferedActions(t *testing.T) {
	t.Run("pure drift rejects retry and offers discard or exit", func(t *testing.T) {
		app, stderr := newSourceDriftReportingApp(t)
		pause := engine.ReceivePause{
			Recovery: engine.ReceiveRecoveryOutcome{
				Retention: engine.ReceiveRetentionCheckpoints, Reason: engine.ReceiveRecoverySourceChanged, HasSourceDrift: true,
			},
			Actions:  []engine.ReceiveRecoveryAction{engine.ReceiveRecoveryDiscard, engine.ReceiveRecoveryExit},
			Transfer: sourceDriftJob(t),
		}
		action, err := app.promptReceiveRecovery(t.Context(), bufio.NewReader(strings.NewReader("r\nd\n")), pause)
		output := stderr.String()
		if err != nil || action != engine.ReceiveRecoveryDiscard {
			t.Fatalf("action=%d err=%v stderr=%q", action, err, output)
		}
		assertContainsAndExcludes(t, output, []string{
			"Download stopped",
			`"e/b.txt"`,
			changedSourceLabel,
			"[d/e]",
		}, []string{"retry", "[r/d/e]"})
	})

	t.Run("mixed drift keeps retry and notes changed items", func(t *testing.T) {
		app, stderr := newSourceDriftReportingApp(t)
		pause := engine.ReceivePause{
			Recovery: engine.ReceiveRecoveryOutcome{
				Retention: engine.ReceiveRetentionCheckpoints, Reason: engine.ReceiveRecoveryTransferFailure, HasSourceDrift: true,
			},
			Actions:  []engine.ReceiveRecoveryAction{engine.ReceiveRecoveryRetry, engine.ReceiveRecoveryDiscard, engine.ReceiveRecoveryExit},
			Transfer: sourceDriftJob(t),
		}
		action, err := app.promptReceiveRecovery(t.Context(), bufio.NewReader(strings.NewReader("r\n")), pause)
		output := stderr.String()
		if err != nil || action != engine.ReceiveRecoveryRetry {
			t.Fatalf("action=%d err=%v stderr=%q", action, err, output)
		}
		assertContainsAndExcludes(t, output, []string{
			"Download paused",
			"new link",
			"[r/d/e]",
		}, []string{"Download stopped"})
	})

	t.Run("stopped share with drift withholds retry", func(t *testing.T) {
		app, stderr := newSourceDriftReportingApp(t)
		job := sourceDriftJob(t)
		job.Progress.DiscoveredFiles++
		pause := engine.ReceivePause{
			Recovery: engine.ReceiveRecoveryOutcome{
				Retention: engine.ReceiveRetentionCheckpoints, Reason: engine.ReceiveRecoveryTransferFailure,
				ShareStop: engine.ReceiveShareStopSender, HasSourceDrift: true,
			},
			Actions:  []engine.ReceiveRecoveryAction{engine.ReceiveRecoveryDiscard, engine.ReceiveRecoveryExit},
			Transfer: job,
		}
		action, err := app.promptReceiveRecovery(t.Context(), bufio.NewReader(strings.NewReader("e\n")), pause)
		output := stderr.String()
		if err != nil || action != engine.ReceiveRecoveryExit {
			t.Fatalf("action=%d err=%v stderr=%q", action, err, output)
		}
		assertContainsAndExcludes(t, output, []string{
			"Download stopped",
			`"e/b.txt"`,
			"[d/e]",
		}, []string{"Retry", "retry"})
	})
}
