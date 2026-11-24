package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const releaseTimeout = 10 * time.Second

// releaseClient follows the host's DNS policy for release-configured authority URLs.
func releaseClient() *http.Client {
	return &http.Client{Timeout: releaseTimeout, CheckRedirect: refuseRedirect}
}

func refuseRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func fetchHTTPS(ctx context.Context, client *http.Client, address string, limit int64) ([]byte, error) {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, errors.New("invalid public HTTPS URL")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.TLS == nil || len(response.TLS.VerifiedChains) == 0 {
		return nil, fmt.Errorf("unverified response (HTTP %d)", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("response too large")
	}
	return body, nil
}
