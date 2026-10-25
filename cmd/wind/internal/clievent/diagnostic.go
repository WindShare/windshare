package clievent

import (
	"github.com/windshare/windshare/core/diagnostics"
	appobs "github.com/windshare/windshare/engine/observation"
)

// Diagnostic is a recorder envelope, never an application event or task result.
type Diagnostic struct {
	Owner  Command
	Record diagnostics.Record
}

func (v Diagnostic) Command() Command  { return v.Owner }
func (Diagnostic) Level() appobs.Level { return appobs.LevelDebug }
func (v Diagnostic) Validate() error {
	if !v.Owner.Valid() || !v.Record.Valid() {
		return ErrInvalidEvent
	}
	return nil
}
