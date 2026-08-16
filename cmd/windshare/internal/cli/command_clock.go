package cli

import "time"

// commandClock is shared by presentation sampling and user trace metadata for
// one command. Connectivity policy keeps its own clock because transport
// deadlines must not become presentation policy accidentally.
type commandClock interface {
	Now() time.Time
	NewTimer(time.Duration) commandTimer
	NewTicker(time.Duration) commandTicker
}

type commandTimer interface {
	C() <-chan time.Time
	Stop() bool
}

type commandTicker interface {
	C() <-chan time.Time
	Stop()
}

type systemCommandClock struct {
	now func() time.Time
}

func newSystemCommandClock(now func() time.Time) systemCommandClock {
	if now == nil {
		now = time.Now
	}
	return systemCommandClock{now: now}
}

func (clock systemCommandClock) Now() time.Time {
	return clock.now()
}

func (systemCommandClock) NewTimer(delay time.Duration) commandTimer {
	return systemCommandTimer{timer: time.NewTimer(delay)}
}

func (systemCommandClock) NewTicker(interval time.Duration) commandTicker {
	return systemCommandTicker{ticker: time.NewTicker(interval)}
}

type systemCommandTimer struct{ timer *time.Timer }

func (timer systemCommandTimer) C() <-chan time.Time { return timer.timer.C }
func (timer systemCommandTimer) Stop() bool          { return timer.timer.Stop() }

type systemCommandTicker struct{ ticker *time.Ticker }

func (ticker systemCommandTicker) C() <-chan time.Time { return ticker.ticker.C }
func (ticker systemCommandTicker) Stop()               { ticker.ticker.Stop() }
