package provider

import (
	"fmt"

	"github.com/pion/logging"
)

// Pion's process-level console logger loses attempt identity. Retain its normal
// output while also recording actionable ICE/TURN failures in the path trace.
type providerLoggerFactory struct {
	logging.LoggerFactory
	observe func(string, string)
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
	observe func(string, string)
}

func (l providerLogger) Warn(message string) {
	l.observe(l.scope+"_warning", message)
	l.LeveledLogger.Warn(message)
}
func (l providerLogger) Warnf(format string, args ...interface{}) {
	l.Warn(fmt.Sprintf(format, args...))
}
func (l providerLogger) Error(message string) {
	l.observe(l.scope+"_error", message)
	l.LeveledLogger.Error(message)
}
func (l providerLogger) Errorf(format string, args ...interface{}) {
	l.Error(fmt.Sprintf(format, args...))
}
