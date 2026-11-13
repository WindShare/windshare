package networkruntime

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/networkendpoint"
	"github.com/windshare/windshare/core/nodedirectory"
)

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
		if len(known) > 0 {
			return known, nil, nil
		}
		return nil, nil, ErrUnavailable
	}
	var once sync.Once
	var expanded []string
	var failure error
	expand := func(ctx context.Context) ([]string, error) {
		once.Do(func() {
			if err := r.directory.Refresh(ctx); err != nil {
				failure = errors.Join(ErrUnavailable, err)
				return
			}
			snapshot, usable := r.directory.Snapshot()
			if !usable {
				failure = ErrUnavailable
				return
			}
			for _, alias := range unknown {
				if endpoint, found := snapshot.Resolve(alias); found {
					expanded = append(expanded, endpoint)
				}
			}
			expanded, failure = uniqueEndpoints(expanded)
			if len(expanded) == 0 {
				failure = ErrUnavailable
			}
		})
		return slices.Clone(expanded), failure
	}
	return known, expand, nil
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
