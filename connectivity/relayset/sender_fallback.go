package relayset

import (
	"context"
	"log/slog"

	"github.com/windshare/windshare/connectivity/relayplan"
)

// A standby wave belongs only to startup. Once any route is ready the link's
// candidates keep their independent retry and resume lifetimes.
type startupFallback struct {
	active   []string
	canaries []string
	untried  map[string]struct{}
	limit    int
	dial     SenderFactory
}

func NewPlannedSender(ctx context.Context, plan relayplan.Plan, dial SenderFactory) (*Sender, error) {
	urls := make([]string, 0, len(plan.Configured)+len(plan.Network))
	for _, c := range plan.Configured {
		urls = append(urls, c.URL)
	}
	active := make([]string, 0, len(plan.Network))
	for _, c := range plan.Network {
		active = append(active, c.URL)
	}
	urls = append(urls, active...)
	if len(plan.Standby) == 0 {
		return NewSender(ctx, urls, dial)
	}
	limit := min(relayplan.MaximumNetworkCandidates, MaximumEndpoints-len(plan.Configured))
	if len(active) == 0 || len(active) > limit || len(plan.Standby) > limit {
		return nil, ErrConfig
	}
	fallback := &startupFallback{active: active, limit: limit, untried: make(map[string]struct{}, len(active)), dial: dial}
	for _, url := range active {
		fallback.untried[url] = struct{}{}
	}
	for _, c := range plan.Standby {
		fallback.canaries = append(fallback.canaries, c.URL)
	}
	return newSender(ctx, urls, fallback, dial)
}

func (s *Sender) initialFailureLocked(url string) {
	f := s.startup
	if f == nil || s.everReady || s.firstReady != "" || s.stopping || s.ctx.Err() != nil {
		return
	}
	if _, watched := f.untried[url]; !watched {
		return
	}
	delete(f.untried, url)
	if len(f.untried) != 0 {
		return
	}
	s.startup = nil
	// Free terminal routes first, then the lowest-ranked failed routes when the
	// link is full. Only the surviving candidates may reconnect after publication.
	drop := len(f.active) + len(f.canaries) - f.limit
	retire := func(url string) {
		delete(s.routes, url)
		s.cancelRoute[url]()
		drop--
	}
	for _, previous := range f.active {
		if drop > 0 && s.routes[previous].terminal {
			retire(previous)
		}
	}
	for i := len(f.active) - 1; i >= 0 && drop > 0; i-- {
		if _, exists := s.routes[f.active[i]]; exists {
			retire(f.active[i])
		}
	}
	for _, standby := range f.canaries {
		s.startLocked(standby, f.dial)
	}
	slog.InfoContext(s.ctx, "network_canary_fallback", "active_relays", f.active, "canary_relays", f.canaries)
}
