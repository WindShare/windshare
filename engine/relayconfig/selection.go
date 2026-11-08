package relayconfig

import (
	"fmt"
	"strings"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/engine/relayprofile"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

// SelectRelays treats a saved list or explicit override as the complete allowed
// set. No infrastructure is added implicitly, including development listeners.
func SelectRelays(config Config, override []string) ([]relayprofile.Candidate, error) {
	saved := config.Profiles
	if len(override) == 0 {
		override = config.ActiveRelays
	}
	if len(override) == 0 {
		return nil, ErrConfig
	}
	credentials := make(map[string]string, len(saved))
	for _, candidate := range saved {
		endpoint, err := v2.NormalizeRelayEndpoint(candidate.PublicURL)
		if err != nil {
			return nil, relayURLError(candidate.PublicURL)
		}
		credentials[endpoint.DialURL] = candidate.AccessKey
	}
	candidates := make([]relayprofile.Candidate, 0, len(override))
	seen := make(map[string]bool, len(override))
	for _, address := range override {
		endpoint, err := v2.NormalizeRelayEndpoint(address)
		if err != nil {
			return nil, relayURLError(address)
		}
		if seen[endpoint.DialURL] {
			continue
		}
		seen[endpoint.DialURL] = true
		// Identity excludes queries for protocol proofs. Credential matching must
		// retain those queries so another routing target cannot inherit authority.
		candidates = append(candidates, relayprofile.Candidate{
			PublicURL: address, AccessKey: credentials[endpoint.DialURL],
		})
	}
	if _, err := relayprofile.PublicURLs(candidates); err != nil || len(candidates) > relayset.MaximumEndpoints {
		return nil, ErrConfig
	}
	return candidates, nil
}

// ValidateRelayURL reports why a copied or saved relay address cannot be used.
func ValidateRelayURL(raw string) error {
	if _, err := v2.NormalizeRelayEndpoint(raw); err != nil {
		return relayURLError(raw)
	}
	return nil
}

func relayURLError(raw string) error {
	if strings.IndexFunc(raw, forbiddenFrontURLRune) >= 0 {
		return fmt.Errorf("%w: contains whitespace or invisible spacing; remove it from the address", ErrRelayURL)
	}
	return fmt.Errorf("%w: expected an absolute HTTP(S) or WS(S) address without credentials or fragment", ErrRelayURL)
}
