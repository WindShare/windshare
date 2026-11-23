package share

import (
	"sync"

	"github.com/windshare/windshare/core/diagnostics"
)

const shareSleepInhibitReason = "WindShare active file transfer"

// sleepGuard holds at most one OS sleep assertion while one or more receiver
// sessions are active, leaving idle shares free to let the machine sleep.
type sleepGuard struct {
	inhibitor   SleepInhibitor
	diagnostics diagnostics.Sink

	mu      sync.Mutex
	active  int
	release func()
}

func newSleepGuard(inhibitor SleepInhibitor, sink diagnostics.Sink) *sleepGuard {
	return &sleepGuard{inhibitor: inhibitor, diagnostics: sink}
}

func (g *sleepGuard) enter() func() {
	if g == nil || g.inhibitor == nil {
		return func() {}
	}
	g.mu.Lock()
	g.active++
	shouldAcquire := g.active == 1 && g.release == nil
	g.mu.Unlock()

	if shouldAcquire {
		g.acquire()
	}

	var once sync.Once
	return func() {
		once.Do(g.leave)
	}
}

func (g *sleepGuard) acquire() {
	release, err := g.inhibitor.InhibitSleep(shareSleepInhibitReason)
	if err != nil {
		if g.diagnostics.Enabled() {
			g.diagnostics.Emit(diagnostics.New("share_sleep_inhibit", diagnostics.Fields{
				"transition": diagnostics.Text("acquire_failed"),
				"failure":    diagnostics.Text(err.Error()),
			}))
		}
		return
	}
	if release == nil {
		return
	}
	g.mu.Lock()
	if g.active > 0 && g.release == nil {
		g.release = release
		release = nil
	}
	g.mu.Unlock()
	if release != nil {
		release()
	}
}

func (g *sleepGuard) leave() {
	var release func()
	g.mu.Lock()
	if g.active > 0 {
		g.active--
	}
	if g.active == 0 && g.release != nil {
		release = g.release
		g.release = nil
	}
	g.mu.Unlock()
	if release != nil {
		release()
	}
}
