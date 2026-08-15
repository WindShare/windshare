package cli

import (
	"bytes"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/transfer"
)

type recordingProgressSink struct {
	measures []transfer.SelectionMeasure
	finishes int
}

func (sink *recordingProgressSink) Update(measure transfer.SelectionMeasure) {
	sink.measures = append(sink.measures, measure)
}

func (sink *recordingProgressSink) Finish() { sink.finishes++ }

func TestGetProgressReporterHonorsTTYInjectionAndStreamBoundary(t *testing.T) {
	var stdout, stderr bytes.Buffer
	recording := &recordingProgressSink{}
	app := &App{
		Stdout: &stdout, Stderr: &stderr,
		getProgress: recording,
		getTTYDetector: TTYDetectorFunc(func(writer io.Writer) bool {
			return false
		}),
	}
	measure := transfer.SelectionMeasure{Discovery: transfer.DiscoveryOpen, DiscoveredFiles: 2}
	reporter := app.getProgressReporter()
	reporter.Update(measure)
	reporter.Finish()
	if len(recording.measures) != 0 || recording.finishes != 0 {
		t.Fatalf("non-TTY invoked progress sink: %+v", recording)
	}

	app.getTTYDetector = TTYDetectorFunc(func(writer io.Writer) bool { return true })
	reporter = app.getProgressReporter()
	reporter.Update(measure)
	reporter.Finish()
	if len(recording.measures) != 1 || recording.finishes != 1 {
		t.Fatalf("TTY progress sink=%+v", recording)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("injected progress wrote stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestTerminalGetProgressIsSingleLineAndOpenDiscoveryHasNoPercentage(t *testing.T) {
	var output bytes.Buffer
	sink := &terminalProgressSink{writer: &output}
	sink.Update(transfer.SelectionMeasure{
		Discovery: transfer.DiscoveryOpen, DiscoveredFiles: 3, DiscoveredBytes: 9,
		CompletedFiles: 1, CompletedBytes: 3,
	})
	openLine := output.String()
	if strings.Contains(openLine, "%") {
		t.Fatalf("open discovery progress=%q", openLine)
	}
	if !strings.HasPrefix(openLine, "\rget: discovering") || strings.Contains(openLine, "\n") {
		t.Fatalf("open discovery was not a single refresh line: %q", openLine)
	}

	completeAt := output.Len()
	sink.Update(transfer.SelectionMeasure{
		Discovery: transfer.DiscoveryComplete, DiscoveredFiles: 3, DiscoveredBytes: math.MaxUint64,
		CompletedFiles: 3, CompletedBytes: math.MaxUint64,
	})
	completeLine := output.String()[completeAt:]
	if !strings.Contains(completeLine, "100%") || strings.Contains(completeLine, "\n") {
		t.Fatalf("complete progress=%q", completeLine)
	}
	sink.Finish()
	if !strings.HasSuffix(output.String(), "\r") || strings.Contains(output.String(), "\n") {
		t.Fatalf("finished progress=%q", output.String())
	}
}

func TestGetLiveOnlyWarningIsEmittedOnceOnStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	warning := app.getWarningReporter()
	warning.Warn(liveOnlyOutputWarning)
	warning.Warn(liveOnlyOutputWarning)
	if count := strings.Count(stderr.String(), liveOnlyOutputWarning); count != 1 {
		t.Fatalf("warning count=%d stderr=%q", count, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("warning wrote stdout=%q", stdout.String())
	}
}

func TestGetSummaryUsesTypedSettlementAggregate(t *testing.T) {
	partial, err := transfer.NewDirectTreeSettlement(transfer.DirectTreeSettlementPartial)
	if err != nil {
		t.Fatal(err)
	}
	result := transfer.JobResult{
		Outcome: transfer.DirectTreeOutcomePartial, Settlement: partial,
		FileOutcomes: transfer.FileOutcomeSummary{
			DownloadedFiles: 2, ResumedFiles: 1, PausedFiles: 1,
			CollisionFiles: 1, FailedFiles: 1, ItemBlockedFiles: 1,
			ModifiedTimeWarnings: 2,
		},
		Files:               []transfer.FileJobFailure{{Path: "unsettled.bin"}},
		Directories:         []transfer.DirectoryJobFailure{{Path: "isolated"}},
		OmittedFileFailures: 2, OmittedDirectoryFailures: 3,
		Measure: transfer.SelectionMeasure{CompletedBytes: 99},
	}
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	app.logGetTransferSummary(result, true)
	want := "get: result=partial downloaded=2 resumed=1 paused=1 collision=1 failed=1 " +
		"item-blocked=1 directories-failed=4 renamed=true metadata-warnings=2 " +
		"omitted-diagnostics=5 bytes=99\n"
	if stderr.String() != want {
		t.Fatalf("summary=%q want=%q", stderr.String(), want)
	}
	if stdout.Len() != 0 {
		t.Fatalf("summary wrote stdout=%q", stdout.String())
	}
}

func TestSuccessfulGetWritesOnlyFinalSummaryToRedirectedStderr(t *testing.T) {
	published, err := transfer.NewDirectTreeSettlement(transfer.DirectTreeSettlementSuccess)
	if err != nil {
		t.Fatal(err)
	}
	result := transfer.JobResult{
		Outcome: transfer.DirectTreeOutcomeSuccess, Settlement: published, SucceededFiles: 1,
		FileOutcomes: transfer.FileOutcomeSummary{DownloadedFiles: 1, ModifiedTimeWarnings: 1},
		Measure: transfer.SelectionMeasure{
			Discovery: transfer.DiscoveryComplete, DiscoveryTerminalSuccess: true,
			DiscoveredFiles: 1, CompletedFiles: 1, DiscoveredBytes: 7, CompletedBytes: 7,
		},
	}
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if code := app.reportTransferResultWithAdmission(nil, nil, nil, result, nil, true); code != ExitOK {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	want := "get: result=success downloaded=1 resumed=0 paused=0 collision=0 failed=0 " +
		"item-blocked=0 directories-failed=0 renamed=true metadata-warnings=1 " +
		"omitted-diagnostics=0 bytes=7\n"
	if stderr.String() != want {
		t.Fatalf("stderr=%q want=%q", stderr.String(), want)
	}
	if stdout.Len() != 0 {
		t.Fatalf("successful get wrote stdout=%q", stdout.String())
	}
}
