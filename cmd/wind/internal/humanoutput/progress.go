package humanoutput

import (
	"math/bits"
	"strings"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
)

const progressBarSegments = 10

type ProgressMetrics struct {
	RateBytesPerSecond   uint64
	RateValid            bool
	RateStable           bool
	SuccessfulSettlement bool
}

type ProgressLayout struct {
	Columns   int
	Unicode   bool
	CellWidth terminalcanvas.CellWidthFunc
}

type progressComponents struct {
	phase, primary, secondary string
	bar, rate, eta            string
}

// FormatProgress is pure: rate eligibility is supplied as a fact, while this
// function owns only progress truth rules and width degradation.
func FormatProgress(
	snapshot clievent.ProgressSnapshot,
	metrics ProgressMetrics,
	layout ProgressLayout,
) terminalcanvas.Line {
	components := progressView(snapshot, metrics, layout.Unicode)
	line := composeProgressLine(components)
	if layout.Columns <= 0 {
		return line
	}

	// Optional information disappears in a fixed semantic order. Canvas clipping
	// remains a final defense for terminals narrower than phase + primary progress.
	for _, remove := range []func(*progressComponents){
		func(value *progressComponents) { value.eta = "" },
		func(value *progressComponents) { value.rate = "" },
		func(value *progressComponents) { value.bar = "" },
		func(value *progressComponents) { value.secondary = "" },
	} {
		if line.DisplayCells(layout.CellWidth) <= layout.Columns {
			break
		}
		remove(&components)
		line = composeProgressLine(components)
	}
	return line
}

func progressView(snapshot clievent.ProgressSnapshot, metrics ProgressMetrics, unicode bool) progressComponents {
	symbols := SelectSymbols(unicode)
	exactPrefix := ""
	if !snapshot.CountersExact() {
		if unicode {
			exactPrefix = "≥"
		} else {
			exactPrefix = ">="
		}
	}
	rate := ""
	if metrics.RateValid && metrics.RateBytesPerSecond != 0 && snapshot.CountersExact() &&
		snapshot.Discovery() != clievent.DiscoveryFailed {
		rate = FormatRate(metrics.RateBytesPerSecond)
	}

	switch snapshot.Discovery() {
	case clievent.DiscoveryOpen:
		return progressComponents{
			phase:     symbols.Discovery + " Discovering" + symbols.Ellipsis,
			primary:   exactPrefix + FormatBytes(snapshot.DiscoveredBytes()) + " ready",
			secondary: exactPrefix + formatFiles(snapshot.DiscoveredFiles()),
			rate:      rate,
		}
	case clievent.DiscoveryFailed:
		return progressComponents{
			phase:     symbols.Failure + " Discovery failed",
			primary:   exactPrefix + FormatBytes(snapshot.VerifiedBytes()) + " verified",
			secondary: exactPrefix + formatFiles(snapshot.PublishedFiles()) + " published",
		}
	}

	if !snapshot.CountersExact() {
		return progressComponents{
			phase:     "Transferring" + symbols.Ellipsis,
			primary:   exactPrefix + FormatBytes(snapshot.VerifiedBytes()) + " verified",
			secondary: exactPrefix + formatFiles(snapshot.PublishedFiles()) + " published",
			rate:      rate,
		}
	}

	if snapshot.DiscoveredBytes() == 0 {
		settledFiles := terminalFileOutcomes(snapshot.FileOutcomes())
		percent := zeroBytePercentage(
			settledFiles,
			snapshot.DiscoveredFiles(),
			snapshot.FileOutcomes(),
			metrics.SuccessfulSettlement,
		)
		return progressComponents{
			phase:   formatCount(percent) + "%",
			primary: formatCount(settledFiles) + "/" + formatFiles(snapshot.DiscoveredFiles()) + " settled",
			bar:     formatProgressBar(percent, unicode),
		}
	}

	if snapshot.VerifiedBytes() == snapshot.DiscoveredBytes() &&
		(snapshot.PublishedBytes() < snapshot.DiscoveredBytes() || snapshot.PublishedFiles() < snapshot.DiscoveredFiles()) {
		return progressComponents{
			phase:     "Finalizing" + symbols.Ellipsis,
			primary:   FormatBytes(snapshot.PublishedBytes()) + "/" + FormatBytes(snapshot.DiscoveredBytes()) + " published",
			secondary: formatCount(snapshot.PublishedFiles()) + "/" + formatFiles(snapshot.DiscoveredFiles()),
		}
	}

	percent := percentage(snapshot.VerifiedBytes(), snapshot.DiscoveredBytes())
	components := progressComponents{
		phase:     formatCount(percent) + "%",
		primary:   FormatBytes(snapshot.VerifiedBytes()) + "/" + FormatBytes(snapshot.DiscoveredBytes()),
		secondary: formatCount(snapshot.PublishedFiles()) + "/" + formatFiles(snapshot.DiscoveredFiles()),
		bar:       formatProgressBar(percent, unicode),
		rate:      rate,
	}
	remaining := snapshot.DiscoveredBytes() - snapshot.VerifiedBytes()
	if remaining != 0 && metrics.RateValid && metrics.RateStable &&
		metrics.RateBytesPerSecond != 0 && !snapshot.FileOutcomes().HasNonSuccess() {
		seconds := remaining / metrics.RateBytesPerSecond
		if remaining%metrics.RateBytesPerSecond != 0 {
			seconds++
		}
		components.eta = FormatETA(seconds)
	}
	return components
}

func composeProgressLine(components progressComponents) terminalcanvas.Line {
	values := make([]string, 0, 6)
	for _, value := range []string{
		components.phase, components.bar, components.secondary,
		components.primary, components.rate, components.eta,
	} {
		if value != "" {
			values = append(values, value)
		}
	}
	spans := make([]terminalcanvas.Span, 0, len(values)*2)
	for index, value := range values {
		if index != 0 {
			spans = append(spans, terminalcanvas.Span{Text: " | ", Style: terminalcanvas.StyleMuted})
		}
		style := terminalcanvas.StyleDefault
		if index == 0 {
			style = terminalcanvas.StyleStrong
		}
		spans = append(spans, terminalcanvas.Span{Text: value, Style: style})
	}
	return terminalcanvas.NewLine(spans...)
}

func percentage(completed, total uint64) uint64 {
	if total == 0 {
		return 0
	}
	if completed >= total {
		return 100
	}
	high, low := bits.Mul64(completed, 100)
	value, _ := bits.Div64(high, low, total)
	return value
}

func zeroBytePercentage(
	settledFiles, discoveredFiles uint64,
	outcomes clievent.FileOutcomes,
	successfulSettlement bool,
) uint64 {
	if discoveredFiles == 0 {
		// No file outcome can prove completion for an empty set, so only the
		// authoritative transfer settlement may close its progress.
		if successfulSettlement && !outcomes.HasNonSuccess() {
			return 100
		}
		return 0
	}

	percent := percentage(settledFiles, discoveredFiles)
	if percent == 100 && (!successfulSettlement || outcomes.HasNonSuccess()) {
		return 99
	}
	return percent
}

func formatProgressBar(percent uint64, unicode bool) string {
	filled := int(percent * progressBarSegments / 100)
	filled = min(filled, progressBarSegments)
	fill, empty := "#", "-"
	if unicode {
		fill, empty = "█", "░"
	}
	return "[" + strings.Repeat(fill, filled) + strings.Repeat(empty, progressBarSegments-filled) + "]"
}

func terminalFileOutcomes(outcomes clievent.FileOutcomes) uint64 {
	total := outcomes.DownloadedFiles
	for _, value := range []uint64{
		outcomes.ResumedFiles, outcomes.PausedFiles, outcomes.CollisionFiles,
		outcomes.ItemBlockedFiles, outcomes.FailedFiles,
	} {
		if ^uint64(0)-total < value {
			return ^uint64(0)
		}
		total += value
	}
	return total
}

func formatFiles(count uint64) string {
	label := "files"
	if count == 1 {
		label = "file"
	}
	return formatCount(count) + " " + label
}
