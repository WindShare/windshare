package v2peer

import (
	"sync"
	"time"
)

type manualTestClock struct {
	mu      sync.RWMutex
	current time.Time
}

func newManualTestClock(current time.Time) *manualTestClock {
	return &manualTestClock{current: current}
}

func (clock *manualTestClock) Now() time.Time {
	clock.mu.RLock()
	defer clock.mu.RUnlock()
	return clock.current
}

func (clock *manualTestClock) Advance(elapsed time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.current = clock.current.Add(elapsed)
}
