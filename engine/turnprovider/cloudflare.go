package turnprovider

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

func NewCloudflare(config CloudflareConfig) (*Cloudflare, error) {
	if config.Lifetime == 0 {
		config.Lifetime = DefaultLifetime
	}
	if config.KeyID == "" || strings.ContainsAny(config.KeyID, "/?# \r\n") ||
		config.APIToken == "" || !validLifetime(config.Lifetime) {
		return nil, ErrConfiguration
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

func (source *Cloudflare) Issue(ctx context.Context, _ Subject) (Issued, error) {
	ctx, cancel := context.WithTimeout(ctx, HTTPTimeout)
	defer cancel()
	started := source.config.Now()
	body, err := json.Marshal(struct {
		TTL int64 `json:"ttl"`
	}{int64(source.config.Lifetime / time.Second)})
	if err != nil {
		return Issued{}, err
	}
	response, err := source.request(ctx, "credentials/generate-ice-servers", body)
	if err != nil {
		return Issued{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		return Issued{}, fmt.Errorf("Cloudflare TURN issuance returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, MaximumResponseBytes+1))
	if err != nil || len(data) > MaximumResponseBytes {
		return Issued{}, errors.Join(errors.New("invalid Cloudflare TURN response size"), err)
	}
	var result struct {
		ICEServers []struct {
			URLs       []string `json:"urls"`
			Username   string   `json:"username"`
			Credential string   `json:"credential"`
		} `json:"iceServers"`
	}
	if err = json.Unmarshal(data, &result); err != nil {
		return Issued{}, fmt.Errorf("decode Cloudflare TURN response: %w", err)
	}
	var issued Issued
	for _, server := range result.ICEServers {
		for _, raw := range server.URLs {
			// The response also contains discovery endpoints. An issuer cannot
			// silently replace the independently configured discovery pool.
			if strings.HasPrefix(raw, "stun:") {
				continue
			}
			if iceconfig.ValidateRelayURL(raw) != nil || len(issued.URLs) >= iceconfig.MaximumURLs {
				return Issued{}, errors.New("invalid Cloudflare TURN endpoint")
			}
			if issued.Credentials.Kind() != 0 &&
				(issued.Credentials.Username() != server.Username || issued.Credentials.Password() != server.Credential) {
				return Issued{}, errors.New("Cloudflare returned conflicting TURN credentials")
			}
			issued.Credentials, err = iceconfig.Expiring(server.Username, server.Credential, started.Add(source.config.Lifetime))
			if err != nil {
				return Issued{}, err
			}
			issued.URLs = append(issued.URLs, raw)
		}
	}
	if len(issued.URLs) == 0 || !issued.Credentials.ValidAt(source.config.Now()) {
		return Issued{}, errors.New("Cloudflare returned no usable TURN authority")
	}
	return issued, nil
}

func (source *Cloudflare) Renew(ctx context.Context, subject Subject, _ Issued) (Issued, error) {
	return source.Issue(ctx, subject)
}

// Revoke returns success only after the provider confirms revocation. Closing a
// local connection alone cannot withdraw credentials delegated to a recipient.
func (source *Cloudflare) Revoke(ctx context.Context, issued Issued) error {
	ctx, cancel := context.WithTimeout(ctx, HTTPTimeout)
	defer cancel()
	if issued.Credentials.Username() == "" {
		return ErrConfiguration
	}
	response, err := source.request(ctx, "credentials/"+url.PathEscape(issued.Credentials.Username())+"/revoke", nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("Cloudflare TURN revocation returned HTTP %d", response.StatusCode)
	}
	return nil
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
