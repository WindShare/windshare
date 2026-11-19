// Package networkruntime assembles directory trust, recent connection facts and
// frontend health without giving library callers implicit infrastructure.
package networkruntime

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/core/serviceauthority"
	"github.com/windshare/windshare/engine/account"
	"github.com/windshare/windshare/engine/networkconfig"
	"github.com/windshare/windshare/engine/networkdirectory"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

type Runtime struct {
	accounts        *account.Service
	config          networkconfig.Config
	directory       *networkdirectory.Directory
	directoryError  error
	now             func() time.Time
	monitor         *networkstate.Monitor
	observations    relayplan.Observations
	mu              sync.Mutex
	linkUnreachable bool
	backupReachable bool
	ctx             context.Context
	cancel          context.CancelFunc
	done            chan struct{}
}

func New(config *networkconfig.Config, now func() time.Time) (*Runtime, error) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &Runtime{now: now, monitor: networkstate.NewMonitor(nil, networkstate.DefaultDebounce), ctx: ctx, cancel: cancel, done: make(chan struct{})}
	if config == nil {
		close(r.done)
		return r, nil
	}
	r.config = *config
	for _, frontend := range []string{config.LinkHost, config.BackupEntry} {
		if frontend != "" {
			if err := relayconfig.ValidateFrontURL(frontend); err != nil {
				cancel()
				return nil, err
			}
		}
	}
	directoryConfig := config.Directory
	directoryConfig.Now = now
	var err error
	r.directory, err = networkdirectory.New(directoryConfig)
	if err != nil {
		// Corrupt or inaccessible trust state must fail closed for directory use,
		// without taking the user's independently configured relays down with it.
		r.directoryError = err
		slog.Warn("network_directory_initialization_failed", "error", err)
	}
	if config.Authority != nil {
		authority := *config.Authority
		authority.Now, authority.ResolveNode = now, r.officialNode
		r.accounts, err = account.New(authority)
		if err != nil {
			cancel()
			if r.directory != nil {
				r.directory.Close()
			}
			return nil, err
		}
	}
	go r.run()
	return r, nil
}

func (r *Runtime) Close() {
	r.cancel()
	r.accounts.Close()
	if r.directory != nil {
		r.directory.Close()
	}
	<-r.done
}

type Selection struct {
	Relays        []relayprofile.Registration
	authorization *account.ShareAuthorization
	Plan          relayplan.Plan
	Frontend      string
	useAliases    bool
	Decision      networkconfig.Decision
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
	generation := r.generation()
	plan, err := r.buildPlan(urls, only, generation)
	if err == nil && len(plan.Configured)+len(plan.Network) == 0 && !only && r.directory != nil {
		if err := r.directory.Refresh(ctx); err != nil {
			return Selection{}, unavailable(err, networkdirectory.SnapshotUnavailable)
		}
		plan, err = r.buildPlan(urls, only, generation)
	}
	if err != nil {
		return Selection{}, err
	}
	if len(plan.Configured)+len(plan.Network) == 0 {
		return Selection{}, r.noCandidates(NoRelayCandidates)
	}
	result := Selection{Plan: plan, Relays: relayprofile.Registrations(configured)}
	result.Decision = networkconfig.Decision{DirectoryGeneration: plan.Decision.DirectoryGeneration, DirectorySerial: plan.Decision.DirectorySerial,
		NetworkGeneration: generation, ConfiguredOnly: only}
	result.Frontend, result.Decision.FrontendReason = r.frontend(frontend)
	// Only these release frontends are built against this directory's trust roots.
	result.useAliases = result.Frontend != "" && (result.Frontend == r.config.LinkHost || result.Frontend == r.config.BackupEntry)
	if result.Frontend != "" {
		if err := relayconfig.ValidateFrontURL(result.Frontend); err != nil {
			return Selection{}, err
		}
	}
	for _, candidate := range plan.Configured {
		result.Decision.Candidates = append(result.Decision.Candidates, networkconfig.Candidate{URL: candidate.URL, Configured: true})
	}
	if len(plan.Network) != 0 {
		result.authorization = r.accounts.NewShareAuthorization(ctx)
	}
	for _, candidate := range plan.Network {
		result.Relays = append(result.Relays, relayprofile.Registration{PublicURL: candidate.URL, Authorization: result.authorization.Source(candidate.URL)})
		result.Decision.Candidates = append(result.Decision.Candidates, networkconfig.Candidate{URL: candidate.URL, Alias: candidate.Alias})
	}
	for _, filter := range plan.Decision.Filters {
		result.Decision.Filters = append(result.Decision.Filters, networkconfig.Filter{Endpoint: filter.Endpoint, Reason: filter.Reason})
	}
	return result, nil
}

func (r *Runtime) buildPlan(configured []string, only bool, generation string) (relayplan.Plan, error) {
	var payload nodedirectory.Payload
	if !only && r.directory != nil {
		if snapshot, usable := r.directory.Snapshot(); usable {
			payload = snapshot.Payload()
		}
	}
	return relayplan.Build(relayplan.Input{Configured: configured, Directory: payload, NetworkGeneration: generation, Now: r.now(),
		Observations: r.observations.Snapshot(generation, r.now()), Policy: relayplan.Policy{ConfiguredOnly: only, GlobalPairCoversMainland: r.config.GlobalPairCoversMainland}})
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

func (r *Runtime) generation() string {
	return strconv.FormatUint(r.monitor.Current().GenerationID(), 10)
}
