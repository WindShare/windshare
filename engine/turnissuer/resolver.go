// Package turnissuer resolves the immutable TURN configuration selected by a share.
package turnissuer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	d "github.com/windshare/windshare/core/diagnostics"

	"github.com/windshare/windshare/connectivity/turnsession"
	p "github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

const issueTimeout = 5 * time.Second
const discoveryTimeout = 3 * time.Second

type Options struct {
	HTTP   *http.Client
	Now    func() time.Time
	Random io.Reader
}
type source struct {
	relayconfig.TURNService
	origin, access string
}
type Resolver struct {
	options     Options
	sources     []source
	mu          sync.Mutex
	randomMu    sync.Mutex
	known       map[string]bool
	discovering chan struct{}
}

func New(manual []relayconfig.TURNService, selected []relayprofile.Registration, profiles []relayprofile.Candidate, options Options) (*Resolver, error) {
	if err := relayconfig.ValidateTURNServices(manual); err != nil {
		return nil, err
	}
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
	r := &Resolver{options: options, known: make(map[string]bool)}
	ids := map[string]bool{}
	for _, s := range relayconfig.CloneTURNServices(manual) {
		r.sources = append(r.sources, source{TURNService: s})
		ids[s.ID] = true
	}
	byURL := make(map[string]relayprofile.Candidate)
	for _, s := range profiles {
		u, err := v2.NormalizeRelayEndpoint(s.PublicURL)
		if err == nil {
			byURL[u.DialURL] = s
		}
	}
	for _, active := range selected {
		u, err := v2.NormalizeRelayEndpoint(active.PublicURL)
		if err != nil {
			continue
		}
		s := byURL[u.DialURL]
		origin, ok := managedOrigin(s.PublicURL)
		if !ok || s.AccessKey == "" {
			continue
		}
		digest := sha256.Sum256([]byte(u.DialURL))
		id := "managed-" + hex.EncodeToString(digest[:12])
		for ids[id] {
			id += "-"
		}
		ids[id] = true
		r.sources = append(r.sources, source{TURNService: relayconfig.TURNService{ID: id, Kind: "managed", Role: relayconfig.RoleFallback}, origin: origin, access: s.AccessKey})
		if s.ManagedTurn {
			r.known[id] = true
		}
	}
	return r, nil
}

// Discovery never delays publication. Concurrent sessions share just the
// capability result, while issued credentials stay scoped to their own session.
func (r *Resolver) Discover(ctx context.Context, trace turnsession.Trace) <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.discovering != nil {
		return r.discovering
	}
	done := make(chan struct{})
	var pending []source
	for _, s := range r.sources {
		if _, known := r.known[s.ID]; s.origin != "" && !known {
			pending = append(pending, s)
		}
	}
	if len(pending) == 0 {
		close(done)
		return done
	}
	r.discovering = done
	go func() {
		var wg sync.WaitGroup
		for _, s := range pending {
			wg.Go(func() {
				probe, cancel := context.WithTimeout(ctx, discoveryTimeout)
				defer cancel()
				var capability struct {
					Managed bool `json:"managed_turn"`
				}
				if err := r.request(probe, http.MethodGet, s.origin+"/.well-known/windshare", "", nil, &capability); err == nil {
					r.mu.Lock()
					r.known[s.ID] = capability.Managed
					r.mu.Unlock()
					trace.Event("turn_capability_discovered", s.ID, d.Fields{"enabled": d.Bool(capability.Managed)})
				}
			})
		}
		wg.Wait()
		r.mu.Lock()
		r.discovering = nil
		close(done)
		r.mu.Unlock()
	}()
	return done
}

func (r *Resolver) Issue(ctx context.Context, request turnsession.IssueRequest) turnsession.IssueResult {
	ctx, cancel := context.WithTimeout(ctx, issueTimeout)
	defer cancel()
	available, unknown := r.availability()
	var discovered <-chan struct{}
	if request.Mode == p.TURNAll && len(unknown) > 0 {
		discovered = r.Discover(ctx, request.Trace)
	}
	var result turnsession.IssueResult
	var targets []source
	for _, s := range activeServices(available, request.ServiceIDs) {
		if s.EffectiveRole() == relayconfig.RoleFallback && request.Mode == p.TURNParallel {
			continue
		}
		if request.Reusable[s.ID] {
			result.Retained = append(result.Retained, s.ID)
			continue
		}
		targets = append(targets, s)
	}
	issued := make([]turnsession.Credential, len(targets))
	var wg sync.WaitGroup
	for i, s := range targets {
		wg.Go(func() {
			// A missing capability may use the same bounded request window, but
			// must not shorten another service's opportunity to issue a credential.
			if unknown[s.ID] && !r.awaitDiscovery(ctx, s.ID, discovered) {
				return
			}
			credential, err := r.issue(ctx, s, request.Trace)
			if err != nil {
				request.Trace.Event("turn_grant_failed", s.ID, d.Fields{"kind": d.Text(s.Kind), "role": d.Text(s.EffectiveRole()), "reason": d.Text("issuer_unavailable")})
				return
			}
			issued[i] = credential
		})
	}
	wg.Wait()
	for _, c := range issued {
		if c.ServiceID != "" {
			result.Credentials = append(result.Credentials, c)
		}
	}
	available, unknown = r.availability()
	result.HasFallback = slices.ContainsFunc(available, func(s source) bool { return !unknown[s.ID] && s.EffectiveRole() == relayconfig.RoleFallback })
	switch {
	case len(result.Credentials)+len(result.Retained) > 0:
		result.Status = p.TURNOK
	case len(available) == 0:
		result.Status = p.TURNUnconfigured
	case len(unknown) > 0 || len(targets) > 0:
		result.Status = p.TURNRetryLater
	default:
		result.Status = p.TURNOK
	}
	return result
}

func (r *Resolver) awaitDiscovery(ctx context.Context, id string, discovered <-chan struct{}) bool {
	select {
	case <-ctx.Done():
		return false
	case <-discovered:
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.known[id]
	}
}

func (r *Resolver) availability() ([]source, map[string]bool) {
	r.mu.Lock()
	var available []source
	unknown := make(map[string]bool)
	for _, s := range r.sources {
		if s.origin != "" {
			active, known := r.known[s.ID]
			if !known {
				unknown[s.ID] = true
			}
			if known && !active {
				continue
			}
		}
		available = append(available, s)
	}
	r.mu.Unlock()
	return available, unknown
}

// Reserve room for both configured roles before filtering parallel demand.
// Escalation can then add the fallback without displacing a busy parallel lane.
func activeServices(available []source, ids []string) []source {
	if len(ids) > 0 {
		return slices.DeleteFunc(available, func(s source) bool { return !slices.Contains(ids, s.ID) })
	}
	if len(available) <= p.MaxGrantsPerMessage {
		return available
	}
	selected := available[:p.MaxGrantsPerMessage]
	role := selected[0].EffectiveRole()
	if !slices.ContainsFunc(selected, func(s source) bool { return s.EffectiveRole() != role }) {
		for _, s := range available[p.MaxGrantsPerMessage:] {
			if s.EffectiveRole() != role {
				selected[len(selected)-1] = s
				break
			}
		}
	}
	return selected
}
