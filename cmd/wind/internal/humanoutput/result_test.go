package humanoutput

import (
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/engine"
)

func TestTransferResultVocabularyAndOutcomeAuthority(t *testing.T) {
	t.Parallel()
	driftFailure := &engine.Failure{Code: engine.FailureSourceChanged}
	tests := []struct {
		name      string
		status    ResultStatus
		drift     bool
		failure   *engine.Failure
		headline  string
		completed bool
	}{
		{"success", ResultSuccess, false, nil, "Download completed", true},
		{"partial", ResultPartial, false, &engine.Failure{Code: engine.FailureOutputFailed}, "finished partially", false},
		{"paused", ResultPaused, false, &engine.Failure{Code: engine.FailureOutputBusy}, "Download paused", false},
		{"failed", ResultFailed, false, &engine.Failure{Code: engine.FailureRelayUnavailable}, "Download failed", false},
		{"paused drift", ResultPaused, true, driftFailure, "Download paused", false},
		{"drift", ResultFailed, true, driftFailure, "source changed", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := TransferResult{
				Status: test.status, SourceDrift: test.drift,
				Elapsed:     9*time.Second + 600*time.Millisecond,
				Destination: "out\n\x1b[31m", DestinationAdjusted: true,
				Files: transfer.FileOutcomeSummary{
					DownloadedFiles: 3, ResumedFiles: 2, PausedFiles: 1,
					CollisionFiles: 1, ItemBlockedFiles: 2, FailedFiles: 1,
					ModifiedTimeWarnings: 1,
				},
				DirectoryFailures: 1, OmittedDiagnostics: 2,
				PublishedBytes: 5_000_000, CountersExact: false, Failure: test.failure,
			}
			var text strings.Builder
			for _, line := range formatTransferResult(result, SelectSymbols(false)) {
				text.WriteString(lineText(line))
				text.WriteByte('\n')
			}
			output := text.String()
			if strings.Contains(output, " · ") || !strings.Contains(output, " | ") {
				t.Errorf("ASCII result separators did not fall back: %q", output)
			}
			if !strings.Contains(output, test.headline) {
				t.Errorf("output %q missing %q", output, test.headline)
			}
			if strings.Contains(output, "Download completed") != test.completed {
				t.Errorf("completed vocabulary mismatch: %q", output)
			}
			if test.drift && !strings.Contains(output, "source changed") {
				t.Errorf("drift reason missing from %q", output)
			}
			for _, want := range []string{
				`Destination: out\n\x1b[31m`, "adjusted", "3 downloaded", "2 resumed", "1 paused",
				"1 collision", "2 item-blocked", "1 failed", "1 modified-time warning",
				"1 directory failed", "2 diagnostics omitted", ">=5.0 MB",
			} {
				if !strings.Contains(output, want) {
					t.Errorf("output %q missing %q", output, want)
				}
			}
		})
	}

	for _, test := range []struct {
		name      string
		drift     bool
		wantDrift bool
	}{
		{"stopped", false, false},
		{"stopped drift", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var text strings.Builder
			for _, line := range formatTransferResult(TransferResult{
				Status: ResultStopped, SourceDrift: test.drift, Elapsed: time.Second, Destination: "out",
				Files: transfer.FileOutcomeSummary{PausedFiles: 1}, CountersExact: true,
			}, SelectSymbols(false)) {
				text.WriteString(lineText(line))
				text.WriteByte('\n')
			}
			output := text.String()
			if !strings.Contains(output, "Download stopped") || !strings.Contains(output, "1 unfinished") ||
				strings.Contains(output, "because the source changed") != test.wantDrift {
				t.Fatalf("stopped output = %q, wantDrift=%t", output, test.wantDrift)
			}
		})
	}
}

func TestShareResultNeverInfersTrafficTotals(t *testing.T) {
	t.Parallel()
	clean := ShareResult{Published: true, StoppedCleanly: true, Elapsed: time.Hour}
	text := lineText(formatShareResult(clean, SelectSymbols(true)))
	if text != "✔ Sharing stopped" || strings.ContainsAny(text, "0123456789") {
		t.Fatalf("clean share result = %q", text)
	}

	failed := ShareResult{
		Published: true,
		Failure:   &engine.Failure{Code: engine.FailureRelayUnavailable},
	}
	if text := lineText(formatShareResult(failed, SelectSymbols(false))); !strings.Contains(text, "Sharing failed") {
		t.Fatalf("failed share result = %q", text)
	}
}

func TestTransferResultPreservesPublishedDestinationKind(t *testing.T) {
	t.Parallel()
	for _, destination := range []string{"./archive.zip", "./vacation_photos/"} {
		result := TransferResult{Status: ResultSuccess, Destination: destination, CountersExact: true}
		lines := formatTransferResult(result, SelectSymbols(false))
		if got := lineText(lines[1]); !strings.Contains(got, destination) {
			t.Errorf("destination line = %q, want %q", got, destination)
		}
	}
}

func TestTransferResultExplainsLocalOutcomeWithoutSourceOrUnexpectedWording(t *testing.T) {
	tests := []struct {
		code     engine.FailureCode
		outcomes transfer.FileOutcomeSummary
		want     string
	}{
		{engine.FailureCheckpointRevisionConflict, transfer.FileOutcomeSummary{ItemBlockedFiles: 1, RevisionConflictFiles: 1}, "another source revision"},
		{engine.FailureCheckpointInvalid, transfer.FileOutcomeSummary{ItemBlockedFiles: 1, CheckpointInvalidFiles: 1}, "invalid checkpoint binding"},
		{engine.FailureOwnedObjectUnknown, transfer.FileOutcomeSummary{ItemBlockedFiles: 1, OwnedObjectUnknownFiles: 1}, "destination object it owns"},
		{engine.FailureDestinationCollision, transfer.FileOutcomeSummary{CollisionFiles: 1}, "Existing destinations prevented"},
	}
	for _, test := range tests {
		failure := engine.Failure{Code: test.code}
		result := TransferResult{
			Status: ResultPartial, Destination: "out", Files: test.outcomes,
			CountersExact: true, Failure: &failure,
		}
		var output strings.Builder
		for _, line := range formatTransferResult(result, SelectSymbols(false)) {
			output.WriteString(lineText(line))
		}
		text := output.String()
		if !strings.Contains(text, test.want) || strings.Contains(text, "unexpected") ||
			strings.Contains(text, "source content changed") || strings.Contains(text, "needs attention") {
			t.Fatalf("local outcome output = %q", text)
		}
	}
}

func TestDisplayValuesAreEscapedBeforeLayout(t *testing.T) {
	t.Parallel()
	subject := SharingSubject{Kind: SharingFile, Name: "bad\n\x1b\u202efile", FileBytes: 1}
	text := lineText(formatSharingSubject(subject, SelectSymbols(false)))
	if strings.ContainsAny(text, "\n\r\x1b") || !strings.Contains(text, `bad\n\x1b\u202efile`) {
		t.Fatalf("escaped subject = %q", text)
	}
}

func TestTransferResultFormatsNameAdjustmentsOnlyWhenPresent(t *testing.T) {
	t.Parallel()
	render := func(res TransferResult) string {
		var out strings.Builder
		for _, line := range formatTransferResult(res, SelectSymbols(false)) {
			out.WriteString(lineText(line))
			out.WriteByte('\n')
		}
		return out.String()
	}

	plain := render(TransferResult{Status: ResultSuccess, Destination: "out", CountersExact: true})
	if strings.Contains(plain, "compatible name") {
		t.Fatalf("zero adjustments emitted notice: %q", plain)
	}

	single := render(TransferResult{
		Status: ResultSuccess, Destination: "out", CountersExact: false,
		NameAdjustments: engine.NameAdjustmentSummary{
			Count: 1,
			Samples: []engine.NameAdjustment{
				{LogicalPath: "sub/CON\x1b[31m.txt", PhysicalPath: "sub/entry-CON.windshare-01.txt"},
			},
		},
	})
	for _, want := range []string{
		"1 item saved under a compatible name:",
		`sub/CON\x1b[31m.txt -> sub/entry-CON.windshare-01.txt`,
	} {
		if !strings.Contains(single, want) {
			t.Fatalf("single adjustment output %q missing %q", single, want)
		}
	}
	if strings.Contains(single, ">=1 item") || strings.Contains(single, "more;") {
		t.Fatalf("unexpected inexact or overflow wording: %q", single)
	}

	capped := render(TransferResult{
		Status: ResultSuccess, Destination: "out", CountersExact: true,
		NameAdjustments: engine.NameAdjustmentSummary{
			Count: 25,
			Samples: []engine.NameAdjustment{
				{LogicalPath: "a:1.txt", PhysicalPath: "a-1.windshare-01.txt"},
				{LogicalPath: "a:2.txt", PhysicalPath: "a-2.windshare-02.txt"},
			},
		},
	})
	for _, want := range []string{
		"25 items saved under compatible names:",
		"a:1.txt -> a-1.windshare-01.txt",
		"23 more; --trace logs mappings applied during a run.",
	} {
		if !strings.Contains(capped, want) {
			t.Fatalf("capped adjustment output %q missing %q", capped, want)
		}
	}
}
