package networkruntime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
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

const (
	ownerObserveBatchSize = 2
	ownerObserveTimeout   = 4 * time.Second
)

func (r *Runtime) startCommunity() {
	if r.config.CommunitySettingsPath == "" && len(r.config.BootstrapNodes) == 0 {
		return
	}
	if r.community == nil {
		r.community = community.New(r.config.CommunitySettingsPath, publicdial.Policy{}, r.now, r.diagnostics)
	}
	// Seed the runtime's observed eligibility baseline before background loops
	// start so cold startup stays silent while subsequent in-process writes
	// (initial enrollment, relay removal, or external edits) emit transitions.
	if r.config.CommunitySettingsPath != "" {
		_, _ = r.communitySettings(r.ctx)
	}
	r.communityKick = make(chan struct{}, 1)
	r.communityWorkers.Go(func() {
		tick := time.NewTicker(community.RefreshInterval)
		defer tick.Stop()
		for {
			r.refreshCommunity()
			select {
			case <-r.ctx.Done():
				return
			case <-r.communityKick:
			case <-tick.C:
			}
		}
	})
	if r.config.CommunitySettingsPath == "" {
		return
	}
	// Run owned-relay eligibility observation on an independent loop so offline
	// or blackholed owned relays (e.g. a home LAN node while away) cannot delay
	// community pool refresh or warming when persisted eligibility already allows it.
	r.ownerKick = make(chan struct{}, 1)
	r.communityWorkers.Go(func() {
		tick := time.NewTicker(community.RefreshInterval)
		defer tick.Stop()
		cursor := 0
		for {
			r.observeOwnedRelays(&cursor)
			select {
			case <-r.ctx.Done():
				return
			case <-r.ownerKick:
			case <-tick.C:
			}
		}
	})
}

func (r *Runtime) refreshCommunity() {
	settings, allowed := r.communitySettings(r.ctx)
	r.deliverCommunityNotice(r.ctx)
	if !allowed {
		return
	}
	r.mu.Lock()
	origin := r.communityOrigin
	r.mu.Unlock()
	if origin == "" {
		origin = r.frontend(settings.FrontURL).primary
	}
	r.community.Refresh(r.ctx, r.scope(origin), r.seeds(settings))
}

func (r *Runtime) observeOwnedRelays(cursor *int) {
	settings, _ := r.communitySettings(r.ctx)
	owned := settings.OwnedNative()
	if len(owned) == 0 {
		return
	}
	observed := r.observeOwnerBatch(owned, cursor)
	if len(observed) == 0 || r.ctx.Err() != nil {
		return
	}
	resumed := r.updateEligibility(observed)
	r.deliverCommunityNotice(r.ctx)
	if resumed {
		r.kickCommunity()
	}
}

func (r *Runtime) observeOwnerBatch(owned []relayprofile.Candidate, cursor *int) map[string]serviceauthority.PublicRelay {
	batchSize := min(ownerObserveBatchSize, len(owned))
	start := *cursor
	*cursor += ownerObserveBatchSize
	type result struct {
		key   string
		state serviceauthority.PublicRelay
		ok    bool
	}
	results := make([]result, batchSize)
	if batchSize == 1 {
		p := owned[start%len(owned)]
		state, ok := r.observeOwner(p)
		results[0] = result{key: p.Node.NodeKey, state: state, ok: ok}
	} else {
		var wg sync.WaitGroup
		for i := range batchSize {
			p := owned[(start+i)%len(owned)]
			wg.Go(func() {
				state, ok := r.observeOwner(p)
				results[i] = result{key: p.Node.NodeKey, state: state, ok: ok}
			})
		}
		wg.Wait()
	}
	observed := make(map[string]serviceauthority.PublicRelay, batchSize)
	for _, item := range results {
		if item.ok {
			observed[item.key] = item.state
		}
	}
	return observed
}

func (r *Runtime) kickCommunity() {
	select {
	case r.communityKick <- struct{}{}:
	default:
	}
}

func (r *Runtime) kickOwnerObserve() {
	select {
	case r.ownerKick <- struct{}{}:
	default:
	}
}

// observeShareOrigin keeps background warming aligned with explicit per-share
// frontend overrides while letting default shares continue tracking saved
// settings and built-in LinkHost/BackupEntry failover dynamically.
func (r *Runtime) observeShareOrigin(frontend, savedFront, origin string) {
	if frontend != "" && origin != frontendOrigin(savedFront) {
		r.noteOrigin(origin)
		return
	}
	r.clearExplicitOrigin(origin)
}

func (r *Runtime) noteOrigin(origin string) {
	r.mu.Lock()
	changed := r.communityOrigin != origin
	r.communityOrigin = origin
	r.mu.Unlock()
	if changed {
		r.kickCommunity()
	}
}

func (r *Runtime) clearExplicitOrigin(currentOrigin string) {
	r.mu.Lock()
	previous := r.communityOrigin
	r.communityOrigin = ""
	r.mu.Unlock()
	if previous != "" && previous != currentOrigin {
		r.kickCommunity()
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
	if r.community == nil || r.ctx.Err() != nil {
		return
	}
	if ctx == nil {
		ctx = r.ctx
	}
	// Reset any share-cached origin so preference edits (including front_url)
	// re-derive the warming origin from the updated settings immediately.
	r.mu.Lock()
	r.communityOrigin = ""
	r.mu.Unlock()
	_, _ = r.communitySettings(ctx)
	r.deliverCommunityNotice(ctx)
	r.kickCommunity()
	r.kickOwnerObserve()
}

func (r *Runtime) communitySettings(ctx context.Context) (relayconfig.Config, bool) {
	if r.community == nil {
		return relayconfig.Config{}, false
	}
	path := r.config.CommunitySettingsPath
	if path == "" {
		return relayconfig.Config{}, true
	}
	if ctx == nil {
		ctx = r.ctx
	}
	c, e, err := (relayconfig.FileStore{Path: path}).ReadCommunityState(ctx)
	if err != nil {
		if errors.Is(err, relayconfig.ErrCorruptEligibility) {
			r.recordCommunityStateError("corrupt_eligibility", err)
			if len(c.OwnedNative()) == 0 {
				// Without owned native relays eligibility cannot be paused; do not
				// fail closed on an irrelevant corrupt sidecar file.
				r.recordEligibilityPaused(false, false, false)
				return c, c.CommunityEnabled() && !c.ConfiguredOnly
			}
			// Return the valid settings so observeOwnedRelays can still re-probe
			// owned relays and repair community-eligibility.json on disk.
			return c, false
		}
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			r.recordCommunityStateError("read_failed", err)
		}
		return relayconfig.Config{}, false
	}
	r.clearCommunityStateError()
	r.recordEligibilityPaused(false, e.Paused, false)
	return c, c.CommunityEnabled() && !c.ConfiguredOnly && !e.Paused
}

func (r *Runtime) recordCommunityStateError(reason string, err error) {
	r.mu.Lock()
	already := r.communityStateErr == reason
	r.communityStateErr = reason
	r.mu.Unlock()
	if !already && r.diagnostics.Enabled() {
		r.diagnostics.Emit(diagnostics.New("community_eligibility_read_failed", diagnostics.Fields{
			"reason": diagnostics.Text(reason),
			"error":  diagnostics.Text(err.Error()),
		}).At(r.now()))
	}
}

func (r *Runtime) clearCommunityStateError() {
	r.mu.Lock()
	r.communityStateErr = ""
	r.mu.Unlock()
}

func (r *Runtime) recordEligibilityPaused(previous, current, hasPrevious bool) {
	r.mu.Lock()
	changed := false
	if r.eligibilityKnown {
		changed = r.eligibilityPaused != current
	} else if hasPrevious {
		changed = previous != current
	}
	r.eligibilityKnown = true
	r.eligibilityPaused = current
	r.mu.Unlock()
	if changed && r.diagnostics.Enabled() {
		r.diagnostics.Emit(diagnostics.New("community_eligibility_changed", diagnostics.Fields{"paused": diagnostics.Bool(current)}).At(r.now()))
	}
}

func (r *Runtime) updateEligibility(observed map[string]serviceauthority.PublicRelay) bool {
	resumed := false
	var prevPaused, nextPaused bool
	err := statefile.Transact(r.ctx, r.config.CommunitySettingsPath, func(tx *statefile.Transaction) error {
		c, err := relayconfig.ReadSettings(tx)
		if err != nil {
			return err
		}
		old, err := relayconfig.ReadEligibility(tx)
		corrupt := errors.Is(err, relayconfig.ErrCorruptEligibility)
		if err != nil && !corrupt {
			return err
		}
		wasPaused := corrupt || relayconfig.EvaluateEligibility(old, c, nil).Paused
		e, err := relayconfig.ReconcileEligibility(tx, c, observed)
		if err != nil {
			return err
		}
		prevPaused, nextPaused = old.Paused, e.Paused
		resumed = wasPaused && c.CommunityEnabled() && !c.ConfiguredOnly && !e.Paused
		return nil
	})
	if err != nil {
		return false
	}
	r.clearCommunityStateError()
	r.recordEligibilityPaused(prevPaused, nextPaused, true)
	return resumed
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
	ctx, cancel := context.WithTimeout(r.ctx, ownerObserveTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, false
	}
	client := r.noRedirectHTTPClient()
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

func (r *Runtime) seeds(settings relayconfig.Config) community.Seeds {
	seen := make(map[string]struct{})
	appendUnique := func(dst *[]string, raw string) {
		key := seedOriginKey(raw)
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		*dst = append(*dst, raw)
	}
	var out community.Seeds
	// Explicit user-configured seeds outrank built-in defaults so manual
	// overrides take effect without waiting on stale official fleets.
	for _, seed := range settings.BootstrapNodes {
		appendUnique(&out.Configured, seed)
	}
	var snapshot nodedirectory.Snapshot
	usable := false
	if r.directory != nil {
		snapshot, usable = r.directory.Snapshot()
	}
	if usable {
		for _, n := range snapshot.Payload().Nodes {
			if n.Status == nodedirectory.Active {
				appendUnique(&out.Active, n.Relay)
			}
		}
	}
	for _, seed := range r.config.BootstrapNodes {
		appendUnique(&out.Fallback, seed)
	}
	if !usable && r.directory != nil {
		// Even after trust expires, historical directory addresses remain
		// permissible lowest-priority untrusted seeds.
		for _, n := range snapshot.Payload().Nodes {
			if n.Status == nodedirectory.Active {
				appendUnique(&out.Fallback, n.Relay)
			}
		}
	}
	return out
}

func seedOriginKey(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return raw
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if scheme == "https" && port == "443" {
		port = ""
	}
	if strings.ContainsRune(host, ':') {
		host = "[" + host + "]"
	}
	if port != "" {
		return scheme + "://" + host + ":" + port
	}
	return scheme + "://" + host
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

func frontendOrigin(frontend string) string {
	u, err := url.Parse(frontend)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func (r *Runtime) scope(frontend string) community.Scope {
	return community.Scope{Origin: frontendOrigin(frontend), Generation: r.generation()}
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
