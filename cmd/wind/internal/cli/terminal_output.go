package cli

import (
	"log/slog"
	"strings"
	"sync"

	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
)

type terminalCapabilityProvider = terminalcanvas.CapabilityProvider
type terminalCellWidthFunc = terminalcanvas.CellWidthFunc

type terminalOutputState struct {
	once    sync.Once
	canvas  *terminalcanvas.Canvas
	caps    terminalcanvas.CapabilityProvider
	clock   commandClock
	width   terminalcanvas.CellWidthFunc
	closeMu sync.Mutex
	closed  bool
}

func (a *App) terminalOutput() *terminalOutputState {
	a.terminal.once.Do(func() {
		var raw terminalcanvas.RawTerminal
		if candidate, ok := a.Stderr.(terminalcanvas.RawTerminal); ok {
			raw = candidate
		}
		environment := terminalcanvas.NewNativeEnvironment(raw)
		a.terminal.caps = a.terminalCapabilities
		if a.terminal.caps == nil {
			a.terminal.caps = environment.Capabilities
		}
		a.terminal.clock = a.clock
		if a.terminal.clock == nil {
			a.terminal.clock = newSystemCommandClock(environment.Clock.Now)
		}
		a.terminal.width = a.terminalCellWidth
		a.terminal.canvas = terminalcanvas.New(terminalcanvas.Config{
			Writer:       a.Stderr,
			Capabilities: a.terminal.caps,
			CellWidth:    a.terminal.width,
		})
	})
	return &a.terminal
}

func (a *App) closeTerminalOutput() {
	terminal := a.terminalOutput()
	terminal.closeMu.Lock()
	defer terminal.closeMu.Unlock()
	if terminal.closed {
		return
	}
	terminal.canvas.Close()
	terminal.closed = true
}

// canvasLineOutput accepts complete records, never byte-stream fragments.
// Calls commit immediately, including prompts without a trailing newline.
// Canvas retains failures for health observation without giving diagnostics
// command or transfer authority.
type canvasLineOutput struct {
	canvas *terminalcanvas.Canvas
}

func (output canvasLineOutput) WriteLines(records ...string) {
	if output.canvas == nil {
		return
	}
	var lines []terminalcanvas.Line
	for _, record := range records {
		lines = append(lines, splitCompleteLines(record)...)
	}
	output.canvas.Insert(lines)
}

func (output canvasLineOutput) jsonHandler() *slog.JSONHandler {
	return slog.NewJSONHandler(jsonLogRecordWriter{output: output}, nil)
}

// slog.JSONHandler guarantees one Write per complete JSON record. Keep its
// adapter private to the handler so commands cannot mistake it for a byte stream.
type jsonLogRecordWriter struct {
	output canvasLineOutput
}

func (writer jsonLogRecordWriter) Write(record []byte) (int, error) {
	if len(record) > 0 {
		writer.output.WriteLines(string(record))
	}
	return len(record), nil
}

func splitCompleteLines(value string) []terminalcanvas.Line {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	parts := strings.Split(value, "\n")
	if len(parts) > 1 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	lines := make([]terminalcanvas.Line, 0, len(parts))
	for _, part := range parts {
		lines = append(lines, terminalcanvas.Plain(part))
	}
	return lines
}
