package humanoutput

import (
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
)

func TestCapacityWaitOverridesOrdinaryProgressOnlyWhileVisible(t *testing.T) {
	spec := clievent.ProgressSpec{
		DiscoveredFiles: 2, DiscoveredBytes: 100,
		PublishedFiles: 1, PublishedBytes: 20,
		VerifiedBytes: 40, NewlyVerifiedBytes: 40,
		FileOutcomes:          clievent.FileOutcomes{DownloadedFiles: 1},
		CapacityActiveWaiters: 1, CapacityAccumulatedWait: time.Second, CapacityWaitAttempts: 2,
		Discovery: clievent.DiscoveryComplete, CountersExact: true,
	}
	metrics := ProgressMetrics{RateBytesPerSecond: 10, RateValid: true, RateStable: true}

	hidden := mustSnapshot(t, spec)
	hiddenText := lineText(FormatProgress(hidden, metrics, ProgressLayout{}))
	if !strings.Contains(hiddenText, "40%") || strings.Contains(hiddenText, "Waiting for sender capacity") {
		t.Fatalf("hidden capacity wait changed ordinary progress: %q", hiddenText)
	}

	spec.CapacityWaitVisible = true
	visible := mustSnapshot(t, spec)
	visibleText := lineText(FormatProgress(visible, metrics, ProgressLayout{}))
	if visibleText != "Waiting for sender capacity" || strings.Contains(visibleText, "%") ||
		strings.Contains(visibleText, "/s") || strings.Contains(visibleText, "left") {
		t.Fatalf("visible capacity wait was not concise: %q", visibleText)
	}

	spec.CapacityActiveWaiters = 0
	spec.CapacityWaitVisible = false
	spec.VerifiedBytes = 50
	cleared := mustSnapshot(t, spec)
	clearedText := lineText(FormatProgress(cleared, metrics, ProgressLayout{}))
	if !strings.Contains(clearedText, "50%") || strings.Contains(clearedText, "Waiting for sender capacity") {
		t.Fatalf("cleared capacity wait did not restore ordinary progress: %q", clearedText)
	}
}

func TestProgressTruthEligibility(t *testing.T) {
	t.Parallel()
	open := mustSnapshot(t, clievent.ProgressSpec{
		DiscoveredFiles: 14, DiscoveredBytes: 8_200_000,
		VerifiedBytes: 2_000_000, NewlyVerifiedBytes: 2_000_000,
		Discovery: clievent.DiscoveryOpen, CountersExact: true,
	})
	text := lineText(FormatProgress(open, ProgressMetrics{
		RateBytesPerSecond: 2_400_000, RateValid: true, RateStable: true,
	}, ProgressLayout{Unicode: true}))
	for _, want := range []string{"Discovering", "14 files", "8.2 MB ready", "2.4 MB/s"} {
		if !strings.Contains(text, want) {
			t.Errorf("open progress %q missing %q", text, want)
		}
	}
	if strings.Contains(text, "%") || strings.Contains(text, "left") {
		t.Errorf("open discovery exposed deterministic progress: %q", text)
	}

	complete := mustSnapshot(t, clievent.ProgressSpec{
		DiscoveredFiles: 34, DiscoveredBytes: 142_500_000,
		PublishedFiles: 27, PublishedBytes: 111_200_000,
		VerifiedBytes: 111_200_000, NewlyVerifiedBytes: 30_000_000,
		FileOutcomes: clievent.FileOutcomes{DownloadedFiles: 27},
		Discovery:    clievent.DiscoveryComplete, CountersExact: true,
	})
	text = lineText(FormatProgress(complete, ProgressMetrics{
		RateBytesPerSecond: 14_800_000, RateValid: true, RateStable: true,
	}, ProgressLayout{}))
	for _, want := range []string{"78%", "27/34 files", "111.2 MB/142.5 MB", "14.8 MB/s", "3s left"} {
		if !strings.Contains(text, want) {
			t.Errorf("complete progress %q missing %q", text, want)
		}
	}

	blocked := mustSnapshot(t, clievent.ProgressSpec{
		DiscoveredFiles: 34, DiscoveredBytes: 142_500_000,
		PublishedFiles: 27, PublishedBytes: 111_200_000,
		VerifiedBytes: 111_200_000, NewlyVerifiedBytes: 30_000_000,
		FileOutcomes: clievent.FileOutcomes{DownloadedFiles: 27, ItemBlockedFiles: 1},
		Discovery:    clievent.DiscoveryComplete, CountersExact: true,
	})
	text = lineText(FormatProgress(blocked, ProgressMetrics{
		RateBytesPerSecond: 14_800_000, RateValid: true, RateStable: true,
	}, ProgressLayout{}))
	if !strings.Contains(text, "14.8 MB/s") || strings.Contains(text, "left") {
		t.Errorf("non-success outcome ETA eligibility is wrong: %q", text)
	}
}

func TestProgressFinalizingAndInexactSemantics(t *testing.T) {
	t.Parallel()
	finalizing := mustSnapshot(t, clievent.ProgressSpec{
		DiscoveredFiles: 2, DiscoveredBytes: 10_000,
		PublishedFiles: 1, PublishedBytes: 6_000,
		VerifiedBytes: 10_000, NewlyVerifiedBytes: 10_000,
		FileOutcomes: clievent.FileOutcomes{DownloadedFiles: 1},
		Discovery:    clievent.DiscoveryComplete, CountersExact: true,
	})
	text := lineText(FormatProgress(finalizing, ProgressMetrics{
		RateBytesPerSecond: 1_000, RateValid: true, RateStable: true,
	}, ProgressLayout{}))
	if !strings.Contains(text, "Finalizing") || strings.Contains(text, "/s") || strings.Contains(text, "left") {
		t.Fatalf("finalizing progress = %q", text)
	}

	inexact := mustSnapshot(t, clievent.ProgressSpec{
		DiscoveredFiles: 5, DiscoveredBytes: 10_000, PublishedFiles: 2,
		PublishedBytes: 4_000, VerifiedBytes: 4_000, NewlyVerifiedBytes: 4_000,
		Discovery: clievent.DiscoveryComplete, CountersExact: false,
	})
	text = lineText(FormatProgress(inexact, ProgressMetrics{
		RateBytesPerSecond: 1_000, RateValid: true, RateStable: true,
	}, ProgressLayout{Unicode: false}))
	if strings.Contains(text, "%") || strings.Contains(text, "left") || !strings.Contains(text, ">=") {
		t.Fatalf("inexact progress = %q", text)
	}
	if strings.Contains(text, "/s") {
		t.Fatalf("inexact progress exposed a rate: %q", text)
	}
	if strings.Contains(text, "Finalizing") {
		t.Fatalf("inexact progress entered finalization: %q", text)
	}
}

func TestExactCompleteZeroByteProgressUsesFileOutcomes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                 string
		files                uint64
		published            uint64
		outcomes             clievent.FileOutcomes
		successfulSettlement bool
		wantPhase            string
		wantPrimary          string
	}{
		{
			name:  "one of two terminal outcomes",
			files: 2, published: 1,
			outcomes:  clievent.FileOutcomes{DownloadedFiles: 1},
			wantPhase: "50%", wantPrimary: "1/2 files settled",
		},
		{
			name:  "all outcomes before settlement",
			files: 2, published: 2,
			outcomes:  clievent.FileOutcomes{DownloadedFiles: 2},
			wantPhase: "99%", wantPrimary: "2/2 files settled",
		},
		{
			name:  "all outcomes after successful settlement",
			files: 2, published: 2,
			outcomes: clievent.FileOutcomes{DownloadedFiles: 2}, successfulSettlement: true,
			wantPhase: "100%", wantPrimary: "2/2 files settled",
		},
		{
			name:  "non-success outcome cannot close progress",
			files: 2, published: 1,
			outcomes: clievent.FileOutcomes{DownloadedFiles: 1, FailedFiles: 1}, successfulSettlement: true,
			wantPhase: "99%", wantPrimary: "2/2 files settled",
		},
		{
			name:      "empty selection before settlement",
			wantPhase: "0%", wantPrimary: "0/0 files settled",
		},
		{
			name:                 "empty selection after successful settlement",
			successfulSettlement: true, wantPhase: "100%", wantPrimary: "0/0 files settled",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := mustSnapshot(t, clievent.ProgressSpec{
				DiscoveredFiles: test.files,
				PublishedFiles:  test.published,
				FileOutcomes:    test.outcomes,
				Discovery:       clievent.DiscoveryComplete,
				CountersExact:   true,
			})
			components := progressView(snapshot, ProgressMetrics{SuccessfulSettlement: test.successfulSettlement}, false)
			if components.phase != test.wantPhase || components.primary != test.wantPrimary {
				t.Fatalf("progress = %q | %q, want %q | %q", components.phase, components.primary, test.wantPhase, test.wantPrimary)
			}
			if strings.Contains(components.phase, "Finalizing") {
				t.Fatalf("zero-byte progress entered byte finalization: %+v", components)
			}
		})
	}
}

func TestProgressWidthDegradesInSemanticOrder(t *testing.T) {
	t.Parallel()
	snapshot := mustSnapshot(t, clievent.ProgressSpec{
		DiscoveredFiles: 100, DiscoveredBytes: 100_000_000,
		PublishedFiles: 50, PublishedBytes: 50_000_000,
		VerifiedBytes: 50_000_000, NewlyVerifiedBytes: 50_000_000,
		FileOutcomes: clievent.FileOutcomes{DownloadedFiles: 50},
		Discovery:    clievent.DiscoveryComplete, CountersExact: true,
	})
	metrics := ProgressMetrics{RateBytesPerSecond: 10_000_000, RateValid: true, RateStable: true}
	components := progressView(snapshot, metrics, false)

	withoutETA := components
	withoutETA.eta = ""
	line := FormatProgress(snapshot, metrics, ProgressLayout{Columns: composeProgressLine(withoutETA).DisplayCells(nil)})
	text := lineText(line)
	if strings.Contains(text, "left") || !strings.Contains(text, "/s") || !strings.Contains(text, "[") {
		t.Fatalf("ETA degradation = %q", text)
	}

	withoutRate := withoutETA
	withoutRate.rate = ""
	line = FormatProgress(snapshot, metrics, ProgressLayout{Columns: composeProgressLine(withoutRate).DisplayCells(nil)})
	text = lineText(line)
	if strings.Contains(text, "/s") || !strings.Contains(text, "[") || !strings.Contains(text, "50/100 files") {
		t.Fatalf("rate degradation = %q", text)
	}

	withoutBar := withoutRate
	withoutBar.bar = ""
	line = FormatProgress(snapshot, metrics, ProgressLayout{Columns: composeProgressLine(withoutBar).DisplayCells(nil)})
	text = lineText(line)
	if strings.Contains(text, "[") || !strings.Contains(text, "50/100 files") {
		t.Fatalf("bar degradation = %q", text)
	}

	minimum := withoutBar
	minimum.secondary = ""
	line = FormatProgress(snapshot, metrics, ProgressLayout{Columns: composeProgressLine(minimum).DisplayCells(nil)})
	text = lineText(line)
	if strings.Contains(text, "files") || !strings.Contains(text, "50%") || !strings.Contains(text, "50.0 MB/100.0 MB") {
		t.Fatalf("secondary degradation = %q", text)
	}
}

func TestProgressSymbolsFallbackToASCII(t *testing.T) {
	t.Parallel()
	snapshot := mustSnapshot(t, clievent.ProgressSpec{Discovery: clievent.DiscoveryOpen, CountersExact: true})
	ascii := lineText(FormatProgress(snapshot, ProgressMetrics{}, ProgressLayout{Unicode: false}))
	unicode := lineText(FormatProgress(snapshot, ProgressMetrics{}, ProgressLayout{Unicode: true}))
	if !strings.HasPrefix(ascii, "? ") || !strings.HasPrefix(unicode, "🔍 ") {
		t.Fatalf("symbol fallback: ASCII %q, Unicode %q", ascii, unicode)
	}
	if strings.Contains(ascii, "…") || !strings.Contains(ascii, "...") {
		t.Fatalf("ASCII ellipsis fallback = %q", ascii)
	}
}

func TestProgressWidthUsesInjectedDisplayCells(t *testing.T) {
	t.Parallel()
	snapshot := mustSnapshot(t, clievent.ProgressSpec{
		DiscoveredFiles: 2, DiscoveredBytes: 10_000,
		PublishedFiles: 1, PublishedBytes: 5_000,
		VerifiedBytes: 5_000, NewlyVerifiedBytes: 5_000,
		FileOutcomes: clievent.FileOutcomes{DownloadedFiles: 1},
		Discovery:    clievent.DiscoveryComplete, CountersExact: true,
	})
	metrics := ProgressMetrics{RateBytesPerSecond: 1_000, RateValid: true, RateStable: true}
	line := FormatProgress(snapshot, metrics, ProgressLayout{
		Columns:   70,
		CellWidth: func(text string) int { return len(text) * 2 },
	})
	if text := lineText(line); strings.Contains(text, "left") || strings.Contains(text, "/s") {
		t.Fatalf("injected-width degradation = %q", text)
	}
}
