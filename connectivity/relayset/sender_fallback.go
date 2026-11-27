package relayset

import (
	"context"
	"slices"

	"github.com/windshare/windshare/connectivity/relayplan"
)

// Fallback records a pre-publication transition to the next eligible routes.
type Fallback struct {
	Active []string
	Next   []string
}

// A standby wave belongs only to startup. Once any route is ready the link's
// candidates keep their independent retry and resume lifetimes.
type startupFallback struct {
	routes   map[string]relayplan.Candidate
	active   []string
	canaries []string
	later    [][]string
	untried  map[string]struct{}
	limit    int
	dial     SenderFactory
	observe  func(Fallback)
}

func NewPlannedSender(ctx context.Context, plan relayplan.Plan, dial SenderFactory, observeFallback func(Fallback)) (*Sender, error) {
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
	if len(active) == 0 || len(active) > limit {
		return nil, ErrConfig
	}
	fallback := &startupFallback{
		active:  active,
		limit:   limit,
		untried: make(map[string]struct{}, len(active)),
		routes:  make(map[string]relayplan.Candidate),
		dial:    dial,
		observe: observeFallback,
	}
	for _, c := range plan.NetworkCandidates() {
		fallback.routes[c.URL] = c
	}
	for _, url := range active {
		fallback.untried[url] = struct{}{}
	}
	var wave []string
	var previous relayplan.Candidate
	for _, c := range plan.Standby {
		if len(wave) > 0 && (c.Source != previous.Source || c.Canary != previous.Canary || c.IPLiteral != previous.IPLiteral) {
			fallback.later = append(fallback.later, wave)
			wave = nil
		}
		wave = append(wave, c.URL)
		previous = c
	}
	fallback.later = append(fallback.later, wave)
	for _, group := range fallback.later {
		if len(group) > limit {
			return nil, ErrConfig
		}
	}
	fallback.canaries, fallback.later = fallback.later[0], fallback.later[1:]
	return newSender(ctx, urls, fallback, dial)
}

func (s *Sender) initialFailureLocked(url string) func() {
	f := s.startup
	if f == nil || s.everReady || s.firstReady != "" || s.stopping || s.ctx.Err() != nil {
		return nil
	}
	if _, watched := f.untried[url]; !watched {
		return nil
	}
	delete(f.untried, url)
	if replacement := f.substitute(url); replacement != "" {
		delete(s.routes, url)
		s.cancelRoute[url]()
		f.active[slices.Index(f.active, url)] = replacement
		f.untried[replacement] = struct{}{}
		s.startLocked(replacement, f.dial)
		return f.notification([]string{url}, []string{replacement})
	}
	if len(f.untried) != 0 {
		return nil
	}
	s.startup = nil
	if len(f.canaries) == 0 {
		return nil
	}
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
	active, added := f.active, f.canaries
	if len(f.later) > 0 {
		var retained []string
		for _, url := range active {
			if _, ok := s.routes[url]; ok {
				retained = append(retained, url)
			}
		}
		f.active = slices.Concat(retained, added)
		f.untried = make(map[string]struct{}, len(added))
		for _, url := range added {
			f.untried[url] = struct{}{}
		}
		f.canaries, f.later = f.later[0], f.later[1:]
		s.startup = f
	}
	return f.notification(active, added)
}

// Replace a failed official zone while another route is still starting. The
// replacement is frozen into the link only if it was selected before admission.
func (f *startupFallback) substitute(failed string) string {
	c := f.routes[failed]
	if c.Source != relayplan.Official || c.Zone == "" {
		return ""
	}
	return f.takeStandby(func(other relayplan.Candidate) bool {
		return other.Source == relayplan.Official && other.Zone == c.Zone
	})
}

func (f *startupFallback) takeStandby(match func(relayplan.Candidate) bool) string {
	waves := append([][]string{f.canaries}, f.later...)
	var next string
	for i, wave := range waves {
		j := slices.IndexFunc(wave, func(url string) bool { return match(f.routes[url]) })
		if j < 0 {
			continue
		}
		next = wave[j]
		waves[i] = slices.Delete(wave, j, j+1)
		if len(waves[i]) == 0 {
			waves = slices.Delete(waves, i, i+1)
		}
		f.canaries, f.later = nil, nil
		if len(waves) > 0 {
			f.canaries, f.later = waves[0], waves[1:]
		}
		break
	}
	return next
}

// Waiting on the shared official account cannot prevent a separately authorized
// community route from publishing. Keep the official pair alive for recovery.
func (s *Sender) communityDuringAuthorizationLocked() (func(), bool) {
	f := s.startup
	if f == nil || s.everReady || s.stopping || s.firstReady != "" || len(f.active) >= f.limit {
		return nil, false
	}
	for _, url := range f.active {
		route := s.routes[url]
		if f.routes[url].Source == relayplan.Community || (!route.terminal && route.state != RegistrationRetrying && route.state != RegistrationWaitingAuthorization && route.state != RegistrationNeedsUserAuthorization) {
			return nil, false
		}
	}
	url := f.takeStandby(func(c relayplan.Candidate) bool { return c.Source == relayplan.Community })
	if url == "" {
		return nil, false
	}
	notify := f.notification(f.active, []string{url})
	f.active = append(f.active, url)
	f.untried[url] = struct{}{}
	s.startLocked(url, f.dial)
	return notify, true
}

func (f *startupFallback) notification(active, added []string) func() {
	if f.observe == nil {
		return nil
	}
	event := Fallback{Active: slices.Clone(active), Next: slices.Clone(added)}
	return func() { f.observe(event) }
}
