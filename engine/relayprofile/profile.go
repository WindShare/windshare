// Package relayprofile describes application relay selection and saved service credentials.
package relayprofile

import (
	"github.com/windshare/windshare/connectivity/relayset"
)

const MaximumEndpoints = relayset.MaximumEndpoints
const MaxAccessKeyBytes = relayset.MaxAccessKeyBytes

type CredentialRole string

const (
	Owner   CredentialRole = "owner"
	Service CredentialRole = "service"
	Account CredentialRole = "account"
)

type NodeIdentity struct {
	Platform  string `json:"platform,omitempty"`
	NodeKey   string `json:"node_key,omitempty"`
	Authority string `json:"authority,omitempty"`
}

type Candidate struct {
	Node            NodeIdentity   `json:"-"`
	Role            CredentialRole `json:"-"`
	ManagedTurn     bool           `json:"managed_turn,omitempty"`
	PublicURL       string
	ConsoleURL      string `json:"-"`
	OfferedFrontend string `json:"-"`
	AccessKey       string `json:"-"`
}

func (candidate Candidate) Validate() error {
	if candidate.Role != "" && (candidate.AccessKey == "" || (candidate.Role != Owner && candidate.Role != Account && candidate.Role != Service)) {
		return relayset.ErrCandidate
	}
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
