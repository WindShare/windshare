package relayprofile

import (
	"context"
	"net/http"
	"sync/atomic"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/serviceauthority"
)

type AuthorizationSource interface {
	ReserveRegistrationAuthorization() serviceauthority.RegistrationAuthorization
}

type CommunityRoute struct {
	Client *http.Client
	Origin string
	Check  func(context.Context) error
	Ready  atomic.Bool
}

type Registration struct {
	Community     *CommunityRoute `json:"-"`
	PublicURL     string
	Authorization AuthorizationSource `json:"-"`
}

func (candidate Candidate) Registration() Registration {
	return Registration{PublicURL: candidate.PublicURL, Authorization: relayset.StaticKey(candidate.AccessKey)}
}

func Registrations(profiles []Candidate) []Registration {
	candidates := make([]Registration, len(profiles))
	for i, profile := range profiles {
		candidates[i] = profile.Registration()
	}
	return candidates
}

func RegistrationURLs(candidates []Registration) ([]string, error) {
	native := make([]relayset.Candidate, len(candidates))
	for i, candidate := range candidates {
		// A community route has a public-only dialer and anonymous admission.
		// Keep that invariant at the final boundary shared by all sender factories.
		if route := candidate.Community; route != nil && (candidate.Authorization != nil || route.Client == nil || route.Check == nil || route.Origin == "") {
			return nil, relayset.ErrCandidate
		}
		native[i] = relayset.Candidate{PublicURL: candidate.PublicURL, Authorization: candidate.Authorization}
	}
	return relayset.PublicURLs(native)
}
