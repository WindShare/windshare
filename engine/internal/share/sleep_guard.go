package share

import (
	"sync"
	"time"

	"github.com/windshare/windshare/core/diagnostics"
)

const (
	shareSleepInhibitReason = "WindShare active file transfer"
	// shareSleepInhibitCooldown bridges gaps between sequential block requests,
	// multi-file commits, and brief path recovery without flapping OS sleep
	// assertions (especially macOS caffeinate subprocesses). Matching the 45s
	// relay silence horizon preserves short-outage continuity while letting
	// idle receiver sessions allow host sleep promptly.
	shareSleepInhibitCooldown = 45 * time.Second
)

// sleepGuard holds at most one OS sleep assertion while sender content
// transfers are active or within a short post-transfer cooldown, leaving idle
// shares and idle receiver sessions free to let the machine sleep.
type sleepGuard struct {
	inhibitor   SleepInhibitor
	diagnostics diagnostics.Sink

	mu          sync.Mutex
	closed      bool
	active      int
	acquiring   bool
	release     func()
	cooldownGen uint64
	timer       *time.Timer
	inFlight    sync.WaitGroup
}

func newSleepGuard(inhibitor SleepInhibitor, sink diagnostics.Sink) *sleepGuard {
	return &sleepGuard{inhibitor: inhibitor, diagnostics: sink}
}

func (g *sleepGuard) BeginTransfer() func() {
	return g.enter()
}

func (g *sleepGuard) enter() func() {
	if g == nil || g.inhibitor == nil {
		return func() {}
	}
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return func() {}
	}
	g.active++
	g.cancelCooldownLocked()
	shouldAcquire := g.release == nil && !g.acquiring
	if shouldAcquire {
		g.acquiring = true
		g.inFlight.Add(1)
	}
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
	defer g.inFlight.Done()
	release, err := g.inhibitor.InhibitSleep(shareSleepInhibitReason)
	if err != nil {
		g.mu.Lock()
		g.acquiring = false
		g.mu.Unlock()
		if g.diagnostics.Enabled() {
			g.diagnostics.Emit(diagnostics.New("share_sleep_inhibit", diagnostics.Fields{
				"transition": diagnostics.Text("acquire_failed"),
				"failure":    diagnostics.Text(err.Error()),
			}))
		}
		return
	}
	acquired := false
	g.mu.Lock()
	g.acquiring = false
	if release != nil && !g.closed && g.active > 0 && g.release == nil {
		g.release = release
		release = nil
		acquired = true
	}
	g.mu.Unlock()
	if acquired {
		if g.diagnostics.Enabled() {
			g.diagnostics.Emit(diagnostics.New("share_sleep_inhibit", diagnostics.Fields{
				"transition": diagnostics.Text("acquired"),
			}))
		}
		return
	}
	if release != nil {
		release()
	}
}

func (g *sleepGuard) leave() {
	g.mu.Lock()
	if g.active > 0 {
		g.active--
	}
	if g.active == 0 && !g.closed && g.release != nil {
		g.armCooldownLocked()
	}
	g.mu.Unlock()
}

func (g *sleepGuard) close() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.closed = true
	g.cancelCooldownLocked()
	release := g.takeReleaseLocked()
	g.mu.Unlock()
	if release != nil {
		release()
		g.emitReleased("closed")
	}
	g.inFlight.Wait()
}

func (g *sleepGuard) armCooldownLocked() {
	if g.timer != nil {
		g.timer.Stop()
	}
	g.cooldownGen++
	gen := g.cooldownGen
	g.timer = time.AfterFunc(shareSleepInhibitCooldown, func() {
		g.expireCooldown(gen)
	})
}

func (g *sleepGuard) cancelCooldownLocked() {
	g.cooldownGen++
	if g.timer != nil {
		g.timer.Stop()
		g.timer = nil
	}
}

func (g *sleepGuard) expireCooldown(gen uint64) {
	var release func()
	g.mu.Lock()
	if !g.closed && g.active == 0 && g.cooldownGen == gen {
		g.timer = nil
		release = g.takeReleaseLocked()
		if release != nil {
			g.inFlight.Add(1)
		}
	}
	g.mu.Unlock()
	if release != nil {
		defer g.inFlight.Done()
		release()
		g.emitReleased("cooldown_expired")
	}
}

func (g *sleepGuard) takeReleaseLocked() func() {
	release := g.release
	g.release = nil
	return release
}

func (g *sleepGuard) emitReleased(reason string) {
	if !g.diagnostics.Enabled() {
		return
	}
	g.diagnostics.Emit(diagnostics.New("share_sleep_inhibit", diagnostics.Fields{
		"transition": diagnostics.Text("released"),
		"reason":     diagnostics.Text(reason),
	}))
}
