// Package networkruntime assembles directory trust, recent connection facts and
// frontend health without giving library callers implicit infrastructure.
package networkruntime

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/core/serviceauthority"
	"github.com/windshare/windshare/engine/account"
	"github.com/windshare/windshare/engine/internal/community"
	"github.com/windshare/windshare/engine/networkconfig"
	"github.com/windshare/windshare/engine/networkdirectory"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

type Runtime struct {
	community         *community.Pool
	communityKick     chan struct{}
	ownerKick         chan struct{}
	communityOrigin   string // Explicit per-share origin override; empty follows settings or built-in frontend.
	communityWorkers  sync.WaitGroup
	accounts          *account.Service
	config            networkconfig.Config
	directory         *networkdirectory.Directory
	directoryError    error
	diagnostics       diagnostics.Sink
	now               func() time.Time
	httpTransport     http.RoundTripper // Test seam for owner/frontend probes; nil uses http.DefaultTransport.
	watcher           *networkstate.Watcher
	ownWatcher        bool
	health            nodeHealth
	selectionSeed     string
	mu                sync.Mutex
	eligibilityKnown  bool
	eligibilityPaused bool
	communityStateErr string
	linkUnreachable   bool
	backupReachable   bool
	ctx               context.Context
	cancel            context.CancelFunc
	done              chan struct{}
}

func New(config *networkconfig.Config, now func() time.Time, selectionSeed string, sink diagnostics.Sink, watcher *networkstate.Watcher) (*Runtime, error) {
	ctx, cancel := context.WithCancel(context.Background())
	ownWatcher := watcher == nil
	if watcher == nil {
		watcher = networkstate.NewWatcher(networkstate.WatcherConfig{Now: now})
	}
	r := &Runtime{diagnostics: sink, now: now, selectionSeed: selectionSeed, watcher: watcher, ownWatcher: ownWatcher, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	if config == nil {
		close(r.done)
		return r, nil
	}
	r.config = *config
	cleanup := func() {
		cancel()
		if r.directory != nil {
			r.directory.Close()
		}
		if ownWatcher {
			watcher.Close()
		}
	}
	for _, frontend := range []string{config.LinkHost, config.BackupEntry} {
		if frontend != "" {
			if err := relayconfig.ValidateFrontURL(frontend); err != nil {
				cleanup()
				return nil, err
			}
		}
	}
	directoryConfig := config.Directory
	directoryConfig.Now = now
	directoryConfig.Diagnostics = sink
	var err error
	r.directory, err = networkdirectory.New(directoryConfig)
	if err != nil {
		// Corrupt or inaccessible trust state must fail closed for directory use,
		// without taking the user's independently configured relays down with it.
		r.directoryError = err
		r.emitDirectoryInitFailure(directoryConfig.Network, err)
	}
	if config.Authority != nil {
		authority := *config.Authority
		authority.Now, authority.ResolveNode, authority.Diagnostics = now, r.officialNode, sink
		r.accounts, err = account.New(authority)
		if err != nil {
			cleanup()
			return nil, err
		}
	}
	r.startCommunity()
	changes, unsubscribe := r.watcher.Subscribe()
	go r.run(changes, unsubscribe)
	return r, nil
}

func (r *Runtime) Close() {
	r.cancel()
	r.communityWorkers.Wait()
	if r.community != nil {
		r.community.Close()
	}
	r.accounts.Close()
	if r.directory != nil {
		r.directory.Close()
	}
	<-r.done
	if r.ownWatcher {
		r.watcher.Close()
	}
}

type Selection struct {
	Relays            []relayprofile.Registration
	Standby           []relayprofile.Registration
	authorization     *account.ShareAuthorization
	Plan              relayplan.Plan
	Frontend          string
	AlternateFrontend string
	useAliases        bool
	Decision          networkconfig.Decision
}

func (selection Selection) ReleaseAuthorization() { selection.authorization.Close() }

func (r *Runtime) Plan(ctx context.Context, intent relayprofile.Intent, frontend string) (Selection, error) {
	if err := intent.Validate(); err != nil {
		return Selection{}, err
	}
	configured := intent.Candidates()
	var urls []string
	for _, candidate := range configured {
		urls = append(urls, candidate.PublicURL)
	}
	only := intent.Mode() != relayprofile.Planned
	front := r.frontend(frontend)
	if front.primary != "" {
		// Validate before deriving community scope or bootstrapping so malformed
		// explicit overrides cannot latch r.communityOrigin or wake background probes.
		if err := relayconfig.ValidateFrontURL(front.primary); err != nil {
			return Selection{}, err
		}
	}
	scope := r.scope(front.primary)
	var allowed bool
	var settings relayconfig.Config
	if !only && intent.CommunityAllowed() && scope.Origin != "" {
		settings, allowed = r.communitySettings(ctx)
		if err := ctx.Err(); err != nil {
			return Selection{}, err
		}
	}
	build := func() (relayplan.Plan, error) { return r.planFor(urls, only, scope, allowed) }
	plan, err := build()
	if allowed {
		r.observeShareOrigin(frontend, settings.FrontURL, scope.Origin)
	}
	if err == nil && !only && len(plan.Configured)+len(plan.Network) == 0 {
		plan, err = r.bootstrap(ctx, scope, settings, allowed, build)
	}
	if err != nil {
		return Selection{}, err
	}
	if len(plan.Configured)+len(plan.Network) == 0 {
		return Selection{}, r.noCandidates(NoRelayCandidates)
	}
	if plan.Decision.CooldownFallback && r.directory != nil {
		r.directory.TriggerRefresh()
	}
	result := Selection{Plan: plan, Relays: relayprofile.Registrations(configured)}
	result.Decision = networkconfig.Decision{DirectoryGeneration: plan.Decision.DirectoryGeneration, DirectorySerial: plan.Decision.DirectorySerial,
		NetworkGeneration: plan.Decision.NetworkGeneration, ConfiguredOnly: only, CooldownFallback: plan.Decision.CooldownFallback}
	result.Frontend, result.AlternateFrontend, result.Decision.FrontendReason = front.primary, front.alternate, front.reason
	// Only these release frontends are built against this directory's trust roots.
	result.useAliases = result.Frontend != "" && (result.Frontend == r.config.LinkHost || result.Frontend == r.config.BackupEntry)
	for _, candidate := range plan.Configured {
		result.Decision.Candidates = append(result.Decision.Candidates, decisionCandidate(candidate))
	}
	if len(plan.DirectoryCandidates()) != 0 {
		if len(configured) > 0 || slices.ContainsFunc(plan.NetworkCandidates(), func(c relayplan.Candidate) bool { return c.Source == relayplan.Community }) {
			result.authorization = r.accounts.PrepareShareAuthorization(ctx)
		} else {
			result.authorization = r.accounts.NewShareAuthorization(ctx)
		}
	}
	for _, candidate := range plan.Network {
		result.Relays = append(result.Relays, r.registration(candidate, result, scope))
		result.Decision.Candidates = append(result.Decision.Candidates, decisionCandidate(candidate))
	}
	for _, candidate := range plan.Standby {
		result.Standby = append(result.Standby, r.registration(candidate, result, scope))
	}
	for _, filter := range plan.Decision.Filters {
		result.Decision.Filters = append(result.Decision.Filters, networkconfig.Filter{Endpoint: filter.Endpoint, Reason: filter.Reason})
	}
	return result, nil
}

func (r *Runtime) buildPlan(configured []string, only bool) (relayplan.Plan, error) {
	settings, allowed := r.communitySettings(r.ctx)
	front := r.frontend(settings.FrontURL).primary
	return r.planFor(configured, only, r.scope(front), allowed)
}

func (r *Runtime) planFor(configured []string, only bool, scope community.Scope, allowed bool) (relayplan.Plan, error) {
	var peers []relayplan.Candidate
	if allowed && !only && scope.Origin != "" {
		peers = r.community.Candidates(scope)
	}
	var payload nodedirectory.Payload
	if !only && r.directory != nil {
		if snapshot, usable := r.directory.Snapshot(); usable {
			payload = snapshot.Payload()
		}
	}
	generation, now := r.generation(), r.now()
	return relayplan.Build(relayplan.Input{Community: peers, Configured: configured, Directory: payload, NetworkGeneration: strconv.FormatUint(generation, 10), Now: now,
		SelectionSeed: r.selectionSeed, Observations: r.health.Snapshot(generation, now), Policy: relayplan.Policy{ConfiguredOnly: only, GlobalPairCoversMainland: r.config.GlobalPairCoversMainland}})
}

func (r *Runtime) Accounts() *account.Service { return r.accounts }

func (r *Runtime) officialNode(address string) (account.Node, bool) {
	if r.directory == nil {
		return account.Node{}, false
	}
	snapshot, usable := r.directory.Snapshot()
	if !usable {
		return account.Node{}, false
	}
	endpoint, err := v2.NormalizeRelayEndpoint(address)
	if err != nil {
		return account.Node{}, false
	}
	for _, node := range snapshot.Payload().Nodes {
		other, err := v2.NormalizeRelayEndpoint(node.Relay)
		if err == nil && endpoint.Identity == other.Identity && node.Status != nodedirectory.Draining && node.HasRole("relay") {
			key, err := serviceauthority.PublicKey(node.NodeKey)
			return account.Node{Key: key}, err == nil
		}
	}
	return account.Node{}, false
}

func (r *Runtime) generation() uint64 {
	// The first snapshot describes the same network as a cold-start handshake.
	return max(1, r.watcher.Current().GenerationID())
}

// noRedirectHTTPClient enforces the non-redirect probe policy in one place
// while allowing package tests to substitute a transport without widening
// networkconfig.Config or disabling private LAN access for owned relays.
func (r *Runtime) noRedirectHTTPClient() http.Client {
	return http.Client{
		Transport:     r.httpTransport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
