package relayconfig

import (
	"errors"
	"fmt"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/networkendpoint"
	"github.com/windshare/windshare/engine/relayprofile"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

func ShareIntent(config Config, override []string) (relayprofile.Intent, error) {
	var candidates []relayprofile.Candidate
	if len(override) > 0 || len(config.ActiveRelays) > 0 {
		var err error
		candidates, err = SelectRelays(config, override)
		if err != nil {
			return relayprofile.Intent{}, err
		}
	}
	var intent relayprofile.Intent
	switch {
	case len(override) > 0:
		intent = relayprofile.UseExplicit(candidates)
	case config.ConfiguredOnly:
		intent = relayprofile.OnlyConfigured(candidates)
	default:
		intent = relayprofile.Plan(candidates)
	}
	if err := intent.Validate(); err != nil {
		return relayprofile.Intent{}, errors.Join(ErrConfig, err)
	}
	return intent, nil
}

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
	profiles := make(map[string]relayprofile.Candidate, len(saved))
	for _, candidate := range saved {
		endpoint, err := v2.NormalizeRelayEndpoint(candidate.PublicURL)
		if err != nil {
			return nil, relayAddressError{cause: err}
		}
		profiles[endpoint.DialURL] = candidate
	}
	candidates := make([]relayprofile.Candidate, 0, len(override))
	seen := make(map[string]bool, len(override))
	for _, address := range override {
		endpoint, err := v2.NormalizeRelayEndpoint(address)
		if err != nil {
			return nil, relayAddressError{cause: err}
		}
		if seen[endpoint.DialURL] {
			continue
		}
		seen[endpoint.DialURL] = true
		// Identity excludes queries for protocol proofs. Credential matching must
		// retain those queries so another routing target cannot inherit authority.
		profile := profiles[endpoint.DialURL]
		profile.PublicURL = address
		candidates = append(candidates, profile)
	}
	if _, err := relayprofile.PublicURLs(candidates); err != nil || len(candidates) > relayset.MaximumEndpoints {
		return nil, ErrConfig
	}
	return candidates, nil
}

// RelayRouteGuidance applies to an observed HTTP failure. The same spelling can
// still name a working mount or an unused profile.
func RelayRouteGuidance(hint networkendpoint.RelayBaseHint) string {
	message := "WebSocket upgrade returned HTTP 404. Use the advertised relay_url"
	if hint.URL() != "" {
		message += fmt.Sprintf(" (check base %q)", hint.URL())
	}
	return message + ". Keep any routing query. For saved settings, update the matching profiles[].url and active_relays[] together."
}

// Preserve the domain cause while presenting only the address detail relevant
// to repair; protocol wrapper errors are not useful configuration instructions.
type relayAddressError struct{ cause error }

func (err relayAddressError) Error() string {
	var detail string
	if host, ok := errors.AsType[*networkendpoint.NumericHostError](err.cause); ok {
		detail = fmt.Sprintf("host %q ends in a number; use a DNS name or a canonical IP address", host.Host)
	} else if errors.Is(err.cause, v2.ErrRelayEndpointWhitespace) {
		detail = "contains whitespace or invisible spacing; remove it from the address"
	} else {
		detail = "not an accepted relay address; use an absolute http, https, ws, or wss URL with a host and no user:password@ or #fragment"
	}
	return ErrRelayURL.Error() + ": " + detail
}

func (err relayAddressError) Is(target error) bool { return target == ErrRelayURL }
func (err relayAddressError) Unwrap() error        { return err.cause }
