// Package relayprofile describes application relay selection and saved credentials.
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
	return (senderrelay.Candidate{PublicURL: candidate.PublicURL, AccessKey: candidate.AccessKey}).Validate()
}

func PublicURLs(candidates []Candidate) ([]string, error) {
	native := make([]senderrelay.Candidate, len(candidates))
	for i, candidate := range candidates {
		native[i] = senderrelay.Candidate{PublicURL: candidate.PublicURL, AccessKey: candidate.AccessKey}
	}
	return senderrelay.PublicURLs(native)
}
