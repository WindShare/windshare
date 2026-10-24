package nativepeer

import (
	"sync"

	"github.com/windshare/windshare/connectivity/socketauthority"
)

// A holding outlives attempt admission. Yielding only the attempt token cannot
// make space for a second receiver once a provider owns the physical sockets.
type optionalHolding struct {
	sockets  *socketauthority.Request
	yield    chan struct{}
	yielding bool
}

func (g *Admission) holdOptional(sockets *socketauthority.Request) (<-chan struct{}, func()) {
	h := &optionalHolding{sockets: sockets, yield: make(chan struct{})}
	g.mu.Lock()
	g.optional = append(g.optional, h)
	g.mu.Unlock()
	return h.yield, sync.OnceFunc(func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		for i, entry := range g.optional {
			if entry == h {
				g.optional = append(g.optional[:i], g.optional[i+1:]...)
				break
			}
		}
		g.drainLocked()
	})
}

func (g *Admission) yieldOptionalLocked(sockets *socketauthority.Request) {
	for _, h := range g.optional {
		if h.sockets.SamePool(sockets) && h.yielding {
			return
		}
	}
	for _, h := range g.optional {
		if h.sockets.SamePool(sockets) {
			h.yielding = true
			close(h.yield)
			return
		}
	}
}

func (n *NativePeerConnectivity) holdOptionalLocked(key pathKey, path *pathResources, sockets *socketauthority.Request) {
	if path.optionalDone != nil {
		return
	}
	yield, release := n.config.Admission.holdOptional(sockets)
	path.optionalDone = make(chan struct{})
	path.releaseOptional = release
	n.operations.Go(func() {
		select {
		case <-yield:
			n.ClosePath(key.session, key.path)
		case <-path.optionalDone:
		}
	})
}
