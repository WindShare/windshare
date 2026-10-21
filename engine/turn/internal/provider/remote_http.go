package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// ManagedEndpoint is anchored to the configured deployment. Discovery cannot
// redirect a device's access key to an arbitrary credential issuer.
func ManagedEndpoint(address string) (string, error) {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") {
		return "", ErrConfiguration
	}
	switch parsed.Scheme {
	case "wss":
		parsed.Scheme = "https"
	case "ws":
		parsed.Scheme = "http"
	}
	loopback := parsed.Hostname() == "localhost" || net.ParseIP(parsed.Hostname()).IsLoopback()
	if parsed.Scheme != "https" && (parsed.Scheme != "http" || !loopback) {
		return "", ErrConfiguration
	}
	parsed.Path = "/api/turn"
	return strings.TrimRight(parsed.String(), "/"), nil
}
func remoteHTTPClient() *http.Client {
	return &http.Client{Timeout: HTTPTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func Discover(ctx context.Context, client HTTPClient, relayURL, accessKey string) (*Capability, error) {
	base, err := ManagedEndpoint(relayURL)
	if err != nil || accessKey == "" {
		return nil, ErrConfiguration
	}
	if client == nil {
		client = remoteHTTPClient()
	}
	var result struct {
		TURN *Capability `json:"turn"`
	}
	if err := remoteCall(ctx, client, base+"/capability", accessKey, nil, &result); err != nil {
		return nil, err
	}
	if result.TURN != nil && !ValidServiceID(result.TURN.ServiceID) {
		return nil, ErrConfiguration
	}
	return result.TURN, nil
}
func remoteCall(ctx context.Context, client HTTPClient, address, key string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, HTTPTimeout)
	defer cancel()
	var reader io.Reader
	method := http.MethodGet
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader, method = bytes.NewReader(data), http.MethodPost
	}
	request, err := http.NewRequestWithContext(ctx, method, address, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+key)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusUnauthorized {
		return ErrAuthorityWithdrawn
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusAccepted {
		return fmt.Errorf("managed TURN request returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, MaximumResponseBytes+1))
	if err != nil {
		return err
	}
	if len(data) > MaximumResponseBytes {
		return ErrConfiguration
	}
	if err := json.Unmarshal(data, out); err != nil {
		return ErrConfiguration
	}
	return nil
}
