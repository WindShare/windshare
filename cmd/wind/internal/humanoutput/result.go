package humanoutput

import (
	"strings"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/engine"
)

type ResultStatus string

const (
	ResultSuccess ResultStatus = "success"
	ResultPartial ResultStatus = "partial"
	ResultPaused  ResultStatus = "paused"
	ResultStopped ResultStatus = "stopped"
	ResultFailed  ResultStatus = "failed"
)

type TransferResult struct {
	Status               ResultStatus
	SourceDrift          bool
	Elapsed              time.Duration
	Destination          string
	DestinationAdjusted  bool
	NameAdjustments      engine.NameAdjustmentSummary
	Files                transfer.FileOutcomeSummary
	SourceOmittedEntries uint64
	DirectoryFailures    uint64
	OmittedDiagnostics   uint64
	PublishedBytes       uint64
	CountersExact        bool
	Failure              *engine.Failure
}

type ShareResult struct {
	Published      bool
	StoppedCleanly bool
	Elapsed        time.Duration
	Failure        *engine.Failure
}

func formatTransferResult(result TransferResult, symbols Symbols) []terminalcanvas.Line {
	elapsed := FormatElapsed(result.Elapsed)
	var headline terminalcanvas.Line
	switch result.Status {
	case ResultSuccess:
		headline = statusLine(symbols.Success, "Download completed in "+elapsed, terminalcanvas.StyleSuccess)
	case ResultPartial:
		headline = statusLine(symbols.Warning, withDrift("Download finished partially in "+elapsed, result.SourceDrift), terminalcanvas.StyleWarning)
	case ResultPaused:
		headline = statusLine(symbols.Paused, withDrift("Download paused after "+elapsed, result.SourceDrift), terminalcanvas.StyleWarning)
	case ResultStopped:
		headline = statusLine(symbols.Warning, "Download stopped after "+elapsed, terminalcanvas.StyleWarning)
	default:
		headline = statusLine(symbols.Failure, withDrift("Download failed after "+elapsed, result.SourceDrift), terminalcanvas.StyleError)
	}

	destination := "  Destination: " + escapedDisplay(result.Destination)
	if result.DestinationAdjusted {
		destination += " (adjusted to avoid an existing item)"
	}
	lines := []terminalcanvas.Line{
		headline,
		terminalcanvas.NewLine(terminalcanvas.Span{Text: destination}),
		terminalcanvas.NewLine(terminalcanvas.Span{Text: "  " + formatOutcomeSummary(result, symbols.Separator)}),
	}
	if result.NameAdjustments.Count != 0 {
		lines = append(lines, formatNameAdjustments(result.NameAdjustments)...)
	}
	if result.SourceOmittedEntries != 0 {
		lines = append(lines, terminalcanvas.Plain("  A skipped directory counts as one item; all of its contents are excluded."))
	}
	if result.Failure != nil && result.Failure.Valid() {
		lines = append(lines, terminalcanvas.NewLine(
			terminalcanvas.Span{Text: "  Reason: ", Style: terminalcanvas.StyleMuted},
			terminalcanvas.Span{Text: failureMessage(*result.Failure)},
		))
	}
	return lines
}

func withDrift(headline string, sourceDrift bool) string {
	if sourceDrift {
		return headline + " because the source changed"
	}
	return headline
}

func formatOutcomeSummary(result TransferResult, separator string) string {
	outcomes := result.Files
	parts := make([]string, 0, 10)
	if outcomes.DownloadedFiles != 0 || terminalFileOutcomes(outcomes) == 0 {
		parts = append(parts, resultCount(result, outcomes.DownloadedFiles)+" downloaded")
	}
	if outcomes.PreviouslyPublishedFiles != 0 {
		parts = append(parts, resultCount(result, outcomes.PreviouslyPublishedFiles)+" already downloaded")
	}
	if outcomes.ResumedFiles != 0 {
		parts = append(parts, resultCount(result, outcomes.ResumedFiles)+" resumed")
	}
	if outcomes.PausedFiles != 0 {
		label := " paused"
		if result.Status == ResultStopped {
			label = " unfinished"
		}
		parts = append(parts, resultCount(result, outcomes.PausedFiles)+label)
	}
	if outcomes.CollisionFiles != 0 {
		parts = append(parts, countedResultNoun(result, outcomes.CollisionFiles, "collision", "collisions"))
	}
	if outcomes.RevisionConflictFiles != 0 {
		parts = append(parts, resultCount(result, outcomes.RevisionConflictFiles)+" revision-conflict blocked")
	}
	if outcomes.CheckpointInvalidFiles != 0 {
		parts = append(parts, resultCount(result, outcomes.CheckpointInvalidFiles)+" invalid-checkpoint blocked")
	}
	if outcomes.OwnedObjectUnknownFiles != 0 {
		parts = append(parts, resultCount(result, outcomes.OwnedObjectUnknownFiles)+" ownership-conflict blocked")
	}
	if residualItemBlockedFiles(outcomes) != 0 {
		parts = append(parts, resultCount(result, residualItemBlockedFiles(outcomes))+" item-blocked")
	}
	if outcomes.FailedFiles != 0 {
		parts = append(parts, resultCount(result, outcomes.FailedFiles)+" failed")
	}
	if outcomes.ModifiedTimeWarnings != 0 {
		parts = append(parts, countedResultNoun(result, outcomes.ModifiedTimeWarnings, "modified-time warning", "modified-time warnings"))
	}
	if result.DirectoryFailures != 0 {
		parts = append(parts, countedResultNoun(result, result.DirectoryFailures, "directory failed", "directories failed"))
	}
	if result.SourceOmittedEntries != 0 {
		parts = append(parts, countedResultNoun(result, result.SourceOmittedEntries, "source item not included", "source items not included"))
	}
	if result.OmittedDiagnostics != 0 {
		parts = append(parts, countedResultNoun(result, result.OmittedDiagnostics, "diagnostic omitted", "diagnostics omitted"))
	}
	bytePrefix := ""
	if !result.CountersExact {
		bytePrefix = ">="
	}
	parts = append(parts, bytePrefix+FormatBytes(result.PublishedBytes))
	return strings.Join(parts, separator)
}

func residualItemBlockedFiles(outcomes transfer.FileOutcomeSummary) uint64 {
	classified := outcomes.RevisionConflictFiles
	for _, count := range []uint64{outcomes.CheckpointInvalidFiles, outcomes.OwnedObjectUnknownFiles} {
		if count > ^uint64(0)-classified {
			return 0
		}
		classified += count
	}
	if classified >= outcomes.ItemBlockedFiles {
		return 0
	}
	return outcomes.ItemBlockedFiles - classified
}

func resultCount(result TransferResult, count uint64) string {
	prefix := ""
	if !result.CountersExact {
		prefix = ">="
	}
	return prefix + formatCount(count)
}

func countedResultNoun(result TransferResult, count uint64, singular, plural string) string {
	label := plural
	if count == 1 {
		label = singular
	}
	return resultCount(result, count) + " " + label
}

func exactCountedNoun(count uint64, singular, plural string) string {
	label := plural
	if count == 1 {
		label = singular
	}
	return formatCount(count) + " " + label
}

func formatNameAdjustments(summary engine.NameAdjustmentSummary) []terminalcanvas.Line {
	if summary.Count == 0 {
		return nil
	}
	// Count comes from the operation's persisted name-mapping ledger rather
	// than bounded transfer progress counters, so it is always exact.
	header := "  " + exactCountedNoun(
		summary.Count,
		"item saved under a compatible name",
		"items saved under compatible names",
	)
	if len(summary.Samples) == 0 {
		return []terminalcanvas.Line{terminalcanvas.Plain(header + ".")}
	}
	lines := make([]terminalcanvas.Line, 0, len(summary.Samples)+2)
	lines = append(lines, terminalcanvas.Plain(header+":"))
	for _, sample := range summary.Samples {
		lines = append(lines, terminalcanvas.Plain(
			"    "+escapedDisplay(sample.LogicalPath)+" -> "+escapedDisplay(sample.PhysicalPath),
		))
	}
	if summary.Count > uint64(len(summary.Samples)) {
		omitted := summary.Count - uint64(len(summary.Samples))
		// NameAdjustments spans the whole operation across retries, while --trace
		// is a per-run diagnostic stream that skips previously delivered receipts.
		lines = append(lines, terminalcanvas.Plain(
			"    "+formatCount(omitted)+" more; --trace logs mappings applied during a run.",
		))
	}
	return lines
}

func formatShareResult(result ShareResult, symbols Symbols) terminalcanvas.Line {
	if result.StoppedCleanly {
		if !result.Published {
			return statusLine(symbols.Success, "Sharing canceled; no link was published", terminalcanvas.StyleSuccess)
		}
		// No sender traffic total exists with stable multi-receiver semantics yet.
		return statusLine(symbols.Success, "Sharing stopped", terminalcanvas.StyleSuccess)
	}
	var failure engine.Failure
	if result.Failure != nil {
		failure = *result.Failure
	}
	return failureLine("Sharing failed", failure, symbols.Failure, terminalcanvas.StyleError)
}
