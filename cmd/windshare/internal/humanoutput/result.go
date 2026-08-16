package humanoutput

import (
	"strings"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
	"github.com/windshare/windshare/cmd/windshare/internal/terminalcanvas"
)

func formatTransferResult(result clievent.TransferResult, symbols Symbols) []terminalcanvas.Line {
	elapsed := FormatElapsed(result.Elapsed())
	var headline terminalcanvas.Line
	switch result.Status() {
	case clievent.ResultSuccess:
		headline = statusLine(symbols.Success, "Download completed in "+elapsed, terminalcanvas.StyleSuccess)
	case clievent.ResultPartial:
		headline = statusLine(symbols.Warning, withDrift("Download finished partially in "+elapsed, result.Drift()), terminalcanvas.StyleWarning)
	case clievent.ResultPaused:
		headline = statusLine(symbols.Paused, withDrift("Download paused after "+elapsed, result.Drift()), terminalcanvas.StyleWarning)
	default:
		headline = statusLine(symbols.Failure, withDrift("Download failed after "+elapsed, result.Drift()), terminalcanvas.StyleError)
	}

	destination := "  Destination: " + escapedDisplay(result.Destination().Text())
	if result.DestinationAdjusted() {
		destination += " (adjusted to avoid an existing item)"
	}
	lines := []terminalcanvas.Line{
		headline,
		terminalcanvas.NewLine(terminalcanvas.Span{Text: destination}),
		terminalcanvas.NewLine(terminalcanvas.Span{Text: "  " + formatOutcomeSummary(result, symbols.Separator)}),
	}
	if failure, ok := result.Failure(); ok {
		lines = append(lines, terminalcanvas.NewLine(
			terminalcanvas.Span{Text: "  Reason: ", Style: terminalcanvas.StyleMuted},
			terminalcanvas.Span{Text: failureMessage(failure)},
		))
	}
	return lines
}

func withDrift(headline string, drift clievent.DriftReason) string {
	if drift == clievent.DriftSource {
		return headline + " because the source changed"
	}
	return headline
}

func formatOutcomeSummary(result clievent.TransferResult, separator string) string {
	outcomes := result.Files()
	parts := make([]string, 0, 10)
	if outcomes.DownloadedFiles != 0 || terminalFileOutcomes(outcomes) == 0 {
		parts = append(parts, resultCount(result, outcomes.DownloadedFiles)+" downloaded")
	}
	if outcomes.ResumedFiles != 0 {
		parts = append(parts, resultCount(result, outcomes.ResumedFiles)+" resumed")
	}
	if outcomes.PausedFiles != 0 {
		parts = append(parts, resultCount(result, outcomes.PausedFiles)+" paused")
	}
	if outcomes.CollisionFiles != 0 {
		parts = append(parts, countedResultNoun(result, outcomes.CollisionFiles, "collision", "collisions"))
	}
	if outcomes.ItemBlockedFiles != 0 {
		parts = append(parts, resultCount(result, outcomes.ItemBlockedFiles)+" item-blocked")
	}
	if outcomes.FailedFiles != 0 {
		parts = append(parts, resultCount(result, outcomes.FailedFiles)+" failed")
	}
	if outcomes.ModifiedTimeWarnings != 0 {
		parts = append(parts, countedResultNoun(result, outcomes.ModifiedTimeWarnings, "modified-time warning", "modified-time warnings"))
	}
	if result.DirectoryFailures() != 0 {
		parts = append(parts, countedResultNoun(result, result.DirectoryFailures(), "directory failed", "directories failed"))
	}
	if result.OmittedDiagnostics() != 0 {
		parts = append(parts, countedResultNoun(result, result.OmittedDiagnostics(), "diagnostic omitted", "diagnostics omitted"))
	}
	bytePrefix := ""
	if !result.CountersExact() {
		bytePrefix = ">="
	}
	parts = append(parts, bytePrefix+FormatBytes(result.PublishedBytes()))
	return strings.Join(parts, separator)
}

func resultCount(result clievent.TransferResult, count uint64) string {
	prefix := ""
	if !result.CountersExact() {
		prefix = ">="
	}
	return prefix + formatCount(count)
}

func countedResultNoun(result clievent.TransferResult, count uint64, singular, plural string) string {
	label := plural
	if count == 1 {
		label = singular
	}
	return resultCount(result, count) + " " + label
}

func formatShareResult(result clievent.ShareResult, symbols Symbols) terminalcanvas.Line {
	if result.StoppedCleanly() {
		// No sender traffic total exists with stable multi-receiver semantics yet.
		return statusLine(symbols.Success, "Sharing stopped", terminalcanvas.StyleSuccess)
	}
	failure, _ := result.Failure()
	return failureLine("Sharing failed", failure, symbols.Failure, terminalcanvas.StyleError)
}
