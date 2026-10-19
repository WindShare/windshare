package relayconfig

import (
	"slices"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/senderrelay"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

// Select treats a saved list or explicit override as the complete allowed set.
// Default infrastructure is used only when no selection has been configured.
func Select(saved []senderrelay.Candidate, override []string, defaultURL string) ([]senderrelay.Candidate, error) {
	if len(override) == 0 {
		if len(saved) != 0 {
			return slices.Clone(saved), nil
		}
		return []senderrelay.Candidate{{PublicURL: defaultURL}}, nil
	}
	credentials := make(map[string]string, len(saved))
	for _, candidate := range saved {
		endpoint, err := v2.NormalizeRelayEndpoint(candidate.PublicURL)
		if err != nil {
			return nil, ErrConfig
		}
		credentials[endpoint.DialURL] = candidate.RegistrationToken
	}
	candidates := make([]senderrelay.Candidate, 0, len(override))
	seen := make(map[string]bool, len(override))
	for _, address := range override {
		endpoint, err := v2.NormalizeRelayEndpoint(address)
		if err != nil {
			return nil, ErrConfig
		}
		if seen[endpoint.DialURL] {
			continue
		}
		seen[endpoint.DialURL] = true
		// Identity excludes queries for protocol proofs. Credential matching must
		// retain those queries so another routing target cannot inherit authority.
		candidates = append(candidates, senderrelay.Candidate{
			PublicURL: address, RegistrationToken: credentials[endpoint.DialURL],
		})
	}
	if _, err := senderrelay.PublicURLs(candidates); err != nil || len(candidates) > relayset.MaximumEndpoints {
		return nil, ErrConfig
	}
	return candidates, nil
}
