package provider

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
)

const (
	MaximumServiceIDBytes = 48
	// These bounds match the managed issuer's reservation and credential policy.
	ManagedLeaseLifetime   = time.Hour
	ManagedIssuanceTimeout = 30 * time.Second
)

var ErrAuthorityWithdrawn = errors.New("managed TURN authority withdrawn")

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

func (source *Remote) Issue(ctx context.Context, subject Subject) (Acquisition, error) {
	return source.issue(ctx, subject, "")
}
func (source *Remote) Renew(ctx context.Context, subject Subject, previous Acquisition) (Acquisition, error) {
	if previous.remote == nil || previous.remote.SessionID != base64.RawURLEncoding.EncodeToString(subject.SessionID[:]) {
		return Acquisition{}, ErrConfiguration
	}
	return source.issue(ctx, subject, previous.remote.LeaseID)
}
func (source *Remote) request(subject Subject, leaseID string) remoteRequest {
	return remoteRequest{ServiceID: source.config.ServiceID, LeaseID: leaseID,
		SessionID: base64.RawURLEncoding.EncodeToString(subject.SessionID[:]),
		PathID:    base64.RawURLEncoding.EncodeToString(subject.PathID[:])}
}
func (source *Remote) issue(ctx context.Context, subject Subject, previous string) (Acquisition, error) {
	if err := ctx.Err(); err != nil {
		return Acquisition{}, err
	}
	if subject.SessionID == ([16]byte{}) || subject.PathID == ([16]byte{}) {
		return Acquisition{}, ErrConfiguration
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Acquisition{}, err
	}
	request := source.request(subject, base64.RawURLEncoding.EncodeToString(id[:]))
	request.PreviousLeaseID = previous
	// The owner exists before I/O, including cancellation and invalid replies.
	// Include both request and issuer windows in the conservative cleanup bound.
	acquisition := Acquisition{remote: &request, Cleanup: &Cleanup{
		ID: request.LeaseID, ExpiresAt: source.config.Now().Add(HTTPTimeout + ManagedIssuanceTimeout + ManagedLeaseLifetime),
		Revoke: func(ctx context.Context) (CleanupResult, error) {
			return source.revoke(ctx, source.request(subject, request.LeaseID))
		},
	}}
	path := "/issue"
	if previous != "" {
		path = "/renew"
	}
	reply, err := source.resolve(ctx, path, request)
	if err != nil {
		return acquisition, err
	}
	grant := reply.grant
	if grant.ServiceID != request.ServiceID || grant.LeaseID != request.LeaseID ||
		grant.SessionID != request.SessionID || grant.PathID != request.PathID {
		return acquisition, ErrConfiguration
	}
	now := source.config.Now()
	expires, err := reply.localExpiry(now)
	slog.Debug("TURN remote grant validity", "service_id", request.ServiceID, "lease_id", request.LeaseID,
		"session_id", request.SessionID, "path_id", request.PathID, "server_date", reply.headers.Get("Date"),
		"issuer_expires_at", grant.ExpiresAt, "request_elapsed", now.Sub(reply.requestedAt),
		"local_expires_at", expires, "reason", err)
	if err != nil {
		return acquisition, err
	}
	credential, err := iceconfig.Expiring(grant.Username, grant.Password, expires)
	if err != nil {
		return acquisition, err
	}
	if _, err := iceconfig.NewRelay(iceconfig.Identity{Provider: "cloudflare", Account: grant.ServiceID,
		Profile: grant.ServiceID}, grant.URLs, credential); err != nil {
		return acquisition, err
	}
	acquisition.Credentials, acquisition.URLs = credential, grant.URLs
	return acquisition, nil
}
func (source *Remote) revoke(ctx context.Context, request remoteRequest) (CleanupResult, error) {
	var result struct {
		State string `json:"state"`
	}
	if _, err := remoteCall(ctx, source.config.Client, source.base+"/revoke", source.config.AccessKey, request, &result); err != nil {
		return 0, err
	}
	switch result.State {
	case "revoked":
		return RevocationConfirmed, nil
	case "expired":
		return AuthorityExpired, nil
	case "canceled", "issuance_failed":
		return IssuanceCanceled, nil
	case "revocation_pending":
		return RevocationQueued, nil
	default:
		return 0, ErrConfiguration
	}
}
