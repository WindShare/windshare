// Package turngrant defines the session's network-independent credential issuance contract.
package turngrant

import (
	"time"

	p "github.com/windshare/windshare/core/session/protocolsession"
)

type Credential struct {
	ServiceID          string
	URLs               []string
	Username, Password string
	ValidFor           time.Duration
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
