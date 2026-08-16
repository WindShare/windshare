package cli

import (
	"io"
	"strings"
	"sync"

	"github.com/windshare/windshare/cmd/windshare/internal/terminalcanvas"
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

// completeLineCanvasWriter is intentionally diagnostic-only: Canvas retains
// the first write failure for health observation while callers receive a full
// successful write so stderr cannot acquire command or transfer authority.
type completeLineCanvasWriter struct {
	canvas *terminalcanvas.Canvas
}

func (writer completeLineCanvasWriter) Write(payload []byte) (int, error) {
	if len(payload) == 0 {
		return 0, nil
	}
	if writer.canvas == nil {
		return len(payload), nil
	}
	writer.canvas.Insert(splitCompleteLines(string(payload)))
	return len(payload), nil
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

var _ io.Writer = completeLineCanvasWriter{}
