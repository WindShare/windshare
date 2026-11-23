package networkruntime

import (
	"context"
	"slices"
	"sync"

	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/networkendpoint"
	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/engine/networkconfig"
	"github.com/windshare/windshare/engine/networkdirectory"
)

// PublishRelayHints retains pending registrations so a healthy relay can publish
// immediately. An initial registration that has already ended cannot serve this share.
func (selection Selection) PublishRelayHints(capability *link.Link, selected []string) []networkconfig.Candidate {
	capability.Relays, capability.NodeAliases = nil, nil
	var published []networkconfig.Candidate
	for _, group := range [][]relayplan.Candidate{selection.Plan.Configured, selection.Plan.Network, selection.Plan.Standby} {
		for _, candidate := range group {
			if !slices.Contains(selected, candidate.URL) {
				continue
			}
			published = append(published, decisionCandidate(candidate))
			if candidate.Alias != "" && selection.useAliases {
				capability.NodeAliases = append(capability.NodeAliases, candidate.Alias)
			} else {
				capability.Relays = append(capability.Relays, candidate.URL)
			}
		}
	}
	return published
}

func decisionCandidate(candidate relayplan.Candidate) networkconfig.Candidate {
	return networkconfig.Candidate{URL: candidate.URL, Alias: candidate.Alias, Configured: candidate.Configured,
		CooldownUntil: candidate.CooldownUntil, ServerRetryAt: candidate.ServerRetryAt}
}

// Resolve returns immediately usable endpoints and optional bounded expansion.
// The relay race owns expansion so known paths never wait for an unknown alias.
func (r *Runtime) Resolve(capability link.Link) ([]string, func(context.Context) ([]string, error), error) {
	if len(capability.Relays)+len(capability.NodeAliases) > link.MaximumRelayHints {
		return nil, nil, link.ErrMalformedLink
	}
	known := slices.Clone(capability.Relays)
	var unknown []string
	var snapshot nodedirectory.Snapshot
	usable := false
	if r.directory != nil {
		snapshot, usable = r.directory.Snapshot()
	}
	for _, alias := range capability.NodeAliases {
		if !nodedirectory.ValidAlias(alias) {
			return nil, nil, link.ErrMalformedLink
		}
		if endpoint, found := snapshot.Resolve(alias); usable && found {
			known = append(known, endpoint)
		} else {
			unknown = append(unknown, alias)
		}
	}
	known, err := uniqueEndpoints(known)
	if err != nil {
		return nil, nil, err
	}
	if len(unknown) == 0 {
		return known, nil, nil
	}
	if r.directory == nil {
		failure := r.noCandidates(AliasUnavailable)
		if len(known) > 0 {
			return known, func(context.Context) ([]string, error) { return nil, failure }, nil
		}
		return nil, nil, failure
	}
	var once sync.Once
	var expanded []string
	var failure error
	expand := func(ctx context.Context) ([]string, error) {
		once.Do(func() { expanded, failure = r.refreshAliases(ctx, unknown) })
		return slices.Clone(expanded), failure
	}
	return known, expand, nil
}

func (r *Runtime) refreshAliases(ctx context.Context, aliases []string) ([]string, error) {
	if err := r.directory.RefreshAliases(ctx, aliases); err != nil {
		return nil, unavailable(err, networkdirectory.SnapshotUnavailable)
	}
	snapshot, usable := r.directory.Snapshot()
	if !usable {
		return nil, r.noCandidates(AliasUnavailable)
	}
	var expanded []string
	unresolved := false
	for _, alias := range aliases {
		if endpoint, found := snapshot.Resolve(alias); found {
			expanded = append(expanded, endpoint)
		} else {
			unresolved = true
		}
	}
	endpoints, err := uniqueEndpoints(expanded)
	if err == nil && unresolved {
		err = r.noCandidates(AliasUnavailable)
	}
	return endpoints, err
}

func uniqueEndpoints(endpoints []string) ([]string, error) {
	seen := map[string]bool{}
	var result []string
	for _, value := range endpoints {
		endpoint, err := networkendpoint.NormalizeRelay(value)
		if err != nil {
			return nil, err
		}
		if !seen[endpoint.IdentityURL] {
			seen[endpoint.IdentityURL] = true
			result = append(result, value)
		}
	}
	return result, nil
}
