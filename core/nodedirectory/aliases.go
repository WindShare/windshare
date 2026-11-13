package nodedirectory

import (
	"maps"

	"github.com/windshare/windshare/core/networkendpoint"
)

// RetainAliases carries tombstones across removals and signing generations.
// A familiar alias must never silently select a different relay authority.
func RetainAliases(previous map[string]string, snapshot Snapshot) (map[string]string, error) {
	next := maps.Clone(previous)
	if next == nil {
		next = make(map[string]string)
	}
	for _, node := range snapshot.payload.Nodes {
		endpoint, err := networkendpoint.NormalizeRelay(node.Relay)
		if err != nil {
			return nil, err
		}
		if old, exists := next[node.Alias]; exists && old != endpoint.IdentityURL {
			return nil, ErrAliasReassigned
		}
		next[node.Alias] = endpoint.IdentityURL
	}
	return next, nil
}
