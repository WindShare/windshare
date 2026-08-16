package cli

import (
	"sync"
	"testing"
	"time"
)

func TestCommandClockProvidesOneInjectableTimeAuthority(t *testing.T) {
	want := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	clock := newSystemCommandClock(func() time.Time { return want })
	if got := clock.Now(); !got.Equal(want) {
		t.Fatalf("Now=%v want=%v", got, want)
	}
	timer := clock.NewTimer(time.Hour)
	if timer.C() == nil || !timer.Stop() {
		t.Fatal("new timer did not expose a live stoppable channel")
	}
	ticker := clock.NewTicker(time.Hour)
	if ticker.C() == nil {
		t.Fatal("new ticker did not expose a channel")
	}
	ticker.Stop()
}

type fakeCommandClock struct {
	mu              sync.Mutex
	now             time.Time
	tickerIntervals []time.Duration
}

func (clock *fakeCommandClock) Now() time.Time { return clock.now }

func (*fakeCommandClock) NewTimer(time.Duration) commandTimer {
	return &fakeCommandTimer{channel: make(chan time.Time)}
}

func (clock *fakeCommandClock) NewTicker(interval time.Duration) commandTicker {
	clock.mu.Lock()
	clock.tickerIntervals = append(clock.tickerIntervals, interval)
	clock.mu.Unlock()
	return &fakeCommandTicker{channel: make(chan time.Time)}
}

type fakeCommandTimer struct{ channel chan time.Time }

func (timer *fakeCommandTimer) C() <-chan time.Time { return timer.channel }
func (*fakeCommandTimer) Stop() bool                { return true }

type fakeCommandTicker struct{ channel chan time.Time }

func (ticker *fakeCommandTicker) C() <-chan time.Time { return ticker.channel }
func (*fakeCommandTicker) Stop()                      {}
