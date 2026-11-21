package humanoutput

import (
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	appobs "github.com/windshare/windshare/engine/observation"
)

func TestTransferResultVocabularyAndOutcomeAuthority(t *testing.T) {
	t.Parallel()
	failure := mustFailure(t, appobs.FailureSourceRevisionChanged)
	tests := []struct {
		name      string
		status    clievent.ResultStatus
		exit      clievent.ExitCode
		drift     clievent.DriftReason
		failure   appobs.Failure
		headline  string
		completed bool
	}{
		{"success", clievent.ResultSuccess, clievent.ExitSuccess, clievent.DriftNone, appobs.Failure{}, "Download completed", true},
		{"partial", clievent.ResultPartial, clievent.ExitFailure, clievent.DriftNone, mustFailure(t, appobs.FailureOutputStateIO), "finished partially", false},
		{"paused", clievent.ResultPaused, clievent.ExitFailure, clievent.DriftNone, mustFailure(t, appobs.FailureCheckpointBusy), "Download paused", false},
		{"failed", clievent.ResultFailed, clievent.ExitNetwork, clievent.DriftNone, mustFailure(t, appobs.FailureRelayTransport), "Download failed", false},
		{"paused drift", clievent.ResultPaused, clievent.ExitDrift, clievent.DriftSource, failure, "Download paused", false},
		{"drift", clievent.ResultFailed, clievent.ExitDrift, clievent.DriftSource, failure, "source changed", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := clievent.NewTransferResult(clievent.TransferResultSpec{
				Status: test.status, ExitCode: test.exit, Drift: test.drift,
				Elapsed:     9*time.Second + 600*time.Millisecond,
				Destination: appobs.NewDisplayPath("out\n\x1b[31m"), DestinationAdjusted: true,
				Files: appobs.FileOutcomes{
					DownloadedFiles: 3, ResumedFiles: 2, PausedFiles: 1,
					CollisionFiles: 1, ItemBlockedFiles: 2, FailedFiles: 1,
					ModifiedTimeWarnings: 1,
				},
				DirectoryFailures: 1, OmittedDiagnostics: 2,
				PublishedBytes: 5_000_000, CountersExact: false, Failure: test.failure,
			})
			if err != nil {
				t.Fatal(err)
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
			if test.drift == clievent.DriftSource && !strings.Contains(output, "source changed") {
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
}

func TestShareResultNeverInfersTrafficTotals(t *testing.T) {
	t.Parallel()
	clean, err := clievent.NewShareResult(clievent.ShareResultSpec{Publication: clievent.SharePublished, ExitCode: clievent.ExitSuccess, Elapsed: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	text := lineText(formatShareResult(clean, SelectSymbols(true)))
	if text != "✔ Sharing stopped" || strings.ContainsAny(text, "0123456789") {
		t.Fatalf("clean share result = %q", text)
	}

	failed, err := clievent.NewShareResult(clievent.ShareResultSpec{Publication: clievent.SharePublished,
		ExitCode: clievent.ExitNetwork, Failure: mustFailure(t, appobs.FailureRelayTransport),
	})
	if err != nil {
		t.Fatal(err)
	}
	if text := lineText(formatShareResult(failed, SelectSymbols(false))); !strings.Contains(text, "Sharing failed") {
		t.Fatalf("failed share result = %q", text)
	}
}

func TestTransferResultPreservesPublishedDestinationKind(t *testing.T) {
	t.Parallel()
	for _, destination := range []string{"./archive.zip", "./vacation_photos/"} {
		result, err := clievent.NewTransferResult(clievent.TransferResultSpec{
			Status: clievent.ResultSuccess, ExitCode: clievent.ExitSuccess,
			Destination: appobs.NewDisplayPath(destination), CountersExact: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		lines := formatTransferResult(result, SelectSymbols(false))
		if got := lineText(lines[1]); !strings.Contains(got, destination) {
			t.Errorf("destination line = %q, want %q", got, destination)
		}
	}
}

func TestTransferResultExplainsLocalOutcomeWithoutSourceOrUnexpectedWording(t *testing.T) {
	tests := []struct {
		code     appobs.FailureCode
		outcomes appobs.FileOutcomes
		want     string
	}{
		{appobs.FailureCheckpointRevisionConflict, appobs.FileOutcomes{ItemBlockedFiles: 1, RevisionConflictFiles: 1}, "another source revision"},
		{appobs.FailureCheckpointInvalid, appobs.FileOutcomes{ItemBlockedFiles: 1, CheckpointInvalidFiles: 1}, "invalid checkpoint binding"},
		{appobs.FailureOwnedObjectUnknown, appobs.FileOutcomes{ItemBlockedFiles: 1, OwnedObjectUnknownFiles: 1}, "destination object it owns"},
		{appobs.FailureDestinationCollision, appobs.FileOutcomes{CollisionFiles: 1}, "Existing destinations prevented"},
	}
	for _, test := range tests {
		failure := mustFailure(t, test.code)
		result, err := clievent.NewTransferResult(clievent.TransferResultSpec{
			Status: clievent.ResultPartial, ExitCode: clievent.ExitFailure,
			Destination: appobs.NewDisplayPath("out"), Files: test.outcomes,
			CountersExact: true, Failure: failure,
		})
		if err != nil {
			t.Fatal(err)
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
	subject, err := appobs.NewFileSubject(appobs.NewDisplayName("bad\n\x1b\u202efile"), 1)
	if err != nil {
		t.Fatal(err)
	}
	text := lineText(formatSharingSubject(subject, SelectSymbols(false)))
	if strings.ContainsAny(text, "\n\r\x1b") || !strings.Contains(text, `bad\n\x1b\u202efile`) {
		t.Fatalf("escaped subject = %q", text)
	}
}

func TestTransferResultReportsCompatibleNameRepairsWhenPresent(t *testing.T) {
	t.Parallel()
	plain, err := clievent.NewTransferResult(clievent.TransferResultSpec{
		Status: clievent.ResultSuccess, ExitCode: clievent.ExitSuccess,
		Destination: appobs.NewDisplayPath("./docs"), CountersExact: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range formatTransferResult(plain, SelectSymbols(false)) {
		if strings.Contains(lineText(line), "compatible name") {
			t.Fatalf("unexpected compatible-name notice: %q", lineText(line))
		}
	}

	for _, tc := range []struct {
		name          string
		status        clievent.ResultStatus
		exit          clievent.ExitCode
		failure       appobs.Failure
		countersExact bool
		count         uint64
		want          string
	}{
		{
			name:          "single exact",
			status:        clievent.ResultSuccess,
			exit:          clievent.ExitSuccess,
			countersExact: true,
			count:         1,
			want:          "  1 item saved under a compatible name (see windshare-names-abc-1-0.json beside destination).",
		},
		{
			name:          "multiple exact",
			status:        clievent.ResultSuccess,
			exit:          clievent.ExitSuccess,
			countersExact: true,
			count:         2,
			want:          "  2 items saved under compatible names (see windshare-names-abc-1-0.json beside destination).",
		},
		{
			name:          "inexact transfer counters keep exact repaired count",
			status:        clievent.ResultPartial,
			exit:          clievent.ExitFailure,
			failure:       mustFailure(t, appobs.FailureSourceOmitted),
			countersExact: false,
			count:         1,
			want:          "  1 item saved under a compatible name (see windshare-names-abc-1-0.json beside destination).",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repaired, err := clievent.NewTransferResult(clievent.TransferResultSpec{
				Status: tc.status, ExitCode: tc.exit,
				Destination: appobs.NewDisplayPath("./docs"), CountersExact: tc.countersExact,
				Failure: tc.failure,
				CompatibleNameReport: clievent.CompatibleNameReport{
					RepairedCount: tc.count, ReportName: appobs.NewDisplayName("windshare-names-abc-1-0.json"),
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			var output strings.Builder
			for _, line := range formatTransferResult(repaired, SelectSymbols(false)) {
				output.WriteString(lineText(line))
				output.WriteByte('\n')
			}
			rendered := output.String()
			if !strings.Contains(rendered, tc.want) {
				t.Fatalf("output %q missing %q", rendered, tc.want)
			}
			if !tc.countersExact && !strings.Contains(rendered, ">=0 B") {
				t.Fatalf("expected inexact transfer summary in %q", rendered)
			}
			if strings.Contains(rendered, ">=1 item saved") {
				t.Fatalf("compatible name count unexpectedly rendered as inexact: %q", rendered)
			}
		})
	}
}
