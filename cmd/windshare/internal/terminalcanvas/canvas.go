package terminalcanvas

import (
	"bytes"
	"errors"
	"io"
	"sync"
)

const (
	clearTerminalLine = "\r\x1b[2K"
	resetStyle        = "\x1b[0m"
)

// ErrWriterUnavailable is latched when a canvas is constructed without its
// mandatory serialized output writer.
var ErrWriterUnavailable = errors.New("terminal canvas writer is unavailable")

// Config keeps the serialized writer independent from the raw descriptor used
// by native capability detection.
type Config struct {
	Writer       io.Writer
	Capabilities CapabilityProvider
	CellWidth    CellWidthFunc
}

// Canvas serializes complete lines around at most one dynamic progress line.
// Writer failures are observable through Err but deliberately never returned
// from rendering calls, so diagnostics cannot become transfer authority.
type Canvas struct {
	mu           sync.Mutex
	writer       io.Writer
	capabilities CapabilityProvider
	cellWidth    CellWidthFunc
	active       Line
	hasActive    bool
	closed       bool
	err          error
}

// New constructs a Canvas. Native callers should pass the serialized stderr
// writer here and the raw stderr handle only to NewNativeEnvironment.
func New(config Config) *Canvas {
	canvas := &Canvas{
		writer:       config.Writer,
		capabilities: config.Capabilities,
		cellWidth:    normalizeCellWidth(config.CellWidth),
	}
	if canvas.capabilities == nil {
		canvas.capabilities = noCapabilities()
	}
	if canvas.writer == nil {
		canvas.err = ErrWriterUnavailable
	}
	return canvas
}

// ReplaceProgress replaces the active dynamic line. When cursor ANSI is not
// available it writes an ordinary complete line instead of using carriage-return
// animation.
func (canvas *Canvas) ReplaceProgress(line Line) {
	canvas.mu.Lock()
	defer canvas.mu.Unlock()
	if canvas.disabledLocked() {
		return
	}

	capabilities := canvas.capabilities.Snapshot()
	if !dynamicAvailable(capabilities) {
		var output bytes.Buffer
		canvas.endAbandonedDynamicLineLocked(&output)
		canvas.renderLineLocked(&output, line, capabilities)
		output.WriteByte('\n')
		if canvas.writeLocked(output.Bytes()) {
			canvas.hasActive = false
		}
		return
	}

	var output bytes.Buffer
	output.WriteString(clearTerminalLine)
	canvas.renderLineLocked(&output, line, capabilities)
	if canvas.writeLocked(output.Bytes()) {
		canvas.active = line.clone()
		canvas.hasActive = true
	}
}

// Insert clears any active progress, writes complete lines, then redraws the
// latest progress from semantic spans using the current terminal width.
func (canvas *Canvas) Insert(lines []Line) {
	canvas.mu.Lock()
	defer canvas.mu.Unlock()
	if canvas.disabledLocked() || len(lines) == 0 {
		return
	}

	capabilities := canvas.capabilities.Snapshot()
	dynamic := dynamicAvailable(capabilities)
	var output bytes.Buffer
	if canvas.hasActive {
		if dynamic {
			output.WriteString(clearTerminalLine)
		} else {
			canvas.endAbandonedDynamicLineLocked(&output)
		}
	}
	for _, line := range lines {
		canvas.renderLineLocked(&output, line, capabilities)
		output.WriteByte('\n')
	}
	if canvas.hasActive && dynamic {
		output.WriteString(clearTerminalLine)
		canvas.renderLineLocked(&output, canvas.active, capabilities)
	}
	if canvas.writeLocked(output.Bytes()) && !dynamic {
		canvas.hasActive = false
	}
}

// FinishProgress removes the transient line and terminates its terminal row
// exactly once. It does not close the Canvas; later progress may start again.
func (canvas *Canvas) FinishProgress() {
	canvas.mu.Lock()
	defer canvas.mu.Unlock()
	canvas.finishProgressLocked()
}

// Close finishes any active progress row once and disables later output.
func (canvas *Canvas) Close() {
	canvas.mu.Lock()
	defer canvas.mu.Unlock()
	if canvas.closed {
		return
	}
	canvas.finishProgressLocked()
	canvas.closed = true
}

// Err returns the first writer or short-write error.
func (canvas *Canvas) Err() error {
	canvas.mu.Lock()
	defer canvas.mu.Unlock()
	return canvas.err
}

func (canvas *Canvas) finishProgressLocked() {
	if canvas.disabledLocked() || !canvas.hasActive {
		return
	}
	capabilities := canvas.capabilities.Snapshot()
	var output bytes.Buffer
	if dynamicAvailable(capabilities) {
		output.WriteString(clearTerminalLine)
	}
	output.WriteByte('\n')
	if canvas.writeLocked(output.Bytes()) {
		canvas.hasActive = false
	}
}

func (canvas *Canvas) endAbandonedDynamicLineLocked(output *bytes.Buffer) {
	if canvas.hasActive {
		// If cursor control disappears mid-run, a newline safely terminates the
		// old row without guessing that carriage returns are still supported.
		output.WriteByte('\n')
	}
}

func (canvas *Canvas) renderLineLocked(output *bytes.Buffer, line Line, capabilities Capabilities) {
	spans := clipSpans(line, capabilities.Columns, canvas.cellWidth)
	styled := dynamicAvailable(capabilities) && capabilities.Color
	for _, span := range spans {
		styleSequence := ""
		if styled {
			styleSequence = ansiForStyle(span.Style)
		}
		if styleSequence == "" {
			output.WriteString(span.Text)
			continue
		}
		output.WriteString(styleSequence)
		output.WriteString(span.Text)
		output.WriteString(resetStyle)
	}
}

func (canvas *Canvas) writeLocked(payload []byte) bool {
	if len(payload) == 0 {
		return true
	}
	written, err := canvas.writer.Write(payload)
	if err != nil {
		canvas.err = err
		return false
	}
	if written != len(payload) {
		canvas.err = io.ErrShortWrite
		return false
	}
	return true
}

func (canvas *Canvas) disabledLocked() bool {
	return canvas.closed || canvas.err != nil
}

func dynamicAvailable(capabilities Capabilities) bool {
	return capabilities.Interactive && capabilities.ANSI
}

func ansiForStyle(style Style) string {
	switch style {
	case StyleStrong:
		return "\x1b[1m"
	case StyleMuted:
		return "\x1b[2m"
	case StyleAccent:
		return "\x1b[36m"
	case StyleSuccess:
		return "\x1b[32m"
	case StyleWarning:
		return "\x1b[33m"
	case StyleError:
		return "\x1b[31m"
	default:
		return ""
	}
}
