// Package capacitytrace routes process-scoped revision-capacity traces to the
// observations owned by the currently active share command.
package capacitytrace

import (
	"sync"

	"github.com/windshare/windshare/core/content/revisioncapacity"
)

// Router keeps the process-scoped coordinator independent from command
// lifetimes while routing immutable facts to the active share.
type Router struct {
	mu         sync.RWMutex
	sink       revisioncapacity.Tracer
	generation uint64
}

func (router *Router) TraceCapacity(value revisioncapacity.TraceEvent) {
	if router == nil {
		return
	}
	router.mu.RLock()
	sink := router.sink
	router.mu.RUnlock()
	if sink != nil {
		sink.TraceCapacity(value)
	}
}

// Bind installs the active command sink and returns an idempotent release.
// Generation fencing prevents a stale command release from unbinding its
// successor when command lifetimes overlap during shutdown.
func (router *Router) Bind(sink revisioncapacity.Tracer) func() {
	if router == nil || sink == nil {
		return func() {}
	}
	router.mu.Lock()
	router.generation++
	generation := router.generation
	router.sink = sink
	router.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			router.mu.Lock()
			if router.generation == generation {
				router.sink = nil
			}
			router.mu.Unlock()
		})
	}
}
