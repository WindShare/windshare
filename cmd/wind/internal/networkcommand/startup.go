package networkcommand

import (
	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/internal/platformsetup"
)

type StartupReader interface {
	Startup() platformsetup.StartupReport
}
type Observer interface{ Observe(clievent.Event) bool }

// ObserveStartup reads only installation history. Keeping this port separate
// from Check prevents diagnostics from leaking into ordinary transfer startup.
func ObserveStartup(reader StartupReader, observer Observer, command clievent.Command) {
	if reader == nil {
		return
	}
	event, err := clievent.NewNetworkSetupObserved(command, reader.Startup())
	if err == nil {
		observer.Observe(event)
	}
}
