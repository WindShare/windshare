package relaysetup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/windshare/windshare/engine/relayconfig"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

const maximumResponseBytes = 64 * 1024
const maximumRetry = 30 * time.Second

type apiFailure struct {
	ErrorCode string `json:"error"`
	RetryMS   int64  `json:"retry_after_ms"`
}

func (failure *apiFailure) Error() string { return "relay setup: " + failure.ErrorCode }
func (failure *apiFailure) delay() time.Duration {
	milliseconds := min(max(failure.RetryMS, time.Second.Milliseconds()), maximumRetry.Milliseconds())
	return time.Duration(milliseconds) * time.Millisecond
}
func (failure *apiFailure) temporary() bool {
	switch failure.ErrorCode {
	case "authorization_pending", "slow_down", "temporarily_unavailable":
		return true
	}
	return false
}

type discovery struct {
	Version     int    `json:"version"`
	Access      string `json:"access"`
	RelayURL    string `json:"relay_url"`
	ConsoleURL  string `json:"console_url"`
	FrontendURL string `json:"frontend_url"`
}

func (service *Service) call(ctx context.Context, method, address string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, address, reader)
	if err != nil {
		return errors.New("invalid access endpoint")
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := service.dependencies.HTTP.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &apiFailure{ErrorCode: "temporarily_unavailable", RetryMS: 5_000}
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &apiFailure{ErrorCode: "temporarily_unavailable", RetryMS: 5_000}
	}
	if len(data) > maximumResponseBytes {
		return errors.New("access service response exceeds the size limit")
	}
	if response.StatusCode == http.StatusOK || response.StatusCode == http.StatusCreated {
		if json.Unmarshal(data, out) != nil {
			return errors.New("invalid access service response")
		}
		return nil
	}
	failure := &apiFailure{}
	if response.StatusCode >= 500 {
		failure.ErrorCode = "temporarily_unavailable"
		failure.RetryMS = 5_000
	} else if json.Unmarshal(data, failure) != nil || failure.ErrorCode == "" {
		return errors.New("unsupported relay access endpoint")
	}
	switch failure.ErrorCode {
	case "access_denied":
		return ErrDenied
	case "expired_token":
		return ErrExpired
	}
	return failure
}
func (service *Service) discover(ctx context.Context, address string) (discovery, error) {
	var result discovery
	parsed, err := url.Parse(address)
	if err != nil {
		return result, errors.New("invalid relay URL")
	}
	if parsed.Scheme == "wss" {
		parsed.Scheme = "https"
	}
	if parsed.Scheme == "ws" {
		parsed.Scheme = "http"
	}
	if relayconfig.ValidateFrontURL(parsed.String()) != nil {
		return result, errors.New("relay discovery requires an HTTP(S) base URL")
	}
	parsed.Path = "/.well-known/windshare"
	if err = service.call(ctx, http.MethodGet, parsed.String(), nil, &result); err != nil {
		return result, err
	}
	if result.Version != 1 || (result.Access != "open" && result.Access != "restricted") || !sameRelay(address, result.RelayURL) {
		return result, errors.New("relay discovery returned an inconsistent identity or policy")
	}
	if result.FrontendURL != "" && relayconfig.ValidateFrontURL(result.FrontendURL) != nil {
		return result, errors.New("invalid suggested frontend URL")
	}
	if result.Access == "restricted" && !secureEndpoint(result.ConsoleURL) {
		return result, errors.New("restricted relay requires HTTPS access endpoints")
	}
	if result.Access == "restricted" && !secureEndpoint(parsed.Scheme+"://"+parsed.Host) {
		return result, errors.New("restricted relay discovery requires HTTPS")
	}
	return result, nil
}
func sameRelay(left, right string) bool {
	a, errA := v2.NormalizeRelayEndpoint(left)
	b, errB := v2.NormalizeRelayEndpoint(right)
	return errA == nil && errB == nil && a.DialURL == b.DialURL
}
func secureEndpoint(address string) bool {
	if relayconfig.ValidateFrontURL(address) != nil {
		return false
	}
	parsed, err := url.Parse(address)
	if err != nil {
		return false
	}
	return parsed.Scheme == "https" || (parsed.Scheme == "http" && (parsed.Hostname() == "localhost" || net.ParseIP(parsed.Hostname()).IsLoopback()))
}
func endpoint(base, path string) string { return strings.TrimRight(base, "/") + "/api/" + path }
