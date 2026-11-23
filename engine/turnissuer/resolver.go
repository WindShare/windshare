// Package turnissuer resolves the immutable TURN configuration selected by a share.
package turnissuer

import (
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	d "github.com/windshare/windshare/core/diagnostics"
	p "github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/turngrant"
	"github.com/windshare/windshare/engine/relayconfig"
)

const issueTimeout = 5 * time.Second
const discoveryTimeout = 3 * time.Second

type Options struct {
	HTTP   *http.Client
	Now    func() time.Time
	Random io.Reader
	Trace  turngrant.Trace
}

type Resolver struct {
	options     Options
	sources     []source
	lifetime    context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	randomMu    sync.Mutex
	known       map[string]bool
	discovering map[string]chan struct{}
	probes      sync.WaitGroup
}

func (sources Sources) Open(owner context.Context, options Options) *Resolver {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Random == nil {
		options.Random = rand.Reader
	}
	var client http.Client
	if options.HTTP != nil {
		client = *options.HTTP
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	options.HTTP = &client
	ctx, cancel := context.WithCancel(owner)
	r := &Resolver{options: options, sources: sources.ordered, lifetime: ctx, cancel: cancel, known: make(map[string]bool), discovering: make(map[string]chan struct{})}
	for _, s := range r.sources {
		if s.advertised {
			r.known[s.ID] = true
		}
	}
	for _, s := range r.sources {
		if s.origin != "" && !s.advertised {
			r.discover(s)
		}
	}
	return r
}

func (r *Resolver) Close() {
	r.mu.Lock()
	r.cancel()
	r.mu.Unlock()
	r.probes.Wait()
}

// A probe belongs to the share. An issuing session can abandon its wait while
// other sessions still need the same endpoint's result.
func (r *Resolver) discover(s source) <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, known := r.known[s.ID]; known || r.lifetime.Err() != nil {
		return nil
	}
	if done := r.discovering[s.ID]; done != nil {
		return done
	}
	done := make(chan struct{})
	r.discovering[s.ID] = done
	r.probes.Go(func() {
		ctx, cancel := context.WithTimeout(r.lifetime, discoveryTimeout)
		defer cancel()
		var capability struct {
			Managed bool `json:"managed_turn"`
		}
		err := r.request(ctx, http.MethodGet, s.origin+"/.well-known/windshare", "", nil, &capability)
		if err == nil {
			r.options.Trace.Event("turn_capability_discovered", s.ID, d.Fields{"enabled": d.Bool(capability.Managed)})
		}
		r.mu.Lock()
		if err == nil {
			r.known[s.ID] = capability.Managed
		}
		delete(r.discovering, s.ID)
		close(done)
		r.mu.Unlock()
	})
	return done
}

type outcome struct {
	target     source
	retained   bool
	credential turngrant.Credential
}

func (r *Resolver) Issue(ctx context.Context, request turngrant.IssueRequest) turngrant.IssueResult {
	ctx, cancel := context.WithTimeout(ctx, issueTimeout)
	defer cancel()
	available, unknown := r.availability()
	targets := activeServices(available, unknown, request)
	ready := readyTargets(request, targets, unknown)
	issued := make([]outcome, len(targets))
	count := 0
	var wg sync.WaitGroup
	issue := func(target source, needsProbe bool) {
		slot := &issued[count]
		count++
		if request.Reusable[target.ID] {
			*slot = outcome{target: target, retained: true}
			return
		}
		wg.Go(func() {
			*slot = r.issueSelected(ctx, request, target, needsProbe)
		})
	}
	for _, target := range ready {
		issue(target, unknown[target.ID])
	}
	missingFallback := false
	if len(ready) < len(targets) {
		for _, probe := range r.probeUnknown(available, unknown) {
			if r.awaitProbe(ctx, probe.target, probe.done) {
				issue(probe.target, false)
				if count == len(targets) {
					break
				}
			}
		}
		if count == len(ready) && !unknown[targets[count].ID] {
			// The last confirmed parallel service remains usable if no fallback was found.
			issue(targets[count], false)
			missingFallback = true
		}
	}
	wg.Wait()
	available, unknown = r.availability()
	return summarizeIssuance(request, issued[:count], available, unknown, missingFallback)
}

func (r *Resolver) issueSelected(ctx context.Context, request turngrant.IssueRequest, target source, needsProbe bool) outcome {
	if needsProbe && !r.awaitDiscovery(ctx, target) {
		return outcome{target: target}
	}
	credential, err := r.issue(ctx, target, request.Trace)
	if err != nil {
		request.Trace.Event("turn_grant_failed", target.ID, d.Fields{"kind": d.Text(target.Kind), "role": d.Text(target.EffectiveRole()), "reason": d.Text("issuer_unavailable")})
		return outcome{target: target}
	}
	return outcome{target: target, credential: credential}
}

func summarizeIssuance(request turngrant.IssueRequest, issued []outcome, available []source, unknown map[string]bool, missingFallback bool) turngrant.IssueResult {
	var result turngrant.IssueResult
	result.HasFallback = slices.ContainsFunc(available, func(s source) bool { return !unknown[s.ID] && s.EffectiveRole() == relayconfig.RoleFallback })
	// A transient discovery failure leaves the fourth parallel grant usable.
	// A receiver that still has room can retry the missing fallback role.
	retry := missingFallback && (len(unknown) != 0 || result.HasFallback)
	for _, item := range issued {
		switch {
		case item.retained:
			result.Retained = append(result.Retained, item.target.ID)
		case item.credential.ServiceID != "":
			result.Credentials = append(result.Credentials, item.credential)
		case slices.ContainsFunc(available, func(s source) bool { return s.ID == item.target.ID }):
			// Discovery and managed issuance can permanently disable a target.
			// Only services still eligible warrant another request.
			retry = true
		}
	}
	// A disabled managed endpoint can uncover a previously unselected service.
	// Return its cached grant immediately, or issue it on the next bounded request.
	for _, s := range activeServices(available, unknown, request) {
		if slices.ContainsFunc(issued, func(item outcome) bool { return item.target.ID == s.ID }) {
			continue
		}
		if request.Reusable[s.ID] && len(result.Retained)+len(result.Credentials) < p.MaxGrantsPerMessage {
			result.Retained = append(result.Retained, s.ID)
		} else {
			retry = true
		}
	}
	switch {
	case retry:
		result.Status = p.TURNRetryStatus(len(result.Retained) + len(result.Credentials))
	case len(result.Credentials)+len(result.Retained) > 0:
		result.Status = p.TURNOK
	case len(available) == 0:
		result.Status = p.TURNUnconfigured
	case len(unknown) == len(available):
		result.Status = p.TURNRetryLater
	default:
		result.Status = p.TURNOK
	}
	return result
}

type capabilityProbe struct {
	target source
	done   <-chan struct{}
}

func (r *Resolver) probeUnknown(available []source, unknown map[string]bool) []capabilityProbe {
	// A share can select more private relays than a session can use as TURN
	// paths. Check all eligible alternatives, but issue only for open places.
	probes := make([]capabilityProbe, 0, len(unknown))
	for _, s := range available {
		if unknown[s.ID] {
			probes = append(probes, capabilityProbe{s, r.discover(s)})
		}
	}
	return probes
}

func (r *Resolver) awaitDiscovery(ctx context.Context, s source) bool {
	return r.awaitProbe(ctx, s, r.discover(s))
}

func (r *Resolver) awaitProbe(ctx context.Context, s source, done <-chan struct{}) bool {
	if done != nil {
		select {
		case <-ctx.Done():
			return false
		case <-done:
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.known[s.ID]
}

func (r *Resolver) availability() ([]source, map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var confirmed, pending []source
	unknown := make(map[string]bool)
	for _, s := range r.sources {
		if s.origin != "" {
			enabled, known := r.known[s.ID]
			if !known {
				unknown[s.ID] = true
				pending = append(pending, s)
				continue
			}
			if !enabled {
				continue
			}
		}
		confirmed = append(confirmed, s)
	}
	return append(confirmed, pending...), unknown
}
