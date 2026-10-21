package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const CloudflareAnalyticsEndpoint = "https://api.cloudflare.com/client/v4/graphql"
const MaximumUsageWindow = 31 * 24 * time.Hour

// Usage is a provider estimate for complete UTC dates, not local transport
// counters or an invoice. Cloudflare's adaptive dataset may be sampled.
type Usage struct {
	FirstDate, LastDate       time.Time
	EgressBytes, IngressBytes uint64
	Sampled                   bool
}
type CloudflareUsageConfig struct {
	AccountID, KeyID, APIToken string
	Client                     HTTPClient
}
type CloudflareUsage struct{ config CloudflareUsageConfig }

func (config CloudflareUsageConfig) Validate() error {
	if !hexID(config.AccountID) || !hexID(config.KeyID) || config.APIToken == "" {
		return ErrConfiguration
	}
	return nil
}

func NewCloudflareUsage(config CloudflareUsageConfig) (*CloudflareUsage, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if config.Client == nil {
		config.Client = &http.Client{Timeout: HTTPTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &CloudflareUsage{config}, nil
}
func hexID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'f' || char >= 'A' && char <= 'F' || char >= '0' && char <= '9' {
			continue
		}
		return false
	}
	return true
}
func (source *CloudflareUsage) Query(ctx context.Context, first, last time.Time) (Usage, error) {
	first = first.UTC().Truncate(24 * time.Hour)
	last = last.UTC().Truncate(24 * time.Hour)
	if first.IsZero() || last.Before(first) || last.Sub(first) > MaximumUsageWindow {
		return Usage{}, ErrConfiguration
	}
	ctx, cancel := context.WithTimeout(ctx, HTTPTimeout)
	defer cancel()
	query := fmt.Sprintf(`query { viewer { accounts(filter:{accountTag:%q}) { callsTurnUsageAdaptiveGroups(limit:1,filter:{date_geq:%q,date_leq:%q,keyId:%q}) { sum { egressBytes ingressBytes } } } } }`,
		source.config.AccountID, first.Format(time.DateOnly), last.Format(time.DateOnly), source.config.KeyID)
	body, _ := json.Marshal(struct {
		Query string `json:"query"`
	}{query})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, CloudflareAnalyticsEndpoint, bytes.NewReader(body))
	if err != nil {
		return Usage{}, err
	}
	request.Header.Set("Authorization", "Bearer "+source.config.APIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := source.config.Client.Do(request)
	if err != nil {
		return Usage{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Usage{}, fmt.Errorf("Cloudflare TURN analytics returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, MaximumResponseBytes+1))
	if err != nil || len(data) > MaximumResponseBytes {
		return Usage{}, errors.Join(errors.New("invalid TURN analytics response size"), err)
	}
	var result struct {
		Data struct {
			Viewer struct {
				Accounts []struct {
					Groups []struct {
						Sum struct {
							EgressBytes  uint64 `json:"egressBytes"`
							IngressBytes uint64 `json:"ingressBytes"`
						} `json:"sum"`
					} `json:"callsTurnUsageAdaptiveGroups"`
				} `json:"accounts"`
			} `json:"viewer"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return Usage{}, err
	}
	if len(result.Errors) > 0 {
		return Usage{}, fmt.Errorf("Cloudflare TURN analytics: %s", result.Errors[0].Message)
	}
	if len(result.Data.Viewer.Accounts) != 1 {
		return Usage{}, errors.New("Cloudflare TURN analytics returned no account")
	}
	usage := Usage{FirstDate: first, LastDate: last, Sampled: true}
	for _, group := range result.Data.Viewer.Accounts[0].Groups {
		usage.EgressBytes += group.Sum.EgressBytes
		usage.IngressBytes += group.Sum.IngressBytes
	}
	return usage, nil
}
