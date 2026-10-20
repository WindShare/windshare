package provider

import (
	"fmt"

	"github.com/pion/logging"
)

// Pion's process-level console logger loses attempt identity. Retain its normal
// output while also recording actionable ICE/TURN failures in the path trace.
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
	if scope != "ice" && scope != "turnc" {
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
	l.observe(Diagnostic{Scope: l.scope, Severity: "error", Message: message})
	l.LeveledLogger.Error(message)
}
func (l providerLogger) Errorf(format string, args ...any) {
	l.Error(fmt.Sprintf(format, args...))
}
