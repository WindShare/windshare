package networkcommand

import "github.com/windshare/windshare/internal/platformsetup"

type StartupReader interface {
	Startup() platformsetup.StartupReport
}

// ObserveStartup reads only installation history. Keeping this port separate
// from Check prevents diagnostics from leaking into ordinary transfer startup.
func ObserveStartup(reader StartupReader, observe func(platformsetup.StartupReport)) {
	if reader == nil || observe == nil {
		return
	}
	observe(reader.Startup())
}
