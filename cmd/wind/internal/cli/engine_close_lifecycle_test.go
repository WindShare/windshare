package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/downloadmetrics"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayprofile"
	"github.com/windshare/windshare/internal/diaglog"
)

func TestShareEngineCloseFailureAlignsExitCodeTraceAndSummary(t *testing.T) {
	t.Run("published", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		relays := &cliShareRelays{
			accepting:      make(chan struct{}),
			stopped:        make(chan struct{}),
			cleanupStarted: make(chan context.Context, 1),
			cleanupRelease: make(chan struct{}),
		}
		close(relays.cleanupRelease)

		var stdout, stderr bytes.Buffer
		trace := newFakeUserTrace(diaglog.RunStatus{})
		app := &App{
			Stdout: &cancelOnWriteBuffer{buf: &stdout, cancel: cancel},
			Stderr: &stderr,
			Stdin:  strings.NewReader(""),
			terminalCapabilities: terminalcanvas.CapabilityProviderFunc(func() terminalcanvas.Capabilities {
				return terminalcanvas.Capabilities{Columns: 200}
			}),
			engineConfig: engine.Config{Share: engine.ShareDependencies{
				Prepare: func(ctx context.Context, config liveshare.SenderConfig) (engine.PreparedShare, error) {
					leakCatalogBudget(t, config.CatalogBudget)
					prepared, err := liveshare.PrepareSender(ctx, config)
					if prepared == nil {
						return nil, err
					}
					return preparedSenderAdapter{PreparedSender: prepared}, err
				},
				Relays:      func(context.Context, []relayprofile.Registration) (engine.ShareRelays, error) { return relays, nil },
				PollNetwork: func(ctx context.Context, _ func()) { <-ctx.Done() },
			}},
			openUserTrace: func(diaglog.Target, string, diaglog.RunOptions) (runTraceRecorder, error) {
				return trace, nil
			},
		}
		t.Cleanup(app.closeTerminalOutput)

		code := app.Run(ctx, []string{
			"share", t.TempDir(),
			"--relay", relays.ReadyRelayURL(),
			"--front-url", "https://front.example",
			"--trace", filepath.Join(t.TempDir(), "share.ndjson"),
		})
		if code != ExitFailure {
			t.Fatalf("exit=%d want=%d stderr=%q", code, ExitFailure, stderr.String())
		}
		if stdout.Len() == 0 {
			t.Fatal("published share did not write capability link")
		}
		output := stderr.String()
		if strings.Contains(output, "Sharing stopped") {
			t.Fatalf("stderr reported clean stop after Engine.Close failure: %q", output)
		}
		closeIdx := strings.Index(output, "wind: close application:")
		failIdx := strings.Index(output, "Sharing failed:")
		if closeIdx < 0 || failIdx < 0 || closeIdx >= failIdx {
			t.Fatalf("stderr order = %q", output)
		}

		records := trace.recorded()
		shutdownIdx, terminalIdx := -1, -1
		var stopped struct {
			Publication    string `json:"publication"`
			ExitCode       int    `json:"exit_code"`
			StoppedCleanly bool   `json:"stopped_cleanly"`
			Failure        *struct {
				Code string `json:"code"`
			} `json:"failure"`
		}
		for i, rec := range records {
			switch rec.Event {
			case "engine_shutdown_failed":
				shutdownIdx = i
			case "sharing_stopped":
				terminalIdx = i
				decodeRecordPayload(t, rec.Payload, &stopped)
			}
		}
		if shutdownIdx < 0 || terminalIdx < 0 || shutdownIdx >= terminalIdx {
			t.Fatalf("trace order shutdown=%d terminal=%d records=%#v", shutdownIdx, terminalIdx, records)
		}
		if stopped.Publication != "published" || stopped.ExitCode != ExitFailure ||
			stopped.StoppedCleanly || stopped.Failure == nil || stopped.Failure.Code != "unexpected" {
			t.Fatalf("sharing_stopped = %+v", stopped)
		}
	})

	t.Run("unpublished", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		waitReadyEntered := make(chan struct{})
		relays := &blockingReadyRelays{
			cliShareRelays: cliShareRelays{
				stopped:        make(chan struct{}),
				cleanupStarted: make(chan context.Context, 1),
				cleanupRelease: make(chan struct{}),
			},
			waitReadyEntered: waitReadyEntered,
		}
		close(relays.cleanupRelease)
		go func() {
			<-waitReadyEntered
			cancel()
		}()

		var stdout, stderr bytes.Buffer
		trace := newFakeUserTrace(diaglog.RunStatus{})
		app := &App{
			Stdout: &stdout,
			Stderr: &stderr,
			Stdin:  strings.NewReader(""),
			terminalCapabilities: terminalcanvas.CapabilityProviderFunc(func() terminalcanvas.Capabilities {
				return terminalcanvas.Capabilities{Columns: 200}
			}),
			engineConfig: engine.Config{Share: engine.ShareDependencies{
				Prepare: func(ctx context.Context, config liveshare.SenderConfig) (engine.PreparedShare, error) {
					leakCatalogBudget(t, config.CatalogBudget)
					prepared, err := liveshare.PrepareSender(ctx, config)
					if prepared == nil {
						return nil, err
					}
					return preparedSenderAdapter{PreparedSender: prepared}, err
				},
				Relays:      func(context.Context, []relayprofile.Registration) (engine.ShareRelays, error) { return relays, nil },
				PollNetwork: func(ctx context.Context, _ func()) { <-ctx.Done() },
			}},
			openUserTrace: func(diaglog.Target, string, diaglog.RunOptions) (runTraceRecorder, error) {
				return trace, nil
			},
		}
		t.Cleanup(app.closeTerminalOutput)

		code := app.Run(ctx, []string{
			"share", t.TempDir(),
			"--relay", relays.ReadyRelayURL(),
			"--front-url", "https://front.example",
			"--trace", filepath.Join(t.TempDir(), "share.ndjson"),
		})
		if code != ExitFailure || stdout.Len() != 0 {
			t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		output := stderr.String()
		if strings.Contains(output, "Sharing canceled") {
			t.Fatalf("stderr reported clean cancel after Engine.Close failure: %q", output)
		}
		closeIdx := strings.Index(output, "wind: close application:")
		errIdx := strings.Index(output, "Error:")
		if closeIdx < 0 || errIdx < 0 || closeIdx >= errIdx {
			t.Fatalf("stderr order = %q", output)
		}

		records := trace.recorded()
		shutdownIdx, failedIdx := -1, -1
		var failed struct {
			ExitCode int `json:"exit_code"`
			Failure  struct {
				Code string `json:"code"`
			} `json:"failure"`
		}
		for i, rec := range records {
			switch rec.Event {
			case "engine_shutdown_failed":
				shutdownIdx = i
			case "command_failed":
				failedIdx = i
				decodeRecordPayload(t, rec.Payload, &failed)
			case "sharing_stopped":
				t.Fatalf("unpublished engine close failure emitted sharing_stopped: %+v", rec)
			}
		}
		if shutdownIdx < 0 || failedIdx < 0 || shutdownIdx >= failedIdx {
			t.Fatalf("trace order shutdown=%d failed=%d records=%#v", shutdownIdx, failedIdx, records)
		}
		if failed.ExitCode != ExitFailure || failed.Failure.Code != "unexpected" {
			t.Fatalf("command_failed = %+v", failed)
		}
	})
}

func TestGetEngineCloseFailurePreservesPublishedSummaryWithoutGreenCompletion(t *testing.T) {
	stderr := &bytes.Buffer{}
	recorder := newFakeUserTrace(diaglog.RunStatus{})
	app := &App{
		Stderr: stderr,
		terminalCapabilities: terminalcanvas.CapabilityProviderFunc(func() terminalcanvas.Capabilities {
			return terminalcanvas.Capabilities{Columns: 500}
		}),
		openUserTrace: func(diaglog.Target, string, diaglog.RunOptions) (runTraceRecorder, error) {
			return recorder, nil
		},
	}
	t.Cleanup(app.closeTerminalOutput)

	runtime, err := app.newCommandRuntime(commandGet, testExactTraceOptions(filepath.Join(t.TempDir(), "get.ndjson")))
	if err != nil {
		t.Fatal(err)
	}
	settlement, err := transfer.NewDirectTreeSettlement(transfer.DirectTreeSettlementSuccess)
	if err != nil {
		t.Fatal(err)
	}
	receiveID, jobID := newGetReportingIDs(t)
	destination := filepath.Join(t.TempDir(), "project")
	settled, err := engine.SettleReceive(engine.ReceiveSettlementInput{
		Result: transfer.JobResult{
			Outcome: transfer.DirectTreeOutcomeSuccess, Settlement: settlement, SucceededFiles: 1,
			Progress: transfer.ReceiveProgressSnapshot{
				Discovery: transfer.DiscoveryComplete, CountersExact: true,
				DiscoveredFiles: 1, DiscoveredBytes: 4, VerifiedBytes: 4,
				PublishedFiles: 1, PublishedBytes: 4,
				FileOutcomes: transfer.FileOutcomeSummary{DownloadedFiles: 1},
			},
		},
		Destination: destination,
		NameAdjustments: engine.NameAdjustmentSummary{
			Count: 1,
			Samples: []engine.NameAdjustment{
				{LogicalPath: "sub/CON.txt", PhysicalPath: "sub/entry-CON.windshare-01.txt"},
			},
		},
		Operation: receiveID,
		Job:       jobID,
		Elapsed:   time.Second,
		Connectivity: downloadmetrics.Snapshot{
			DownloadID:  "dl-1",
			DirectBytes: 4,
			Final:       true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	closeErr := &engine.UnreleasedResourcesError{CacheBytes: 4096}
	closeEngine := app.observeApplicationClose(func() error { return closeErr })
	code := app.reportReceiveTask(engine.TaskResult[engine.ReceiveResult]{Completion: settled}, receiveSelection{}, runtime, closeEngine())
	_ = closeEngine()
	runtime.Close()

	if code != ExitFailure {
		t.Fatalf("exit=%d want=%d", code, ExitFailure)
	}
	output := stderr.String()
	if strings.Contains(output, "Download completed") {
		t.Fatalf("stderr reported green completion after Engine.Close failure: %q", output)
	}
	closeIdx := strings.Index(output, "wind: close application:")
	failIdx := strings.Index(output, "Download failed")
	if closeIdx < 0 || failIdx < 0 || closeIdx >= failIdx {
		t.Fatalf("stderr order = %q", output)
	}
	for _, want := range []string{
		"4096 cached bytes",
		"Destination: " + destination,
		"1 downloaded",
		"4 B",
		"1 item saved under a compatible name:",
		"sub/CON.txt -> sub/entry-CON.windshare-01.txt",
		"An unexpected error occurred.",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("stderr=%q missing %q", output, want)
		}
	}

	var settledPayload struct {
		ResultStatus         string            `json:"result_status"`
		ExitCode             int               `json:"exit_code"`
		PublishedBytes       string            `json:"published_bytes"`
		NameAdjustments      string            `json:"name_adjustments"`
		FileOutcomes         map[string]string `json:"file_outcomes"`
		DownloadConnectivity *struct {
			DownloadID  string `json:"download_id"`
			DirectBytes string `json:"direct_bytes"`
		} `json:"download_connectivity"`
		Failure *struct {
			Code string `json:"code"`
		} `json:"failure"`
	}
	foundSettled := false
	for _, rec := range recorder.recorded() {
		if rec.Event == "transfer_settled" {
			foundSettled = true
			if rec.Level != "error" {
				t.Fatalf("transfer_settled level = %q, want error", rec.Level)
			}
			decodeRecordPayload(t, rec.Payload, &settledPayload)
		}
	}
	if !foundSettled {
		t.Fatal("transfer_settled missing from trace")
	}
	if settledPayload.ResultStatus != "failed" || settledPayload.ExitCode != ExitFailure ||
		settledPayload.PublishedBytes != "4" || settledPayload.NameAdjustments != "1" ||
		settledPayload.FileOutcomes["downloaded_files"] != "1" ||
		settledPayload.DownloadConnectivity == nil || settledPayload.DownloadConnectivity.DownloadID != "dl-1" ||
		settledPayload.Failure == nil || settledPayload.Failure.Code != "unexpected" {
		t.Fatalf("transfer_settled = %+v", settledPayload)
	}
}

func TestEngineCloseFailureDoesNotOverridePrimaryFailure(t *testing.T) {
	closeErr := &engine.UnreleasedResourcesError{CacheBytes: 128}

	t.Run("share publication usage error", func(t *testing.T) {
		recorder := newFakeUserTrace(diaglog.RunStatus{})
		app := &App{
			Stderr: &bytes.Buffer{},
			openUserTrace: func(diaglog.Target, string, diaglog.RunOptions) (runTraceRecorder, error) {
				return recorder, nil
			},
		}
		runtime, err := app.newCommandRuntime(commandShare, testExactTraceOptions("share.ndjson"))
		if err != nil {
			t.Fatal(err)
		}
		code := reportShareResult(runtime, engine.TaskResult[engine.ShareResult]{}, errShareCapabilityEncoding, closeErr)
		runtime.Close()
		if code != ExitUsage {
			t.Fatalf("exit=%d want=%d", code, ExitUsage)
		}
	})

	t.Run("get source drift error", func(t *testing.T) {
		recorder := newFakeUserTrace(diaglog.RunStatus{})
		app := &App{
			Stderr: &bytes.Buffer{},
			openUserTrace: func(diaglog.Target, string, diaglog.RunOptions) (runTraceRecorder, error) {
				return recorder, nil
			},
		}
		runtime, err := app.newCommandRuntime(commandGet, testExactTraceOptions("get.ndjson"))
		if err != nil {
			t.Fatal(err)
		}
		partial, err := transfer.NewDirectTreeSettlement(transfer.DirectTreeSettlementPartial)
		if err != nil {
			t.Fatal(err)
		}
		receiveID, jobID := newGetReportingIDs(t)
		settled, err := engine.SettleReceive(engine.ReceiveSettlementInput{
			Result: transfer.JobResult{
				Outcome: transfer.DirectTreeOutcomePartial, Settlement: partial,
				SourceDriftFault: mustCLIFault(transferfault.NewSource(transferfault.ScopeFileLocal, transferfault.SourceRevisionChanged)),
			},
			Destination: t.TempDir(), Operation: receiveID, Job: jobID,
		})
		if err != nil {
			t.Fatal(err)
		}
		code := app.reportEngineReceive(settled, receiveSelection{}, runtime, closeErr)
		runtime.Close()
		if code != ExitDrift {
			t.Fatalf("exit=%d want=%d", code, ExitDrift)
		}
	})
}

func TestObserveApplicationCloseRunsAndReportsOnce(t *testing.T) {
	var stderr bytes.Buffer
	app := &App{Stderr: &stderr}
	t.Cleanup(app.closeTerminalOutput)

	calls := 0
	boom := errors.New("retained test budget")
	closeEngine := app.observeApplicationClose(func() error {
		calls++
		return boom
	})
	if err := closeEngine(); !errors.Is(err, boom) {
		t.Fatalf("first close = %v, want %v", err, boom)
	}
	if err := closeEngine(); !errors.Is(err, boom) {
		t.Fatalf("second close = %v, want %v", err, boom)
	}
	if calls != 1 {
		t.Fatalf("close calls = %d, want 1", calls)
	}
	if count := strings.Count(stderr.String(), "wind: close application:"); count != 1 {
		t.Fatalf("close lines = %d, stderr=%q", count, stderr.String())
	}
}

func leakCatalogBudget(t *testing.T, process *catalog.BudgetAccount) {
	t.Helper()
	shareBudget, err := catalog.NewBudgetAccount("leak-share", catalog.DefaultShareBudgetLimits())
	if err != nil {
		t.Fatal(err)
	}
	sessionBudget, err := catalog.NewBudgetAccount("leak-session", catalog.DefaultSessionBudgetLimits())
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := catalog.ReserveHierarchy(
		catalog.BudgetHierarchy{Process: process, Share: shareBudget, Session: sessionBudget},
		catalog.ResourceUsage{Entries: 1, MemoryBytes: 64},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reservation.Release)
}

type cancelOnWriteBuffer struct {
	buf    *bytes.Buffer
	cancel context.CancelFunc
}

func (w *cancelOnWriteBuffer) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	w.cancel()
	return n, err
}

type blockingReadyRelays struct {
	cliShareRelays
	waitReadyEntered chan struct{}
}

func (relays *blockingReadyRelays) WaitReady(ctx context.Context) error {
	close(relays.waitReadyEntered)
	<-ctx.Done()
	return ctx.Err()
}

type preparedSenderAdapter struct {
	*liveshare.PreparedSender
}

func (a preparedSenderAdapter) NewRuntimeFactory(config liveshare.RuntimeFactoryConfig) (engine.ShareSessionFactory, error) {
	return a.PreparedSender.NewRuntimeFactory(config)
}
