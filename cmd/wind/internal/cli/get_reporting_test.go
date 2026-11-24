package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/core/transfer/revisionwait"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/internal/diaglog"
)

func newGetReportingRuntime(t *testing.T, interactive, verbose bool) (*commandRuntime, *bytes.Buffer) {
	t.Helper()
	stderr := &bytes.Buffer{}
	app := &App{
		Stderr: stderr,
		terminalCapabilities: terminalcanvas.CapabilityProviderFunc(func() terminalcanvas.Capabilities {
			return terminalcanvas.Capabilities{Interactive: interactive, Unicode: false, Columns: 500}
		}),
	}
	runtime, err := app.newCommandRuntime(
		commandGet,
		observationOptions{verbose: verbose},
	)
	if err != nil {
		t.Fatal(err)
	}
	return runtime, stderr
}

func newGetReportingIDs(t *testing.T) (receivecontract.OperationID, transfer.TransferJobID) {
	t.Helper()
	receiveID, err := receivecontract.OperationIDFromBytes(bytes.Repeat([]byte{1}, 16))
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := transfer.TransferJobIDFromBytes(bytes.Repeat([]byte{2}, 16))
	if err != nil {
		t.Fatal(err)
	}
	return receiveID, jobID
}

func TestGetResultUsesAuthorityDestinationAndHumanOutcomeVocabulary(t *testing.T) {
	runtime, stderr := newGetReportingRuntime(t, false, false)
	published, err := transfer.NewDirectTreeSettlement(transfer.DirectTreeSettlementSuccess)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "report (1).txt")
	result := transfer.JobResult{
		Outcome: transfer.DirectTreeOutcomeSuccess, Settlement: published, SucceededFiles: 1,
		Progress: transfer.ReceiveProgressSnapshot{
			Discovery: transfer.DiscoveryComplete, CountersExact: true,
			DiscoveredFiles: 1, DiscoveredBytes: 7, VerifiedBytes: 7,
			PublishedFiles: 1, PublishedBytes: 7, NewlyVerifiedBytes: 3,
			FileOutcomes: transfer.FileOutcomeSummary{DownloadedFiles: 1},
		},
	}
	startedAt := runtime.Clock().Now().Add(-2 * time.Second)
	if code := (&App{}).reportTransferResultWithAdmission(
		t, t.Context(), result, nil, nil, nil,
		destination, true, startedAt, runtime,
	); code != ExitOK {
		t.Fatalf("exit=%d", code)
	}
	runtime.Close()
	output := stderr.String()
	for _, want := range []string{"Download completed", "Destination: " + destination, "1 downloaded", "7 B"} {
		if !strings.Contains(output, want) {
			t.Fatalf("stderr=%q missing %q", output, want)
		}
	}
	if strings.Contains(output, "result=") || strings.Contains(output, "completed_files=") {
		t.Fatalf("stderr retained internal ledger: %q", output)
	}
}

func TestGetNonSuccessNeverUsesCompletedWordingAndKeepsOutcomeCounts(t *testing.T) {
	runtime, stderr := newGetReportingRuntime(t, false, false)
	settlement, err := transfer.NewDirectTreeSettlement(transfer.DirectTreeSettlementPartial)
	if err != nil {
		t.Fatal(err)
	}
	result := transfer.JobResult{
		Outcome: transfer.DirectTreeOutcomePartial, Settlement: settlement,
		Progress: transfer.ReceiveProgressSnapshot{
			Discovery: transfer.DiscoveryComplete, CountersExact: true,
			DiscoveredFiles: 6,
			FileOutcomes: transfer.FileOutcomeSummary{
				DownloadedFiles: 1, ResumedFiles: 1, PausedFiles: 1,
				CollisionFiles: 1, ItemBlockedFiles: 1, FailedFiles: 1,
			},
		},
	}
	if code := (&App{}).reportTransferResultWithAdmission(
		t, t.Context(), result, nil, nil, nil,
		t.TempDir(), false, runtime.Clock().Now(), runtime,
	); code != ExitFailure {
		t.Fatalf("exit=%d", code)
	}
	runtime.Close()
	output := stderr.String()
	if strings.Contains(output, "Download completed") {
		t.Fatalf("partial result used success wording: %q", output)
	}
	for _, want := range []string{"partial", "1 downloaded", "1 resumed", "1 paused", "1 collision", "1 item-blocked", "1 failed"} {
		if !strings.Contains(output, want) {
			t.Fatalf("stderr=%q missing %q", output, want)
		}
	}
}

func TestGetCapacityBudgetPauseReportsResumableJobWithoutFileFailure(t *testing.T) {
	runtime, stderr := newGetReportingRuntime(t, false, false)
	settlement, err := transfer.NewDirectTreeSettlement(transfer.DirectTreeSettlementPaused)
	if err != nil {
		t.Fatal(err)
	}
	faultValue, err := transferfault.NewSession(
		transferfault.ScopeOutputPause,
		transferfault.SessionResourceBudget,
	)
	if err != nil {
		t.Fatal(err)
	}
	result := transfer.JobResult{
		Outcome: transfer.DirectTreeOutcomePaused, Settlement: settlement,
		TerminationFault: faultValue,
		Progress: transfer.ReceiveProgressSnapshot{
			Discovery: transfer.DiscoveryComplete, CountersExact: true,
			CapacityWait: revisionwait.Snapshot{
				AccumulatedWait: revisionwait.DefaultWaitBudget, Attempts: 4,
			},
		},
	}
	code := (&App{}).reportTransferResultWithAdmission(
		t, t.Context(), result, nil, nil, nil,
		t.TempDir(), false, runtime.Clock().Now(), runtime,
	)
	runtime.Close()
	output := stderr.String()
	if code != ExitFailure || !strings.Contains(output, "Download paused") ||
		!strings.Contains(output, "0 downloaded") || strings.Contains(output, "1 failed") {
		t.Fatalf("capacity-budget pause exit=%d output=%q", code, output)
	}
}

func TestGetResultIntegrationPreservesEveryNonSuccessExitClass(t *testing.T) {
	driftFault, err := transferfault.NewSource(
		transferfault.ScopeFileLocal,
		transferfault.SourceRevisionChanged,
	)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name         string
		result       transfer.JobResult
		admissionErr error
		wantExit     int
		wantText     string
	}{
		{
			name: "paused local",
			result: transfer.JobResult{
				Outcome: transfer.DirectTreeOutcomePaused, TerminationCause: context.Canceled,
			},
			wantExit: ExitFailure, wantText: "Download paused",
		},
		{
			name: "failed network",
			result: transfer.JobResult{
				Outcome: transfer.DirectTreeOutcomeFailed, TerminationCause: errors.New("private network detail"),
			},
			admissionErr: errors.New("private admission detail"),
			wantExit:     ExitNetwork, wantText: "Download failed",
		},
		{
			name: "source drift",
			result: transfer.JobResult{
				Outcome: transfer.DirectTreeOutcomePartial, SourceDriftFault: driftFault,
			},
			wantExit: ExitDrift, wantText: "because the source changed",
		},
		{
			name: "proven missing selection",
			result: transfer.JobResult{
				Outcome: transfer.DirectTreeOutcomePartial,
				Progress: transfer.ReceiveProgressSnapshot{
					Discovery: transfer.DiscoveryComplete, CountersExact: true,
				},
				SelectionResolutionFailure: transfer.ErrSelectionTargetMissing,
			},
			wantExit: ExitUsage, wantText: "selected content was not found",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime, stderr := newGetReportingRuntime(t, false, false)
			code := (&App{}).reportTransferResultWithAdmission(
				t, t.Context(), test.result, test.admissionErr, nil, nil,
				t.TempDir(), false, runtime.Clock().Now(), runtime,
			)
			runtime.Close()
			if code != test.wantExit {
				t.Fatalf("exit=%d want=%d stderr=%q", code, test.wantExit, stderr.String())
			}
			output := stderr.String()
			if !strings.Contains(output, test.wantText) || strings.Contains(output, "Download completed") ||
				strings.Contains(output, "private network detail") || strings.Contains(output, "private admission detail") {
				t.Fatalf("non-success output=%q want=%q", output, test.wantText)
			}
		})
	}
}

func TestGetProgressProjectsVerifiedAndNewBytesWithoutLegacyMeasure(t *testing.T) {
	runtime, stderr := newGetReportingRuntime(t, true, false)
	receiveID, jobID := newGetReportingIDs(t)
	runtime.ObserveEngine(engine.Observation{Event: engine.TransferProgress{
		Operation: receiveID,
		Job:       jobID,
		Snapshot: transfer.ReceiveProgressSnapshot{
			Discovery: transfer.DiscoveryOpen, CountersExact: true,
			DiscoveredFiles: 2, DiscoveredBytes: 10, VerifiedBytes: 4, NewlyVerifiedBytes: 1,
		},
	}})
	runtime.Close()
	output := stderr.String()
	if strings.Contains(output, "%") || !strings.Contains(output, "Discovering") || !strings.Contains(output, "10 B ready") {
		t.Fatalf("open discovery progress=%q", output)
	}
}

type getHostileError struct{ secret string }

func (getHostileError) Error() string { panic("provider Error must not be rendered") }

func TestGetFailureProjectionDoesNotRenderProviderText(t *testing.T) {
	runtime, stderr := newGetReportingRuntime(t, false, false)
	secret := "provider-secret-and-local-path"
	runtime.StageCommandFailure(ExitFailure, classifyFailure(getHostileError{secret: secret}))
	runtime.Close()
	if strings.Contains(stderr.String(), secret) || !strings.Contains(stderr.String(), "unexpected") {
		t.Fatalf("unsafe failure output=%q", stderr.String())
	}
}

func TestGetWarningUsesClosedFailureVocabulary(t *testing.T) {
	runtime, stderr := newGetReportingRuntime(t, false, false)
	runtime.ObserveEngine(engine.Observation{Event: engine.Warning{Failure: engine.Failure{Code: engine.FailureOutputFailed}}})
	runtime.Close()
	if output := stderr.String(); !strings.Contains(output, "Warning") || !strings.Contains(output, "destination") {
		t.Fatalf("warning=%q", output)
	}
}

func TestGetTraceRuntimeFailureCannotReclassifySuccess(t *testing.T) {
	stderr := &bytes.Buffer{}
	recorder := newFakeUserTrace(diaglog.RunStatus{WriterFailed: true})
	app := &App{
		Stderr: stderr,
		terminalCapabilities: terminalcanvas.CapabilityProviderFunc(func() terminalcanvas.Capabilities {
			return terminalcanvas.Capabilities{Columns: 500}
		}),
		openUserTrace: func(diaglog.Target, string, diaglog.RunOptions) (runTraceRecorder, error) {
			return recorder, nil
		},
	}
	runtime, err := app.newCommandRuntime(
		commandGet,
		testExactTraceOptions(filepath.Join(t.TempDir(), "get.ndjson")),
	)
	if err != nil {
		t.Fatal(err)
	}
	settlement, err := transfer.NewDirectTreeSettlement(transfer.DirectTreeSettlementSuccess)
	if err != nil {
		t.Fatal(err)
	}
	result := transfer.JobResult{
		Outcome: transfer.DirectTreeOutcomeSuccess, Settlement: settlement,
		Progress: transfer.ReceiveProgressSnapshot{
			Discovery: transfer.DiscoveryComplete, CountersExact: true,
		},
	}
	code := app.reportTransferResultWithAdmission(
		t, t.Context(), result, nil, nil, nil,
		t.TempDir(), false, runtime.Clock().Now(), runtime,
	)
	runtime.Close()
	if code != ExitOK {
		t.Fatalf("trace runtime failure changed exit=%d", code)
	}
	output := stderr.String()
	if !strings.Contains(output, "Download completed") || !strings.Contains(output, "Trace is incomplete") {
		t.Fatalf("trace failure output=%q", output)
	}
}

func (a *App) reportTransferResultWithAdmission(
	t *testing.T,
	ctx context.Context,
	result transfer.JobResult,
	admissionErr error,
	runtimeErr error,
	connectionErr error,
	destination string,
	destinationAdjusted bool,
	startedAt time.Time,
	runtime *commandRuntime,
) int {
	t.Helper()
	receiveID, jobID := newGetReportingIDs(t)
	now := runtime.Clock().Now()
	elapsed := max(now.Sub(startedAt), 0)
	settled, err := engine.SettleReceive(engine.ReceiveSettlementInput{
		Result: result, AdmissionError: admissionErr,
		RuntimeError: runtimeErr, ConnectionError: connectionErr, ContextError: ctx.Err(),
		Elapsed: elapsed, Destination: destination,
		DestinationAdjusted: destinationAdjusted,
		Operation:           receiveID,
		Job:                 jobID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a.reportEngineReceive(settled, receiveSelection{}, runtime)
}

func TestMissingSelectionEchoesTypedOnlyValuesInCommandLineOrder(t *testing.T) {
	const hint = "--only paths start with the shared file or folder name (<name>/...) and are case-sensitive."
	many := receiveSelection{}
	var manyMissing []string
	for index := range maxReportedMissingSelections + 3 {
		path := fmt.Sprintf("file-%02d", index)
		many.paths, many.typed = append(many.paths, path), append(many.typed, path+"/")
		manyMissing = append(manyMissing, path)
	}
	manyLines := []string{"Not found in this share:"}
	for _, typed := range many.typed[:maxReportedMissingSelections] {
		manyLines = append(manyLines, fmt.Sprintf("  --only %q", typed))
	}
	tests := []struct {
		name      string
		selection receiveSelection
		missing   []string
		wantLines []string
		absent    []string
	}{
		{
			name: "typed spelling and order, present target unnamed",
			selection: receiveSelection{
				paths: []string{"d/nope", "d/a b.txt", "a b.txt"},
				typed: []string{"d/nope", "d/a b.txt", "a b.txt/"},
			},
			missing:   []string{"a b.txt", "d/nope"},
			wantLines: []string{"Not found in this share:", `  --only "d/nope"`, `  --only "a b.txt/"`, hint},
			absent:    []string{`"d/a b.txt"`, `--only "a b.txt"` + "\n", "omitted"},
		},
		{
			name:    "opaque node-ID miss names no paths",
			missing: nil, absent: []string{"Not found in this share", hint},
		},
		{
			name:      "long selection is capped",
			selection: many, missing: manyMissing,
			wantLines: append(manyLines, "  3 more omitted.", hint),
			absent:    []string{fmt.Sprintf("%q", many.typed[maxReportedMissingSelections])},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stderr := &bytes.Buffer{}
			app := &App{Stderr: stderr, terminalCapabilities: terminalcanvas.CapabilityProviderFunc(func() terminalcanvas.Capabilities {
				return terminalcanvas.Capabilities{Columns: 500}
			})}
			runtime, err := app.newCommandRuntime(commandGet, observationOptions{})
			if err != nil {
				t.Fatal(err)
			}
			settled, err := engine.SettleReceive(engine.ReceiveSettlementInput{
				Result: transfer.JobResult{
					Outcome:                    transfer.DirectTreeOutcomePartial,
					Progress:                   transfer.ReceiveProgressSnapshot{Discovery: transfer.DiscoveryComplete, CountersExact: true},
					SelectionResolutionFailure: transfer.ErrSelectionTargetMissing,
					MissingSelectionPaths:      test.missing,
				},
				Destination: t.TempDir(),
			})
			if err != nil {
				t.Fatal(err)
			}
			code := app.reportEngineReceive(settled, test.selection, runtime)
			runtime.Close()
			output := stderr.String()
			want := strings.Join(append(test.wantLines, "Correct --only and run wind get with the intended selection."), "\n") + "\n"
			if code != ExitUsage || !strings.Contains(output, want) || !strings.Contains(output, "selected content was not found") {
				t.Fatalf("exit=%d stderr=%q\nwant block=%q", code, output, want)
			}
			for _, unwanted := range test.absent {
				if strings.Contains(output, unwanted) {
					t.Fatalf("stderr=%q contains %q", output, unwanted)
				}
			}
		})
	}
}
