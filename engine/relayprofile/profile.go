// Package relayprofile describes application relay selection and saved service credentials.
package relayprofile

import (
	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/senderrelay"
)

const MaximumEndpoints = relayset.MaximumEndpoints
const MaxAccessKeyBytes = senderrelay.MaxAccessKeyBytes

type Candidate struct {
	PublicURL string
	AccessKey string `json:"-"`
}

func (candidate Candidate) Validate() error {
	return (senderrelay.Candidate{PublicURL: candidate.PublicURL, Authorization: senderrelay.StaticKey(candidate.AccessKey)}).Validate()
}

func PublicURLs(candidates []Candidate) ([]string, error) {
	for _, candidate := range candidates {
		if err := candidate.Validate(); err != nil {
			return nil, err
		}
	}
	return RegistrationURLs(Registrations(candidates))
}
