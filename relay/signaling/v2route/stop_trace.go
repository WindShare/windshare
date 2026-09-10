package v2route

import (
	"errors"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

type StopTrace struct {
	ShareID       v2.ShareID
	StopID        v2.StopID
	Outcome       string
	ActiveRoutes  int
	RouteCapacity int
	Err           error
}

type StopTracer interface{ TraceStop(StopTrace) }
type StopTraceFunc func(StopTrace)

func (f StopTraceFunc) TraceStop(event StopTrace) {
	if f != nil {
		f(event)
	}
}

func (r *Registry) traceStop(init v2.StopInit, err error) {
	if r.stopTracer == nil {
		return
	}
	outcome := "committed"
	if errors.Is(err, ErrCommitUncertain) {
		outcome = "uncertain"
	} else if err != nil {
		outcome = "failed"
	}
	r.mu.Lock()
	active := len(r.routes)
	r.mu.Unlock()
	// Observers run outside the registry lock so diagnostics cannot stall
	// established routing or re-enter the registry while it is locked.
	r.stopTracer.TraceStop(StopTrace{
		ShareID: init.ShareID, StopID: init.StopID, Outcome: outcome,
		ActiveRoutes: active, RouteCapacity: r.maxRoutes, Err: err,
	})
}
