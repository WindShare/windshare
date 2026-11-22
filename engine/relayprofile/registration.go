package relayprofile

import (
	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/serviceauthority"
)

type AuthorizationSource interface {
	ReserveRegistrationAuthorization() serviceauthority.RegistrationAuthorization
}

type Registration struct {
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
		native[i] = relayset.Candidate{PublicURL: candidate.PublicURL, Authorization: candidate.Authorization}
	}
	return relayset.PublicURLs(native)
}
