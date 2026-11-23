package pionpeer

import (
	"fmt"
	"io"
	"strings"

	"github.com/pion/logging"
)

// Pion's process-level console logger loses attempt identity. Retain its normal
// output while also recording actionable ICE failures in the path trace.
// Do not suppress further Pion console noise by adding patches under
// third_party/pion; route or filter it through WindShare's logger wiring instead.
type providerLoggerFactory struct {
	logging.LoggerFactory
	observe func(Diagnostic)
}

// Diagnostic keeps provider log text separate from transport state transitions.
type Diagnostic struct {
	Scope    string
	Severity string
	Message  string
}

func (c *Connection) observeDiagnostic(diagnostic Diagnostic) {
	if c.request.Observe == nil {
		return
	}
	event := c.event("provider_diagnostic", "")
	event.Diagnostic = &diagnostic
	c.request.Observe(event)
}

func (f providerLoggerFactory) NewLogger(scope string) logging.LeveledLogger {
	base := f.LoggerFactory.NewLogger(scope)
	if scope != "ice" {
		return base
	}
	return providerLogger{LeveledLogger: base, scope: scope, observe: f.observe}
}

type providerLogger struct {
	logging.LeveledLogger
	scope   string
	observe func(Diagnostic)
}

func (l providerLogger) Warn(message string) {
	l.observe(Diagnostic{Scope: l.scope, Severity: "warning", Message: message})
	l.LeveledLogger.Warn(message)
}
func (l providerLogger) Warnf(format string, args ...any) {
	l.Warn(fmt.Sprintf(format, args...))
}
func (l providerLogger) Error(message string) {
	// Late packets arriving while a shared UDP mux connection is closing trigger
	// io.ErrClosedPipe in upstream pion/ice; downgrade routine shutdown drops here.
	if strings.Contains(message, "Failed to write packet:") && strings.Contains(message, io.ErrClosedPipe.Error()) {
		l.Trace(message)
		return
	}
	l.observe(Diagnostic{Scope: l.scope, Severity: "error", Message: message})
	l.LeveledLogger.Error(message)
}
func (l providerLogger) Errorf(format string, args ...any) {
	l.Error(fmt.Sprintf(format, args...))
}
