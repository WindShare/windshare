package relayprofile

import (
	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/core/serviceauthority"
)

type AuthorizationSource interface {
	RegistrationAuthorization() serviceauthority.RegistrationAuthorization
}

type Registration struct {
	PublicURL     string
	Authorization AuthorizationSource `json:"-"`
}

func (candidate Candidate) Registration() Registration {
	return Registration{PublicURL: candidate.PublicURL, Authorization: senderrelay.StaticKey(candidate.AccessKey)}
}

func Registrations(profiles []Candidate) []Registration {
	candidates := make([]Registration, len(profiles))
	for i, profile := range profiles {
		candidates[i] = profile.Registration()
	}
	return candidates
}

func RegistrationURLs(candidates []Registration) ([]string, error) {
	native := make([]senderrelay.Candidate, len(candidates))
	for i, candidate := range candidates {
		native[i] = senderrelay.Candidate{PublicURL: candidate.PublicURL, Authorization: candidate.Authorization}
	}
	return senderrelay.PublicURLs(native)
}
