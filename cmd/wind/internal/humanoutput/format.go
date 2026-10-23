package humanoutput

import (
	"strconv"
	"strings"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
	appobs "github.com/windshare/windshare/engine/observation"
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
	return strconv.FormatUint(whole, 10) + "." + strconv.FormatUint(tenth, 10) + " " + suffix
}

func FormatRate(bytesPerSecond uint64) string {
	return FormatBytes(bytesPerSecond) + "/s"
}

func FormatElapsed(elapsed time.Duration) string {
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed < time.Minute {
		tenths := (elapsed + 50*time.Millisecond) / (100 * time.Millisecond)
		return strconv.FormatInt(int64(tenths/10), 10) + "." + strconv.FormatInt(int64(tenths%10), 10) + "s"
	}
	totalSeconds := uint64((elapsed + time.Second/2) / time.Second)
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

func eventName(value interface{ Name() (string, bool) }) string {
	name, _ := value.Name()
	return strings.ReplaceAll(name, "_", " ")
}

func titleName(value interface{ Name() (string, bool) }) string {
	name := eventName(value)
	if name == "" {
		return "Unknown"
	}
	return strings.ToUpper(name[:1]) + name[1:]
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

func failureMessage(failure appobs.Failure) string {
	key, _ := failure.MessageKey()
	var message string
	switch key {
	case appobs.MessageInterrupted:
		message = "The operation was interrupted."
	case appobs.MessageTimedOut:
		message = "The operation timed out."
	case appobs.MessageInvalidRequest:
		message = "The request is invalid."
	case appobs.MessageCapabilityInvalid:
		message = "The share link or key is invalid."
	case appobs.MessageSelectionMissing:
		message = "The selected content was not found."
	case appobs.MessageRelayRejected:
		message = "The relay rejected the request."
	case appobs.MessageRelayUnavailable:
		message = "The relay is unavailable."
	case appobs.MessageShareUnavailable:
		message = "The share is not currently available. Retry the original link."
	case appobs.MessageDirectUnavailable:
		message = "The direct connection is unavailable."
	case appobs.MessageDirectPolicy:
		message = "The direct path was rejected by connection policy."
	case appobs.MessageDirectBusy:
		message = "A previous direct connection attempt is still finishing."
	case appobs.MessageDirectAdmission:
		message = "The direct channel could not be admitted."
	case appobs.MessageSourceUnavailable:
		message = "The source content is unavailable."
	case appobs.MessageSourceMissing:
		message = "Source path not found. Check the path and try again."
	case appobs.MessageSourceAccessDenied:
		message = "Permission denied while opening the source. Check read access to the source and its parent directory."
	case appobs.MessageSourceUnsupported:
		message = "This source cannot be shared safely. Select a regular file or directory on a supported filesystem; symbolic links are not supported."
	case appobs.MessageSourceChanged:
		message = "The source content changed during transfer."
		if _, selected := failure.SourcePath(); selected {
			message = "The source changed while it was being opened. Retry the share."
		}
	case appobs.MessageCatalogUnavailable:
		message = "The content catalog is unavailable."
	case appobs.MessageSessionFailed:
		message = "The transfer session failed."
	case appobs.MessageOutputFailed:
		message = "The destination could not be updated safely."
	case appobs.MessageOutputRecoveryUnavailable:
		message = "Downloading without restart recovery. Keep WindShare running; an unexpected exit may leave unfinished files."
	case appobs.MessageCheckpointFailed:
		message = "The recovery checkpoint could not be updated safely."
	case appobs.MessagePublicationFailed:
		message = "The share link could not be published."
	case appobs.MessageTraceIncomplete:
		message = "The trace is incomplete."
	case appobs.MessageTraceExists:
		message = "The trace path already exists; prior evidence was preserved and command/output state was untouched. Choose a new --trace path, use --trace-dir, or omit tracing."
	case appobs.MessageOutputNeedsAttention:
		message = "The destination needs attention before the transfer can continue."
	case appobs.MessageDestinationCollision:
		message = "Existing destinations prevented one or more files from completing."
	case appobs.MessageCheckpointRevisionConflict:
		message = "Local resume data belongs to another source revision; no destination was replaced."
	case appobs.MessageCheckpointInvalid:
		message = "Local resume data has an invalid checkpoint binding; existing evidence was preserved."
	case appobs.MessageOwnedObjectUnknown:
		message = "Local resume data disagrees about which destination object it owns; no automatic change was made."
	default:
		message = "An unexpected error occurred."
	}
	if retryMillis, ok := failure.RetryAfterMillis(); ok {
		seconds := (retryMillis + 999) / 1000
		message += " Retry after " + formatClockDuration(seconds) + "."
	}
	if path, ok := failure.SourcePath(); ok {
		message += " Path: " + escapedDisplay(path.Text())
	}
	return message
}

func failureLine(label string, failure appobs.Failure, symbol string, style terminalcanvas.Style) terminalcanvas.Line {
	return statusLine(symbol, label+": "+failureMessage(failure), style)
}
