package humanoutput

import (
	"strconv"
	"strings"
	"time"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
	"github.com/windshare/windshare/cmd/windshare/internal/terminalcanvas"
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

func failureMessage(failure clievent.Failure) string {
	key, _ := failure.MessageKey()
	var message string
	switch key {
	case clievent.MessageInterrupted:
		message = "The operation was interrupted."
	case clievent.MessageTimedOut:
		message = "The operation timed out."
	case clievent.MessageInvalidRequest:
		message = "The request is invalid."
	case clievent.MessageCapabilityInvalid:
		message = "The share link or key is invalid."
	case clievent.MessageSelectionMissing:
		message = "The selected content was not found."
	case clievent.MessageRelayRejected:
		message = "The relay rejected the request."
	case clievent.MessageRelayUnavailable:
		message = "The relay is unavailable."
	case clievent.MessageDirectUnavailable:
		message = "The direct connection is unavailable."
	case clievent.MessageSourceUnavailable:
		message = "The source content is unavailable."
	case clievent.MessageSourceChanged:
		message = "The source content changed during transfer."
	case clievent.MessageCatalogUnavailable:
		message = "The content catalog is unavailable."
	case clievent.MessageSessionFailed:
		message = "The transfer session failed."
	case clievent.MessageOutputFailed:
		message = "The destination could not be updated safely."
	case clievent.MessageCheckpointFailed:
		message = "The recovery checkpoint could not be updated safely."
	case clievent.MessagePublicationFailed:
		message = "The share link could not be published."
	case clievent.MessageTraceIncomplete:
		message = "The trace is incomplete."
	case clievent.MessageTraceExists:
		message = "The trace path already exists; prior evidence was preserved and command/output state was untouched. Choose a new --trace path, use --trace-dir, or omit tracing."
	case clievent.MessageOutputNeedsAttention:
		message = "The destination needs attention before the transfer can continue."
	default:
		message = "An unexpected error occurred."
	}
	if retryMillis, ok := failure.RetryAfterMillis(); ok {
		seconds := (retryMillis + 999) / 1000
		message += " Retry after " + formatClockDuration(seconds) + "."
	}
	return message
}

func failureLine(label string, failure clievent.Failure, symbol string, style terminalcanvas.Style) terminalcanvas.Line {
	return statusLine(symbol, label+": "+failureMessage(failure), style)
}
