package turnprovider

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
)

const MaximumServiceIDBytes = 48

var (
	ErrAuthorityWithdrawn = errors.New("managed TURN authority withdrawn")
	ErrRevocationPending  = errors.New("TURN revocation durably queued")
)

// Capability is inventory, never a credential. Absence during a failed discovery
// cannot invalidate a previously discovered service.
type Capability struct {
	ServiceID string `json:"service_id"`
	Enabled   bool   `json:"enabled"`
}

type RemoteConfig struct {
	RelayURL  string
	AccessKey string
	ServiceID string
	Client    HTTPClient
	Now       func() time.Time
}
type Remote struct {
	config RemoteConfig
	base   string
}

func NewRemote(config RemoteConfig) (*Remote, error) {
	base, err := ManagedEndpoint(config.RelayURL)
	if err != nil || config.AccessKey == "" || !ValidServiceID(config.ServiceID) {
		return nil, ErrConfiguration
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Client == nil {
		config.Client = remoteHTTPClient()
	}
	return &Remote{config: config, base: base}, nil
}
func ValidServiceID(id string) bool {
	return len(id) <= MaximumServiceIDBytes && (iceconfig.Identity{Provider: "cloudflare", Account: id, Profile: id}).Validate() == nil
}

type remoteRequest struct {
	ServiceID       string `json:"service_id"`
	LeaseID         string `json:"lease_id"`
	SessionID       string `json:"session_id"`
	PathID          string `json:"path_id"`
	PreviousLeaseID string `json:"previous_lease_id,omitempty"`
}
type remoteGrant struct {
	ServiceID string   `json:"service_id"`
	LeaseID   string   `json:"lease_id"`
	SessionID string   `json:"session_id"`
	PathID    string   `json:"path_id"`
	URLs      []string `json:"urls"`
	Username  string   `json:"username"`
	Password  string   `json:"password"`
	ExpiresAt int64    `json:"expires_at"`
}

func (source *Remote) Issue(ctx context.Context, subject Subject) (Issued, error) {
	return source.issue(ctx, subject, "")
}
func (source *Remote) Renew(ctx context.Context, subject Subject, previous Issued) (Issued, error) {
	if previous.LeaseID == "" || previous.Subject.SessionID != subject.SessionID {
		return Issued{}, ErrConfiguration
	}
	return source.issue(ctx, subject, previous.LeaseID)
}
func (source *Remote) request(subject Subject, leaseID string) remoteRequest {
	return remoteRequest{ServiceID: source.config.ServiceID, LeaseID: leaseID,
		SessionID: base64.RawURLEncoding.EncodeToString(subject.SessionID[:]),
		PathID:    base64.RawURLEncoding.EncodeToString(subject.PathID[:])}
}
func (source *Remote) issue(ctx context.Context, subject Subject, previous string) (Issued, error) {
	if subject.SessionID == ([16]byte{}) || subject.PathID == ([16]byte{}) {
		return Issued{}, ErrConfiguration
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Issued{}, err
	}
	request := source.request(subject, base64.RawURLEncoding.EncodeToString(id[:]))
	request.PreviousLeaseID = previous
	path := "/issue"
	if previous != "" {
		path = "/renew"
	}
	var grant remoteGrant
	if err := remoteCall(ctx, source.config.Client, source.base+path, source.config.AccessKey, request, &grant); err != nil {
		return Issued{}, err
	}
	if grant.ServiceID != request.ServiceID || grant.LeaseID != request.LeaseID ||
		grant.SessionID != request.SessionID || grant.PathID != request.PathID {
		return Issued{}, ErrConfiguration
	}
	expires := time.UnixMilli(grant.ExpiresAt)
	now := source.config.Now()
	if !expires.After(now) || expires.After(now.Add(MaximumLifetime)) {
		return Issued{}, ErrExpired
	}
	credential, err := iceconfig.Expiring(grant.Username, grant.Password, expires)
	if err != nil {
		return Issued{}, err
	}
	if _, err := iceconfig.NewRelay(iceconfig.Identity{Provider: "cloudflare", Account: grant.ServiceID,
		Profile: grant.ServiceID}, grant.URLs, credential); err != nil {
		return Issued{}, err
	}
	return Issued{Credentials: credential, URLs: grant.URLs, LeaseID: grant.LeaseID, Subject: subject}, nil
}
func (source *Remote) Revoke(ctx context.Context, issued Issued) error {
	if issued.LeaseID == "" {
		return ErrConfiguration
	}
	var result struct {
		State string `json:"state"`
	}
	if err := remoteCall(ctx, source.config.Client, source.base+"/revoke", source.config.AccessKey,
		source.request(issued.Subject, issued.LeaseID), &result); err != nil {
		return err
	}
	switch result.State {
	case "revoked", "expired":
		return nil
	case "revocation_pending":
		return ErrRevocationPending
	default:
		return ErrConfiguration
	}
}
