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
		if len(wave) > 0 && (c.Source != previous.Source || c.Canary != previous.Canary || c.IPLiteral != previous.IPLiteral || c.ServerRetryAt.IsZero() != previous.ServerRetryAt.IsZero()) {
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

func (s *Sender) advanceStartupLocked(url string, registration routeRegistration) func() {
	f := s.startup
	if f == nil || s.everReady || s.firstReady != "" || s.stopping || s.ctx.Err() != nil {
		return nil
	}
	if registration.failed() {
		if notify, advanced := s.initialFailureLocked(url); advanced {
			return notify
		}
		// A route failure can leave every remaining official route blocked on shared
		// authorization, or exhaust an active community probe while official routes wait.
		return s.communityDuringAuthorizationLocked()
	}
	if registration.waitingAuthorization() {
		return s.communityDuringAuthorizationLocked()
	}
	return nil
}

func (s *Sender) initialFailureLocked(url string) (func(), bool) {
	f := s.startup
	if f == nil {
		return nil, false
	}
	if _, watched := f.untried[url]; !watched {
		return nil, false
	}
	delete(f.untried, url)
	if replacement := f.substitute(url); replacement != "" {
		s.replaceActiveLocked(f, url, replacement)
		return f.notification([]string{url}, []string{replacement}), true
	}
	if len(f.untried) != 0 {
		return nil, false
	}
	s.startup = nil
	if len(f.canaries) == 0 {
		return nil, true
	}
	// Free terminal routes first, then the lowest-ranked failed routes when the
	// link is full. Only the surviving candidates may reconnect after publication.
	drop := len(f.active) + len(f.canaries) - f.limit
	retire := func(url string) {
		s.retireRouteLocked(url)
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
	return f.notification(active, added), true
}

// Replace a failed active official zone while another route is still starting.
// The replacement is frozen into the link only if it was selected before
// admission. Official canaries and server-backoff routes stay reserved for
// standby waves until active routes fail.
func (f *startupFallback) substitute(failed string) string {
	c := f.routes[failed]
	if c.Source != relayplan.Official || c.Canary || c.Zone == "" {
		return ""
	}
	return f.takeStandby(func(other relayplan.Candidate) bool {
		return other.Source == relayplan.Official && !other.Canary && other.ServerRetryAt.IsZero() && other.Zone == c.Zone
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
// community route from publishing. Keep waiting official routes alive for
// recovery while rotating through community standbys whenever a community probe
// fails and every active official route is already blocked or failed.
func (s *Sender) communityDuringAuthorizationLocked() func() {
	f := s.startup
	if f == nil {
		return nil
	}
	var hasOfficialAuthWait bool
	var replace string
	for _, url := range f.active {
		route := s.routes[url]
		if f.routes[url].Source == relayplan.Community {
			if !route.failed() {
				return nil
			}
			// Prefer retiring a terminal probe if multiple community routes are active.
			if replace == "" || route.terminal {
				replace = url
			}
			continue
		}
		if route.waitingAuthorization() {
			hasOfficialAuthWait = true
			continue
		}
		if !route.failed() {
			return nil
		}
	}
	if !hasOfficialAuthWait {
		return nil
	}
	if replace == "" && len(f.active) >= f.limit {
		replace = s.failedOfficialToRetireLocked(f)
		if replace == "" {
			return nil
		}
	}
	next := f.takeStandby(func(c relayplan.Candidate) bool { return c.Source == relayplan.Community })
	if next == "" {
		return nil
	}
	before := slices.Clone(f.active)
	if replace != "" {
		s.replaceActiveLocked(f, replace, next)
	} else {
		f.active = append(f.active, next)
		f.untried[next] = struct{}{}
		s.startLocked(next, f.dial)
	}
	return f.notification(before, []string{next})
}

func (s *Sender) failedOfficialToRetireLocked(f *startupFallback) string {
	for _, url := range f.active {
		if s.routes[url].terminal {
			return url
		}
	}
	for _, url := range slices.Backward(f.active) {
		if s.routes[url].failed() {
			return url
		}
	}
	return ""
}

func (s *Sender) replaceActiveLocked(f *startupFallback, old, next string) {
	s.retireRouteLocked(old)
	delete(f.untried, old)
	f.active[slices.Index(f.active, old)] = next
	f.untried[next] = struct{}{}
	s.startLocked(next, f.dial)
}

func (s *Sender) retireRouteLocked(url string) {
	delete(s.routes, url)
	if cancel := s.cancelRoute[url]; cancel != nil {
		cancel()
		delete(s.cancelRoute, url)
	}
}

func (f *startupFallback) notification(active, added []string) func() {
	if f.observe == nil {
		return nil
	}
	event := Fallback{Active: slices.Clone(active), Next: slices.Clone(added)}
	return func() { f.observe(event) }
}
