// Package account coordinates official-service credentials outside relay handshakes.
package account

import (
	"context"
	"net/http"
	"time"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/serviceauthority"
)

const (
	CacheLoadTimeout = 100 * time.Millisecond
	RequestTimeout   = 8 * time.Second
	// The authority waits up to 15 seconds for the mail provider to accept delivery.
	EmailRequestTimeout = 20 * time.Second
	RefreshInterval     = time.Minute
	TicketRenewalLead   = time.Hour
	PollInterval        = 5 * time.Second
	MinimumRetry        = time.Second
	MaximumRetry        = time.Minute
	TicketMargin        = time.Minute
	maxRateLimitSeconds = 3600
)

type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}
type Store interface {
	Load(context.Context) (Record, error)
	Update(context.Context, func(Record) (Record, error)) (Record, error)
}

type Node struct{ Key [32]byte }
type Config struct {
	URL         string
	Root        [32]byte
	Store       Store
	Client      HTTPClient
	Now         func() time.Time
	ResolveNode func(string) (Node, bool)
	Diagnostics diagnostics.Sink
}

// RefreshOutcome describes what a completed attempt achieved for its caller.
type RefreshOutcome string

const (
	RefreshSucceeded         RefreshOutcome = "refreshed"
	RefreshNeedsRegistration RefreshOutcome = "registration_required"
	RefreshNeedsEmail        RefreshOutcome = "email_required"
)

type State string

const (
	Pending              State = "pending_setup"
	RegistrationRequired State = "registration_required"
	Registering          State = "registering"
	EmailRequired        State = "email_required"
	Ready                State = "ready"
	RefreshRequired      State = "refresh_required"
	Refreshing           State = "refreshing"
	Unavailable          State = "temporarily_unavailable"
	StorageError         State = "storage_error"
	Denied               State = "denied"
)

type Challenge struct {
	ID        string `json:"id"`
	Mode      string `json:"mode"`
	Bits      uint8  `json:"bits"`
	ExpiresAt uint64 `json:"expires_at"`
}
type Proof struct {
	Challenge Challenge `json:"challenge"`
	Solution  string    `json:"solution"`
}
type Snapshot struct {
	Active               bool                       `json:"active"`
	State                State                      `json:"state"`
	EmailCodePending     bool                       `json:"email_code_pending,omitempty"`
	Account              string                     `json:"account,omitempty"`
	Credential           string                     `json:"credential,omitempty"`
	Username             string                     `json:"username,omitempty"`
	Profile              Profile                    `json:"profile"`
	PendingEmail         PendingEmail               `json:"pending_email,omitzero"`
	Allowance            serviceauthority.Allowance `json:"allowance"`
	RemainingBytes       uint64                     `json:"remaining_bytes"`
	ResetAt              time.Time                  `json:"reset_at,omitzero"`
	ValidUntil           time.Time                  `json:"valid_until,omitzero"`
	TicketExpires        time.Time                  `json:"ticket_expires,omitzero"`
	RetryAt              time.Time                  `json:"retry_at,omitzero"`
	RegistrationPermille uint16                     `json:"registration_permille,omitempty"`
	Reason               string                     `json:"reason,omitempty"`
}

type Credential struct {
	Account  [16]byte `json:"account"`
	ID       [16]byte `json:"id"`
	Secret   string   `json:"secret"`
	Username string   `json:"username,omitempty"`
}
type Record struct {
	Issuer       string                       `json:"issuer"`
	SetupSecret  string                       `json:"setup_secret,omitempty"`
	Credential   *Credential                  `json:"credential,omitempty"`
	Profile      Profile                      `json:"profile,omitzero"`
	PendingEmail PendingEmail                 `json:"pending_email,omitzero"`
	Certificate  string                       `json:"certificate,omitempty"`
	Ticket       string                       `json:"ticket,omitempty"`
	Entitlement  serviceauthority.Entitlement `json:"entitlement"`
	Denial       string                       `json:"denial,omitempty"`
	Proof        *Proof                       `json:"proof,omitempty"`
}

type session struct {
	Account     [16]byte                     `json:"account"`
	Credential  [16]byte                     `json:"credential"`
	Secret      string                       `json:"credential_secret"`
	Username    string                       `json:"username"`
	Profile     Profile                      `json:"profile"`
	Certificate string                       `json:"certificate"`
	Ticket      string                       `json:"ticket"`
	ExpiresAt   uint64                       `json:"expires_at"`
	Entitlement serviceauthority.Entitlement `json:"entitlement"`
}
type fault struct {
	Kind       string    `json:"kind"`
	Reason     string    `json:"reason"`
	RetryAfter uint64    `json:"retry_after"`
	Challenge  Challenge `json:"challenge"`
	Profile    Profile   `json:"profile,omitzero"`
}

func (failure *fault) Error() string {
	return "account authority: " + failure.Kind + " " + failure.Reason
}
