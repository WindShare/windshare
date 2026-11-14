package engine

import (
	"errors"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/engine/internal/networkruntime"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

type ShareAuthorizationKind string

const (
	ShareSelfHostedAuthorization ShareAuthorizationKind = "self_hosted"
	ShareOfficialAuthorization   ShareAuthorizationKind = "official"
)

type ShareAuthorization struct {
	Kind     ShareAuthorizationKind `json:"kind"`
	RelayURL string                 `json:"relay_url,omitempty"`
}

func shareAuthorization(err error, selection networkruntime.Selection) *ShareAuthorization {
	denied := make(map[string]bool)
	collectDeniedRegistrations(err, denied)
	// Prefer an explicit relay's device approval. Account provisioning is an
	// independent official-network workflow, never self-hosted enrollment.
	for _, candidate := range selection.Plan.Configured {
		if denied[candidate.URL] {
			return &ShareAuthorization{Kind: ShareSelfHostedAuthorization, RelayURL: candidate.URL}
		}
	}
	for _, candidate := range selection.Plan.Network {
		if denied[candidate.URL] {
			return &ShareAuthorization{Kind: ShareOfficialAuthorization}
		}
	}
	return nil
}

//nolint:errorlint // Walk every tree node; errors.As would discard sibling registration failures.
func collectDeniedRegistrations(err error, denied map[string]bool) {
	if failure, ok := err.(*relayset.RegistrationFailure); ok {
		var rejection *relayv2.RelayError
		if errors.As(failure.Cause, &rejection) && rejection.Code == v2.ErrorAccessDenied {
			denied[failure.URL] = true
		}
		return
	}
	switch failure := err.(type) {
	case interface{ Unwrap() []error }:
		for _, cause := range failure.Unwrap() {
			collectDeniedRegistrations(cause, denied)
		}
	case interface{ Unwrap() error }:
		collectDeniedRegistrations(failure.Unwrap(), denied)
	}
}
