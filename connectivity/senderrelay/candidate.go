package senderrelay

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

const MaxRegistrationTokenBytes = 4096

var ErrCandidate = errors.New("invalid sender relay candidate")

// Candidate binds deployment registration authority to one public relay address.
// Only PublicURL may enter a capability; the token is not session/STOP authority.
type Candidate struct {
	PublicURL         string
	RegistrationToken string `json:"-"`
}

func (candidate Candidate) Validate() error {
	endpoint, err := v2.NormalizeRelayEndpoint(candidate.PublicURL)
	if err != nil {
		return errors.Join(ErrCandidate, err)
	}
	parsed, err := url.Parse(endpoint.DialURL)
	if err != nil {
		return ErrCandidate
	}
	for pair := range strings.SplitSeq(parsed.RawQuery, "&") {
		key, _, _ := strings.Cut(pair, "=")
		decoded, err := url.QueryUnescape(key)
		if err != nil || decoded == v2.ShareRouteQuery {
			return ErrCandidate
		}
	}
	if len(candidate.RegistrationToken) > MaxRegistrationTokenBytes {
		return ErrCandidate
	}
	for _, character := range candidate.RegistrationToken {
		if character < '!' || character > '~' {
			return ErrCandidate
		}
	}
	return nil
}

// PublicURLs validates the entire publication before any candidate can register.
// Aliases of the same relay identity would compete for one share's ownership.
func PublicURLs(candidates []Candidate) ([]string, error) {
	urls := make([]string, 0, len(candidates))
	seen := make(map[v2.RelayIdentity]bool, len(candidates))
	for _, candidate := range candidates {
		if err := candidate.Validate(); err != nil {
			return nil, err
		}
		endpoint, _ := v2.NormalizeRelayEndpoint(candidate.PublicURL)
		if seen[endpoint.Identity] {
			return nil, ErrCandidate
		}
		seen[endpoint.Identity] = true
		urls = append(urls, candidate.PublicURL)
	}
	return urls, nil
}

func (candidate Candidate) registrationDialOptions(capacity int, resume bool) relayv2.DialOptions {
	options := relayv2.DialOptions{LifecycleObservationCapacity: capacity}
	if !resume && candidate.RegistrationToken != "" {
		options.Header = http.Header{"Authorization": {"Bearer " + candidate.RegistrationToken}}
		// Even same-host redirects can target another relay deployment. Credentials
		// authorize the configured candidate, never a redirect-selected endpoint.
		options.HTTPClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
	}
	return options
}
