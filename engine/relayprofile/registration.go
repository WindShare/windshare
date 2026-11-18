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

type StaticKey string

func (key StaticKey) RegistrationAuthorization() serviceauthority.RegistrationAuthorization {
	return serviceauthority.RegistrationAuthorization{State: serviceauthority.AdmissionReady, Bearer: string(key)}
}

func (candidate Candidate) Registration() Registration {
	return Registration{PublicURL: candidate.PublicURL, Authorization: StaticKey(candidate.AccessKey)}
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
