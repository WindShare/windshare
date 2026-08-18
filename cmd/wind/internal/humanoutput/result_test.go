package humanoutput

import (
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
)

func TestTransferResultVocabularyAndOutcomeAuthority(t *testing.T) {
	t.Parallel()
	failure := mustFailure(t, clievent.FailureSourceRevisionChanged)
	tests := []struct {
		name      string
		status    clievent.ResultStatus
		exit      clievent.ExitCode
		drift     clievent.DriftReason
		failure   clievent.Failure
		headline  string
		completed bool
	}{
		{"success", clievent.ResultSuccess, clievent.ExitSuccess, clievent.DriftNone, clievent.Failure{}, "Download completed", true},
		{"partial", clievent.ResultPartial, clievent.ExitFailure, clievent.DriftNone, mustFailure(t, clievent.FailureOutputStateIO), "finished partially", false},
		{"paused", clievent.ResultPaused, clievent.ExitFailure, clievent.DriftNone, mustFailure(t, clievent.FailureCheckpointBusy), "Download paused", false},
		{"failed", clievent.ResultFailed, clievent.ExitNetwork, clievent.DriftNone, mustFailure(t, clievent.FailureRelayTransport), "Download failed", false},
		{"paused drift", clievent.ResultPaused, clievent.ExitDrift, clievent.DriftSource, failure, "Download paused", false},
		{"drift", clievent.ResultFailed, clievent.ExitDrift, clievent.DriftSource, failure, "source changed", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := clievent.NewTransferResult(clievent.TransferResultSpec{
				Status: test.status, ExitCode: test.exit, Drift: test.drift,
				Elapsed:     9*time.Second + 600*time.Millisecond,
				Destination: clievent.NewDisplayPath("out\n\x1b[31m"), DestinationAdjusted: true,
				Files: clievent.FileOutcomes{
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
	clean, err := clievent.NewShareResult(clievent.ShareResultSpec{ExitCode: clievent.ExitSuccess, Elapsed: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	text := lineText(formatShareResult(clean, SelectSymbols(true)))
	if text != "✔ Sharing stopped" || strings.ContainsAny(text, "0123456789") {
		t.Fatalf("clean share result = %q", text)
	}

	failed, err := clievent.NewShareResult(clievent.ShareResultSpec{
		ExitCode: clievent.ExitNetwork, Failure: mustFailure(t, clievent.FailureRelayTransport),
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
			Destination: clievent.NewDisplayPath(destination), CountersExact: true,
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

func TestDisplayValuesAreEscapedBeforeLayout(t *testing.T) {
	t.Parallel()
	subject, err := clievent.NewFileSubject(clievent.NewDisplayName("bad\n\x1b\u202efile"), 1)
	if err != nil {
		t.Fatal(err)
	}
	text := lineText(formatSharingSubject(subject, SelectSymbols(false)))
	if strings.ContainsAny(text, "\n\r\x1b") || !strings.Contains(text, `bad\n\x1b\u202efile`) {
		t.Fatalf("escaped subject = %q", text)
	}
}
