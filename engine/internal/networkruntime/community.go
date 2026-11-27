package networkruntime

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/windshare/windshare/connectivity/publicdial"
	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/networkendpoint"
	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/core/serviceauthority"
	"github.com/windshare/windshare/engine/internal/community"
	"github.com/windshare/windshare/engine/internal/statefile"
	"github.com/windshare/windshare/engine/networkdirectory"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
)

func (r *Runtime) startCommunity() {
	if r.config.CommunitySettingsPath == "" && len(r.config.BootstrapNodes) == 0 {
		return
	}
	r.community = community.New(r.config.CommunitySettingsPath, publicdial.Policy{}, r.now, r.diagnostics)
	r.communityKick = make(chan struct{}, 1)
	r.communityWorkers.Go(func() {
		tick := time.NewTicker(community.RefreshInterval)
		defer tick.Stop()
		cursor := 0
		for {
			settings, allowed := r.communitySettings()
			r.deliverCommunityNotice(r.ctx)
			if owned := settings.OwnedNative(); len(owned) > 0 {
				observed := map[string]serviceauthority.PublicRelay{}
				for i := 0; i < min(2, len(owned)); i++ {
					p := owned[(cursor+i)%len(owned)]
					if state, ok := r.observeOwner(p); ok {
						observed[p.Node.NodeKey] = state
					}
				}
				cursor += 2
				if len(observed) > 0 {
					allowed = r.updateEligibility(observed)
					r.deliverCommunityNotice(r.ctx)
				}
			}
			if allowed {
				r.mu.Lock()
				origin := r.communityOrigin
				r.mu.Unlock()
				if origin == "" {
					origin = r.frontend(settings.FrontURL).primary
				}
				r.community.Refresh(r.ctx, r.scope(origin), r.seeds(settings))
			}
			select {
			case <-r.ctx.Done():
				return
			case <-r.communityKick:
			case <-tick.C:
			}
		}
	})
}

func (r *Runtime) noteOrigin(origin string) {
	r.mu.Lock()
	changed := r.communityOrigin != origin
	r.communityOrigin = origin
	r.mu.Unlock()
	if changed {
		select {
		case r.communityKick <- struct{}{}:
		default:
		}
	}
}

func (r *Runtime) deliverCommunityNotice(ctx context.Context) {
	if r.config.CommunityPaused == nil || r.config.CommunitySettingsPath == "" {
		return
	}
	store := relayconfig.FileStore{Path: r.config.CommunitySettingsPath}
	if show, err := store.TakeCommunityNotice(ctx); err == nil && show {
		r.config.CommunityPaused()
	}
}

func (r *Runtime) RelaySettingsChanged(ctx context.Context) {
	if r.community == nil {
		return
	}
	r.deliverCommunityNotice(ctx)
	select {
	case r.communityKick <- struct{}{}:
	default:
	}
}

func (r *Runtime) communitySettings() (relayconfig.Config, bool) {
	if r.community == nil {
		return relayconfig.Config{}, false
	}
	path := r.config.CommunitySettingsPath
	if path == "" {
		return relayconfig.Config{}, true
	}
	var c relayconfig.Config
	allowed := false
	err := statefile.Transact(r.ctx, path, func(tx *statefile.Transaction) error {
		var err error
		c, err = relayconfig.ReadSettings(tx)
		if err != nil {
			return err
		}
		e, err := relayconfig.ReconcileEligibility(tx, c, nil)
		allowed = c.CommunityEnabled() && !c.ConfiguredOnly && !e.Paused
		return err
	})
	return c, allowed && err == nil
}

func (r *Runtime) updateEligibility(observed map[string]serviceauthority.PublicRelay) bool {
	allowed := false
	err := statefile.Transact(r.ctx, r.config.CommunitySettingsPath, func(tx *statefile.Transaction) error {
		c, err := relayconfig.ReadSettings(tx)
		if err != nil {
			return err
		}
		old, err := relayconfig.ReadEligibility(tx)
		if err != nil {
			return err
		}
		e, err := relayconfig.ReconcileEligibility(tx, c, observed)
		allowed = c.CommunityEnabled() && !c.ConfiguredOnly && !e.Paused
		if err == nil && old.Paused != e.Paused && r.diagnostics.Enabled() {
			r.diagnostics.Emit(diagnostics.New("community_eligibility_changed", diagnostics.Fields{"paused": diagnostics.Bool(e.Paused)}))
		}
		return err
	})
	return allowed && err == nil
}

func (r *Runtime) observeOwner(profile relayprofile.Candidate) (serviceauthority.PublicRelay, bool) {
	// The owner explicitly configured this endpoint, including private LAN addresses.
	ep, err := networkendpoint.NormalizeRelay(profile.PublicURL)
	if err != nil {
		return 0, false
	}
	u, _ := url.Parse(ep.IdentityURL)
	u.Scheme = map[string]string{"ws": "http", "wss": "https"}[u.Scheme]
	u.Path = "/.well-known/windshare"
	u.RawPath = ""
	ctx, cancel := context.WithTimeout(r.ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, false
	}
	client := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, serviceauthority.MaximumDescriptorBytes+1))
	if err != nil {
		return 0, false
	}
	key, err := serviceauthority.PublicKey(profile.Node.NodeKey)
	if err != nil {
		return 0, false
	}
	d, err := serviceauthority.VerifyDescriptor(data, key, r.now())
	return d.PublicRelay, err == nil && ep.IdentityURL == d.RelayEndpoint && d.Authority == nil
}

func (r *Runtime) seeds(settings relayconfig.Config) []string {
	targets := slices.Clone(settings.BootstrapNodes)
	for _, seed := range r.config.BootstrapNodes {
		if !slices.Contains(targets, seed) {
			targets = append(targets, seed)
		}
	}
	if r.directory != nil {
		// Even after trust expires, an address remains a permissible untrusted seed.
		snapshot, _ := r.directory.Snapshot()
		for _, n := range snapshot.Payload().Nodes {
			if n.Status == nodedirectory.Active && !slices.Contains(targets, n.Relay) {
				targets = append(targets, n.Relay)
			}
		}
	}
	return targets
}

// Empty-cache startup races the two independent discovery paths. Once a plan
// is usable, a slow directory mirror or a stale seed cannot delay publication.
func (r *Runtime) bootstrap(ctx context.Context, scope community.Scope, settings relayconfig.Config, allow bool, build func() (relayplan.Plan, error)) (relayplan.Plan, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, 2)
	pending := 0
	if allow {
		pending++
		go func() { r.community.Refresh(ctx, scope, r.seeds(settings)); results <- nil }()
	}
	if r.directory != nil {
		pending++
		go func() { results <- r.directory.Refresh(ctx) }()
	}
	var last error
	for range pending {
		select {
		case <-ctx.Done():
			return relayplan.Plan{}, ctx.Err()
		case err := <-results:
			if err != nil {
				last = err
			}
			plan, err := build()
			if err != nil || len(plan.Network)+len(plan.Configured) > 0 {
				return plan, err
			}
		}
	}
	if last != nil {
		return relayplan.Plan{}, unavailable(last, networkdirectory.SnapshotUnavailable)
	}
	return build()
}

func (r *Runtime) scope(frontend string) community.Scope {
	u, err := url.Parse(frontend)
	if err != nil || u.Host == "" {
		return community.Scope{Generation: r.generation()}
	}
	return community.Scope{Origin: u.Scheme + "://" + u.Host, Generation: r.generation()}
}

func (r *Runtime) registration(c relayplan.Candidate, s Selection, scope community.Scope) relayprofile.Registration {
	reg := relayprofile.Registration{PublicURL: c.URL}
	switch c.Source {
	case relayplan.Community:
		reg.Community = &relayprofile.CommunityRoute{Client: r.community.Transport(), Origin: scope.Origin,
			Check: func(ctx context.Context) error { return r.community.Verify(ctx, scope, c.Identity) }}
		reg.Community.Ready.Store(c.Verified)
	case relayplan.Official:
		reg.Authorization = s.authorization.Source(c.URL)
	}
	return reg
}
