package receive

import (
	"github.com/windshare/windshare/engine/internal/task"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestObservation(t *testing.T) (getObservation, *[]task.Event) {
	t.Helper()
	var mu sync.Mutex
	events := []task.Event{}
	observation := newObservation(task.Control{Now: time.Now, Emit: func(event task.Event) bool { mu.Lock(); defer mu.Unlock(); events = append(events, event); return true }}, false, nil)
	t.Cleanup(observation.complete)
	return observation, &events
}

type testRuntimeCloser struct{ calls atomic.Int32 }

func (r *testRuntimeCloser) Close() { r.calls.Add(1) }

func warningEvents(events []task.Event) []task.Event {
	var out []task.Event
	for _, event := range events {
		if _, ok := event.(Warning); ok {
			out = append(out, event)
		}
	}
	return out
}
