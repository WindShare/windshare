// Package networkruntime assembles directory trust, recent connection facts and
// frontend health without giving library callers implicit infrastructure.
package networkruntime

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/engine/networkconfig"
	"github.com/windshare/windshare/engine/networkdirectory"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
)

var ErrUnavailable = errors.New("built-in network has no usable relay candidates")

type Runtime struct {
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
	go r.run()
	return r, nil
}

func (r *Runtime) Close() {
	r.cancel()
	if r.directory != nil {
		r.directory.Close()
	}
	<-r.done
}

type Selection struct {
	Relays   []relayprofile.Candidate
	Plan     relayplan.Plan
	Frontend string
	Decision networkconfig.Decision
}

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
	var payload nodedirectory.Payload
	if !only && r.directory != nil {
		snapshot, usable := r.directory.Snapshot()
		if !usable && len(configured) == 0 {
			if err := r.directory.Refresh(ctx); err != nil {
				return Selection{}, errors.Join(ErrUnavailable, err)
			}
			snapshot, usable = r.directory.Snapshot()
		}
		if usable {
			payload = snapshot.Payload()
		}
	}
	generation := r.generation()
	plan, err := relayplan.Build(relayplan.Input{Configured: urls, Directory: payload, NetworkGeneration: generation, Now: r.now(),
		Observations: r.observations.Snapshot(generation, r.now()), Policy: relayplan.Policy{ConfiguredOnly: only, GlobalPairCoversMainland: r.config.GlobalPairCoversMainland}})
	if err != nil {
		return Selection{}, err
	}
	if len(plan.Configured)+len(plan.Network) == 0 {
		return Selection{}, errors.Join(ErrUnavailable, r.directoryError)
	}
	result := Selection{Plan: plan, Relays: configured}
	result.Decision = networkconfig.Decision{DirectoryGeneration: plan.Decision.DirectoryGeneration, DirectorySerial: plan.Decision.DirectorySerial,
		NetworkGeneration: generation, ConfiguredOnly: only}
	for _, candidate := range plan.Configured {
		result.Decision.Candidates = append(result.Decision.Candidates, networkconfig.Candidate{URL: candidate.URL, Configured: true})
	}
	for _, candidate := range plan.Network {
		result.Relays = append(result.Relays, relayprofile.Candidate{PublicURL: candidate.URL})
		result.Decision.Candidates = append(result.Decision.Candidates, networkconfig.Candidate{URL: candidate.URL, Alias: candidate.Alias})
	}
	for _, filter := range plan.Decision.Filters {
		result.Decision.Filters = append(result.Decision.Filters, networkconfig.Filter{Endpoint: filter.Endpoint, Reason: filter.Reason})
	}
	result.Frontend, result.Decision.FrontendReason = r.frontend(frontend)
	if result.Frontend != "" {
		if err := relayconfig.ValidateFrontURL(result.Frontend); err != nil {
			return Selection{}, err
		}
	}
	return result, nil
}

func (r *Runtime) generation() string {
	return strconv.FormatUint(r.monitor.Current().GenerationID(), 10)
}
