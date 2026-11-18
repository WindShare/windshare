package relayconfig

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

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
	credentials := make(map[string]string, len(saved))
	for _, candidate := range saved {
		endpoint, err := v2.NormalizeRelayEndpoint(candidate.PublicURL)
		if err != nil {
			return nil, relayAddressError{cause: err}
		}
		credentials[endpoint.DialURL] = candidate.AccessKey
	}
	candidates := make([]relayprofile.Candidate, 0, len(override))
	seen := make(map[string]bool, len(override))
	for _, address := range override {
		endpoint, err := normalizeSelectedRelay(address)
		if err != nil {
			return nil, err
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

// Relays print the WebSocket endpoint they serve, but candidates are base URLs
// to which normalization appends that path. The doubled path fails every
// upgrade, so both explicit overrides and saved active relays reject the copied
// endpoint before a share retries it indefinitely. Inactive profiles are not
// dialed and keep their existing validation.
func normalizeSelectedRelay(address string) (v2.RelayEndpoint, error) {
	endpoint, err := v2.NormalizeRelayEndpoint(address)
	if err == nil && repeatsWebSocketPath(endpoint) {
		err = &relayWebSocketPathError{address: redactRelayAddress(address)}
	}
	if err != nil {
		return v2.RelayEndpoint{}, relayAddressError{cause: err}
	}
	return endpoint, nil
}

func repeatsWebSocketPath(endpoint v2.RelayEndpoint) bool {
	parsed, err := url.Parse(endpoint.IdentityURL)
	return err == nil && strings.HasSuffix(parsed.EscapedPath(), v2.WebSocketPath+v2.WebSocketPath)
}

// Name the address so one bad entry is identifiable in a multi-relay list,
// without echoing routing queries or other secrets it may carry.
type relayWebSocketPathError struct{ address string }

func (err *relayWebSocketPathError) Error() string {
	return "relay base " + err.address + " ends in " + v2.WebSocketPath
}
func (err *relayWebSocketPathError) Unwrap() []error {
	return []error{v2.ErrIdentity, networkendpoint.ErrInvalid}
}

func redactRelayAddress(address string) string {
	parsed, err := url.Parse(address)
	if err != nil {
		return "(unparseable address)"
	}
	parsed.User, parsed.RawQuery, parsed.ForceQuery = nil, "", false
	parsed.Fragment, parsed.RawFragment = "", ""
	return parsed.String()
}

// Preserve the domain cause while presenting only the address detail relevant
// to repair; protocol wrapper errors are not useful configuration instructions.
type relayAddressError struct{ cause error }

func (err relayAddressError) Error() string {
	var detail string
	host, numeric := errors.AsType[*networkendpoint.NumericHostError](err.cause)
	copied, endpoint := errors.AsType[*relayWebSocketPathError](err.cause)
	switch {
	case numeric:
		detail = fmt.Sprintf("host %q ends in a number; use a DNS name or a canonical IP address", host.Host)
	case endpoint:
		detail = fmt.Sprintf("address %q ends in %s, which WindShare adds itself; remove it to use the relay base URL", copied.address, v2.WebSocketPath)
	case errors.Is(err.cause, v2.ErrRelayEndpointWhitespace):
		detail = "contains whitespace or invisible spacing; remove it from the address"
	default:
		detail = "not an accepted relay address; use an absolute http, https, ws, or wss URL with a host and no user:password@ or #fragment"
	}
	return ErrRelayURL.Error() + ": " + detail
}

func (err relayAddressError) Is(target error) bool { return target == ErrRelayURL }
func (err relayAddressError) Unwrap() error        { return err.cause }
