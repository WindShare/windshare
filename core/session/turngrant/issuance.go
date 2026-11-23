// Package turngrant defines the session's network-independent credential issuance contract.
package turngrant

import (
	"time"

	p "github.com/windshare/windshare/core/session/protocolsession"
)

// RenewalLead leaves a quarter of a short credential's lifetime for renewal
// while avoiding unnecessary issuance early in a long-lived grant.
func RenewalLead(lifetime time.Duration) time.Duration {
	return min(lifetime/4, 30*time.Second)
}

type Credential struct {
	ServiceID          string
	URLs               []string
	Username, Password string
	// The issuer owns expiry. Waiting for sibling issuers cannot extend a credential.
	// A zero deadline is reserved for explicitly static credentials.
	ExpiresAt time.Time
}

// Reusable contains only live grants already owned by the requesting session.
type IssueRequest struct {
	Trace      Trace
	Mode       p.TURNMode
	ServiceIDs []string
	Reusable   map[string]bool
}
type IssueResult struct {
	Retained    []string
	Status      p.TURNStatus
	HasFallback bool
	Credentials []Credential
}
