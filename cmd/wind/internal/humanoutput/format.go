package humanoutput

import (
	"strconv"
	"strings"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayconfig"
)

const (
	decimalKilo = uint64(1_000)
	decimalMega = uint64(1_000_000)
	decimalGiga = uint64(1_000_000_000)
)

// FormatBytes uses SI decimal units because progress describes transfer bytes,
// not filesystem allocation units.
func FormatBytes(value uint64) string {
	unit, suffix := uint64(1), "B"
	switch {
	case value >= decimalGiga:
		unit, suffix = decimalGiga, "GB"
	case value >= decimalMega:
		unit, suffix = decimalMega, "MB"
	case value >= decimalKilo:
		unit, suffix = decimalKilo, "KB"
	}
	if unit == 1 {
		return formatCount(value) + " " + suffix
	}

	whole := value / unit
	// Round to one decimal without converting large counters through float64.
	tenth := ((value%unit)*10 + unit/2) / unit
	if tenth == 10 {
		whole++
		tenth = 0
	}
	// Rounding can reach the next unit's threshold (999.95 KB); showing
	// "1000.0 KB" would contradict the "1.0 MB" that 1,000,000 B reports.
	if whole == decimalKilo && unit < decimalGiga {
		return FormatBytes(unit * decimalKilo)
	}
	return strconv.FormatUint(whole, 10) + "." + strconv.FormatUint(tenth, 10) + " " + suffix
}

func FormatRate(bytesPerSecond uint64) string {
	return FormatBytes(bytesPerSecond) + "/s"
}

// secondTenthsPerMinute counts the 0.1s ticks FormatElapsed prints below one minute.
const secondTenthsPerMinute = 600

func FormatElapsed(elapsed time.Duration) string {
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed < time.Minute {
		tenths := (elapsed + 50*time.Millisecond) / (100 * time.Millisecond)
		// A value that rounds up to 60.0s belongs to the minute format below, which
		// prints it as "1m 0s"; "60.0s" would be a spelling no other duration uses.
		if tenths < secondTenthsPerMinute {
			return strconv.FormatInt(int64(tenths/10), 10) + "." + strconv.FormatInt(int64(tenths%10), 10) + "s"
		}
	}
	// Rounding after division keeps saturated durations (time.Sub clamps to the
	// maximum Duration) from wrapping into a negative sum.
	totalSeconds := uint64(elapsed / time.Second)
	if elapsed%time.Second >= time.Second/2 {
		totalSeconds++
	}
	return formatClockDuration(totalSeconds)
}

func FormatETA(seconds uint64) string {
	return formatClockDuration(seconds) + " left"
}

func formatClockDuration(totalSeconds uint64) string {
	if totalSeconds < 60 {
		return strconv.FormatUint(totalSeconds, 10) + "s"
	}
	if totalSeconds < 60*60 {
		return strconv.FormatUint(totalSeconds/60, 10) + "m " +
			strconv.FormatUint(totalSeconds%60, 10) + "s"
	}
	return strconv.FormatUint(totalSeconds/(60*60), 10) + "h " +
		strconv.FormatUint((totalSeconds/60)%60, 10) + "m"
}

func formatCount(value uint64) string {
	digits := strconv.FormatUint(value, 10)
	if len(digits) <= 3 {
		return digits
	}
	first := len(digits) % 3
	if first == 0 {
		first = 3
	}
	var output strings.Builder
	output.Grow(len(digits) + len(digits)/3)
	output.WriteString(digits[:first])
	for index := first; index < len(digits); index += 3 {
		output.WriteByte(',')
		output.WriteString(digits[index : index+3])
	}
	return output.String()
}

func escapedDisplay(text string) string {
	// Escaping here, before layout and styling, makes width decisions exact.
	// Canvas repeats the operation defensively, and visible escapes are idempotent.
	return terminalcanvas.EscapeText(text)
}

func statusLine(symbol, text string, style terminalcanvas.Style) terminalcanvas.Line {
	if symbol == "" {
		return terminalcanvas.NewLine(
			terminalcanvas.Span{Text: text},
		)
	}
	return terminalcanvas.NewLine(
		terminalcanvas.Span{Text: symbol + " ", Style: style},
		terminalcanvas.Span{Text: text},
	)
}

const ConfigureRelaysGuidance = "configure relay candidates with wind config import <file|->, or pass --relay for this share"

// Sub-second RetryAfter values (such as relay-core's 250ms lifecycle and
// admission polling hints) pace automatic client retries rather than human
// action; rounding them up to "Retry after 1s." distorts the delay and misleads
// CLI users after a terminal wait has already elapsed.
const minHumanRetryAfter = time.Second

// failureReason describes what failed without appending manual retry advice.
// Non-terminal recovery attempts and live warnings use it because the command
// is still retrying on the user's behalf.
func failureReason(failure engine.Failure) string {
	return appendFailureSourcePath(failureCause(failure), failure.SourcePath)
}

// failureMessage describes a terminal failure together with actionable user
// recovery advice and any human-scale RetryAfter cooldown.
func failureMessage(failure engine.Failure) string {
	message := failureCause(failure)
	if advice := failureRecoveryAdvice(failure.Code); advice != "" {
		message += " " + advice
	}
	if retry := formatHumanRetryAfter(failure.RetryAfter); retry != "" {
		message += " " + retry
	}
	return appendFailureSourcePath(message, failure.SourcePath)
}

func failureCause(failure engine.Failure) string {
	switch failure.Code {
	case engine.FailureCanceled:
		return "The operation was interrupted."
	case engine.FailureDeadline:
		return "The operation timed out."
	case engine.FailureInvalidInput:
		return "The request is invalid."
	case engine.FailureNetworkUnavailable:
		return "The WindShare network is unavailable."
	case engine.FailureCapabilityInvalid:
		return "The share link or key is invalid."
	case engine.FailureSelectionMissing:
		return "The selected content was not found."
	case engine.FailureRetainedOutputMissing:
		return "No saved download matches this resume request."
	case engine.FailureSelectionCollision:
		message := "Selected names must differ even ignoring case. Rename one item or remove a duplicate selection."
		if failure.SelectionCollision != nil {
			return "Selected names collide: " + escapedDisplay(failure.SelectionCollision.FirstPath) + " and " +
				escapedDisplay(failure.SelectionCollision.SecondPath) + ". " + message
		}
		return message
	case engine.FailureRelayRouteNotFound:
		return relayconfig.RelayRouteGuidance(failure.RelayBaseHint)
	case engine.FailureRelayAccessDenied:
		return "Relay access was denied."
	case engine.FailureAccountStorageError:
		return "The local account file cannot be read or is invalid."
	case engine.FailureRelayRejected:
		return "The relay rejected the request."
	case engine.FailureRelayUnavailable:
		return "The relay is unavailable."
	case engine.FailureRelayNotFound:
		return "The share is not currently available."
	case engine.FailureRelayStopped:
		return "A relay reported that its route for this share was stopped."
	case engine.FailureShareNotReady:
		return "The share is not ready yet."
	case engine.FailureShareStopped:
		return "This share has stopped. Ask the sender for a new link."
	case engine.FailureDirectUnavailable:
		return "The direct connection is unavailable."
	case engine.FailureDirectPolicy:
		return "The direct path was rejected by connection policy."
	case engine.FailureDirectBusy:
		return "A previous direct connection attempt is still finishing."
	case engine.FailureDirectAdmission:
		return "The direct channel could not be admitted."
	case engine.FailureSourceUnavailable, engine.FailureSourceMissing, engine.FailureSourceAccessDenied,
		engine.FailureSourceUnsupported, engine.FailureSourceCloudPlaceholder,
		engine.FailureSourceOmitted, engine.FailureSourceChanged:
		return sourceFailureMessage(failure)
	case engine.FailureCatalogUnavailable:
		return "The content catalog is unavailable."
	case engine.FailureSessionFailed:
		return "The transfer session failed."
	case engine.FailureOutputFailed, engine.FailureOutputBusy, engine.FailureOutputOperationAlreadyRunning,
		engine.FailureCheckpointFailed,
		engine.FailureOutputNeedsAttention, engine.FailureDestinationCollision,
		engine.FailureCheckpointRevisionConflict, engine.FailureCheckpointInvalid, engine.FailureOwnedObjectUnknown:
		return outputFailureMessage(failure.Code)
	case engine.FailurePublication:
		return "The share link could not be published."
	default:
		return "An unexpected error occurred."
	}
}

func failureRecoveryAdvice(code engine.FailureCode) string {
	switch code {
	case engine.FailureNetworkUnavailable:
		return "Check your connection and retry."
	case engine.FailureRelayAccessDenied:
		return "Check your account or relay authorization."
	case engine.FailureAccountStorageError:
		return "Repair or remove it, then retry."
	case engine.FailureRelayNotFound:
		return "Retry the original link."
	default:
		return ""
	}
}

func formatHumanRetryAfter(retryAfter time.Duration) string {
	if retryAfter < minHumanRetryAfter {
		return ""
	}
	seconds := uint64(retryAfter / time.Second)
	if retryAfter%time.Second != 0 {
		seconds++
	}
	return "Retry after " + formatClockDuration(seconds) + "."
}

func appendFailureSourcePath(message, sourcePath string) string {
	if sourcePath == "" {
		return message
	}
	return message + " Path: " + escapedDisplay(sourcePath)
}

func outputFailureMessage(code engine.FailureCode) string {
	switch code {
	case engine.FailureOutputBusy:
		return "The destination is busy. Check for another WindShare command or a program holding its files open before retrying."
	case engine.FailureOutputOperationAlreadyRunning:
		return "This download is already running at this destination. Check the running command; if the download stops before completing, rerun this command to resume."
	case engine.FailureCheckpointFailed:
		return "The recovery checkpoint could not be updated safely."
	case engine.FailureOutputNeedsAttention:
		return "The destination needs attention before the transfer can continue."
	case engine.FailureDestinationCollision:
		return "Existing destinations prevented one or more files from completing."
	case engine.FailureCheckpointRevisionConflict:
		return "Local resume data belongs to another source revision; no destination was replaced."
	case engine.FailureCheckpointInvalid:
		return "Local resume data has an invalid checkpoint binding; existing evidence was preserved."
	case engine.FailureOwnedObjectUnknown:
		return "Local resume data disagrees about which destination object it owns; no automatic change was made."
	default:
		return "The destination could not be updated safely."
	}
}

func sourceFailureMessage(failure engine.Failure) string {
	switch failure.Code {
	case engine.FailureSourceMissing:
		return "Source path not found. Check the path and try again."
	case engine.FailureSourceAccessDenied:
		return "Permission denied while opening the source. Check read access to the source and its parent directory."
	case engine.FailureSourceUnsupported:
		return "This source cannot be shared safely. Select a regular file or directory on a supported filesystem; symbolic links are not supported."
	case engine.FailureSourceCloudPlaceholder:
		return "This item is stored in the cloud and not fully available on this device. Make this item available on this device, then share it again."
	case engine.FailureSourceOmitted:
		return "The sender could not include some source items. Ask the sender to check the skipped names and reasons, fix the source, and share it again."
	case engine.FailureSourceChanged:
		if failure.SourcePath != "" {
			return "The source changed while it was being opened. Retry the share."
		}
		return "The source content changed during transfer."
	default:
		return "The source content is unavailable."
	}
}
