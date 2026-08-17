package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
	"github.com/windshare/windshare/cmd/windshare/internal/commandprojection"
	"github.com/windshare/windshare/cmd/windshare/internal/runtrace"
	"github.com/windshare/windshare/cmd/windshare/internal/terminalcanvas"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
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
		clievent.CommandGet,
		observationOptions{verbose: verbose},
	)
	if err != nil {
		t.Fatal(err)
	}
	return runtime, stderr
}

func newGetReportingCompletion(t *testing.T, result transfer.JobResult) getTransferCompletion {
	t.Helper()
	receiveID, err := receivecontract.OperationIDFromBytes(bytes.Repeat([]byte{1}, 16))
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := transfer.TransferJobIDFromBytes(bytes.Repeat([]byte{2}, 16))
	if err != nil {
		t.Fatal(err)
	}
	progress, err := commandprojection.ProjectTransferProgress(receiveID, jobID, result.Progress)
	if err != nil {
		progress, err = commandprojection.ProjectTransferProgress(
			receiveID,
			jobID,
			transfer.ReceiveProgressSnapshot{
				Discovery:     transfer.DiscoveryComplete,
				CountersExact: true,
			},
		)
	}
	if err != nil {
		t.Fatal(err)
	}
	return getTransferCompletion{result: result, finalProgress: progress}
}

func TestGetResultUsesAuthorityDestinationAndHumanOutcomeVocabulary(t *testing.T) {
	runtime, stderr := newGetReportingRuntime(t, false, false)
	observation := getObservation{runtime: runtime}
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
		t.Context(), newGetReportingCompletion(t, result), nil, nil, nil,
		destination, true, startedAt, observation,
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

func TestGetFinalProgressAndSettlementUseOrderedFinalization(t *testing.T) {
	runtime, writer, recorder := newSaturatedCommandRuntime(t, clievent.CommandGet, false)
	settlement, err := transfer.NewDirectTreeSettlement(transfer.DirectTreeSettlementSuccess)
	if err != nil {
		t.Fatal(err)
	}
	result := transfer.JobResult{
		Outcome:        transfer.DirectTreeOutcomeSuccess,
		Settlement:     settlement,
		SucceededFiles: 1,
		Progress: transfer.ReceiveProgressSnapshot{
			Discovery:          transfer.DiscoveryComplete,
			CountersExact:      true,
			DiscoveredFiles:    1,
			DiscoveredBytes:    7,
			VerifiedBytes:      7,
			PublishedFiles:     1,
			PublishedBytes:     7,
			NewlyVerifiedBytes: 7,
			FileOutcomes:       transfer.FileOutcomeSummary{DownloadedFiles: 1},
		},
	}
	if code := (&App{}).reportTransferResultWithAdmission(
		t.Context(),
		newGetReportingCompletion(t, result),
		nil,
		nil,
		nil,
		t.TempDir(),
		false,
		runtime.Clock().Now(),
		getObservation{runtime: runtime},
	); code != ExitOK {
		t.Fatalf("exit=%d", code)
	}
	closeBlockedRuntime(t, runtime, writer)

	assertRuntimeEventTypes(t, recorder.recorded(),
		clievent.Warning{},
		clievent.Warning{},
		clievent.ObserverLossObserved{},
		clievent.TransferProgress{},
		clievent.TransferSettled{},
	)
}

func TestGetNonSuccessNeverUsesCompletedWordingAndKeepsOutcomeCounts(t *testing.T) {
	runtime, stderr := newGetReportingRuntime(t, false, false)
	observation := getObservation{runtime: runtime}
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
		t.Context(), newGetReportingCompletion(t, result), nil, nil, nil,
		t.TempDir(), false, runtime.Clock().Now(), observation,
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
				t.Context(), newGetReportingCompletion(t, test.result), test.admissionErr, nil, nil,
				t.TempDir(), false, runtime.Clock().Now(), getObservation{runtime: runtime},
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
	observation := getObservation{runtime: runtime}
	receiveID, err := receivecontract.OperationIDFromBytes(bytes.Repeat([]byte{1}, 16))
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := transfer.TransferJobIDFromBytes(bytes.Repeat([]byte{2}, 16))
	if err != nil {
		t.Fatal(err)
	}
	observation.progress(receiveID, jobID, transfer.ReceiveProgressSnapshot{
		Discovery: transfer.DiscoveryOpen, CountersExact: true,
		DiscoveredFiles: 2, DiscoveredBytes: 10, VerifiedBytes: 4, NewlyVerifiedBytes: 1,
	})
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
	code := (getObservation{runtime: runtime}).commandFailure(ExitFailure, getHostileError{secret: secret})
	if code != ExitFailure {
		t.Fatalf("exit=%d", code)
	}
	runtime.Close()
	if strings.Contains(stderr.String(), secret) || !strings.Contains(stderr.String(), "unexpected") {
		t.Fatalf("unsafe failure output=%q", stderr.String())
	}
}

func TestGetWarningUsesClosedFailureVocabulary(t *testing.T) {
	runtime, stderr := newGetReportingRuntime(t, false, false)
	(getObservation{runtime: runtime}).warningCode(clievent.FailureOutputUnsupportedFilesystem)
	runtime.Close()
	if output := stderr.String(); !strings.Contains(output, "Warning") || !strings.Contains(output, "destination") {
		t.Fatalf("warning=%q", output)
	}
}

type getIncompleteTraceRecorder struct {
	health chan clievent.TraceIncomplete
}

func (*getIncompleteTraceRecorder) Record(clievent.Event) bool { return true }
func (*getIncompleteTraceRecorder) ReportUpstreamLoss(uint64, uint64) bool {
	return true
}
func (recorder *getIncompleteTraceRecorder) Health() <-chan clievent.TraceIncomplete {
	return recorder.health
}
func (*getIncompleteTraceRecorder) Close() runtrace.Status {
	return runtrace.Status{WriterFailed: true}
}

func TestGetTraceRuntimeFailureCannotReclassifySuccess(t *testing.T) {
	stderr := &bytes.Buffer{}
	recorder := &getIncompleteTraceRecorder{health: make(chan clievent.TraceIncomplete)}
	app := &App{
		Stderr: stderr,
		terminalCapabilities: terminalcanvas.CapabilityProviderFunc(func() terminalcanvas.Capabilities {
			return terminalcanvas.Capabilities{Columns: 500}
		}),
		openUserTrace: func(
			string,
			clievent.Command,
			runtrace.Config,
			runtrace.Dependencies,
		) (userTraceRecorder, error) {
			return recorder, nil
		},
	}
	runtime, err := app.newCommandRuntime(
		clievent.CommandGet,
		observationOptions{tracePath: filepath.Join(t.TempDir(), "get.ndjson")},
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
		t.Context(), newGetReportingCompletion(t, result), nil, nil, nil,
		t.TempDir(), false, runtime.Clock().Now(), getObservation{runtime: runtime},
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
