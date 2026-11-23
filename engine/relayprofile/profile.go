// Package relayprofile describes application relay selection and saved service credentials.
package relayprofile

import (
	"github.com/windshare/windshare/connectivity/relayset"
)

const MaximumEndpoints = relayset.MaximumEndpoints
const MaxAccessKeyBytes = relayset.MaxAccessKeyBytes

type Candidate struct {
	ManagedTurn     bool `json:"managed_turn,omitempty"`
	PublicURL       string
	ConsoleURL      string `json:"-"`
	OfferedFrontend string `json:"-"`
	AccessKey       string `json:"-"`
}

func (candidate Candidate) Validate() error {
	return (relayset.Candidate{PublicURL: candidate.PublicURL, Authorization: relayset.StaticKey(candidate.AccessKey)}).Validate()
}

func PublicURLs(candidates []Candidate) ([]string, error) {
	for _, candidate := range candidates {
		if err := candidate.Validate(); err != nil {
			return nil, err
		}
	}
	return RegistrationURLs(Registrations(candidates))
}
