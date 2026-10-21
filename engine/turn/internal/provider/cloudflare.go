package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
)

const (
	CloudflareEndpoint   = "https://rtc.live.cloudflare.com/v1/turn/keys/"
	MaximumResponseBytes = 64 * 1024
	HTTPTimeout          = 15 * time.Second
)

type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

type CloudflareConfig struct {
	KeyID    string
	APIToken string
	Lifetime time.Duration
	Client   HTTPClient
	Now      func() time.Time
}

type Cloudflare struct{ config CloudflareConfig }

func (config CloudflareConfig) Validate() error {
	if config.Lifetime == 0 {
		config.Lifetime = DefaultLifetime
	}
	if config.KeyID == "" || strings.ContainsAny(config.KeyID, "/?# \r\n") ||
		config.APIToken == "" || !validLifetime(config.Lifetime) {
		return ErrConfiguration
	}
	return nil
}

func NewCloudflare(config CloudflareConfig) (*Cloudflare, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if config.Lifetime == 0 {
		config.Lifetime = DefaultLifetime
	}
	if config.Client == nil {
		config.Client = &http.Client{Timeout: HTTPTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Cloudflare{config}, nil
}

func (source *Cloudflare) Issue(ctx context.Context, _ Subject) (Acquisition, error) {
	ctx, cancel := context.WithTimeout(ctx, HTTPTimeout)
	defer cancel()
	started := source.config.Now()
	body, err := json.Marshal(struct {
		TTL int64 `json:"ttl"`
	}{int64(source.config.Lifetime / time.Second)})
	if err != nil {
		return Acquisition{}, err
	}
	response, err := source.request(ctx, "credentials/generate-ice-servers", body)
	if err != nil {
		return Acquisition{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		return Acquisition{}, fmt.Errorf("Cloudflare TURN issuance returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, MaximumResponseBytes+1))
	if err != nil || len(data) > MaximumResponseBytes {
		return Acquisition{}, errors.Join(errors.New("invalid Cloudflare TURN response size"), err)
	}
	return source.decodeGrant(data, started)
}

func (source *Cloudflare) decodeGrant(data []byte, started time.Time) (Acquisition, error) {
	var result struct {
		ICEServers []struct {
			URLs       []string `json:"urls"`
			Username   string   `json:"username"`
			Credential string   `json:"credential"`
		} `json:"iceServers"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return Acquisition{}, fmt.Errorf("decode Cloudflare TURN response: %w", err)
	}
	var issued Acquisition
	for _, server := range result.ICEServers {
		for _, raw := range server.URLs {
			// The response also contains discovery endpoints. An issuer cannot
			// silently replace the independently configured discovery pool.
			if strings.HasPrefix(raw, "stun:") {
				continue
			}
			if issued.Cleanup == nil && server.Username != "" {
				username := server.Username
				issued.Cleanup = &Cleanup{ID: username, ExpiresAt: source.config.Now().Add(source.config.Lifetime),
					Revoke: func(ctx context.Context) (CleanupResult, error) { return source.revoke(ctx, username) }}
			}
			if iceconfig.ValidateRelayURL(raw) != nil || len(issued.URLs) >= iceconfig.MaximumURLs {
				return issued, errors.New("invalid Cloudflare TURN endpoint")
			}
			if issued.Credentials.Kind() != 0 &&
				(issued.Credentials.Username() != server.Username || issued.Credentials.Password() != server.Credential) {
				return issued, errors.New("Cloudflare returned conflicting TURN credentials")
			}
			credential, err := iceconfig.Expiring(server.Username, server.Credential, started.Add(source.config.Lifetime))
			if err != nil {
				return issued, err
			}
			issued.Credentials = credential
			issued.URLs = append(issued.URLs, raw)
		}
	}
	if len(issued.URLs) == 0 || !issued.Credentials.ValidAt(source.config.Now()) {
		return issued, errors.New("Cloudflare returned no usable TURN authority")
	}
	return issued, nil
}

func (source *Cloudflare) Renew(ctx context.Context, subject Subject, _ Acquisition) (Acquisition, error) {
	return source.Issue(ctx, subject)
}

// revoke returns success only after the provider confirms revocation. Closing a
// local connection alone cannot withdraw credentials delegated to a recipient.
func (source *Cloudflare) revoke(ctx context.Context, username string) (CleanupResult, error) {
	ctx, cancel := context.WithTimeout(ctx, HTTPTimeout)
	defer cancel()
	if username == "" {
		return 0, ErrConfiguration
	}
	response, err := source.request(ctx, "credentials/"+url.PathEscape(username)+"/revoke", nil)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return 0, fmt.Errorf("Cloudflare TURN revocation returned HTTP %d", response.StatusCode)
	}
	return RevocationConfirmed, nil
}

func (source *Cloudflare) request(ctx context.Context, path string, body []byte) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		CloudflareEndpoint+url.PathEscape(source.config.KeyID)+"/"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+source.config.APIToken)
	request.Header.Set("Content-Type", "application/json")
	return source.config.Client.Do(request)
}
