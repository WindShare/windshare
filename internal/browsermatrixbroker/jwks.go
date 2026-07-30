package browsermatrixbroker

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"
)

const defaultJWKSRequestTimeout = 10 * time.Second

type HTTPJWKSFetcher struct {
	client *http.Client
}

func NewHTTPJWKSFetcher(client *http.Client) *HTTPJWKSFetcher {
	if client == nil {
		client = &http.Client{Timeout: defaultJWKSRequestTimeout}
	}
	owned := *client
	if owned.Timeout <= 0 {
		owned.Timeout = defaultJWKSRequestTimeout
	}
	owned.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("credential broker JWKS redirect is forbidden")
	}
	return &HTTPJWKSFetcher{client: &owned}
}

func (fetcher *HTTPJWKSFetcher) Fetch(ctx context.Context, endpoint string) ([]byte, error) {
	if fetcher == nil || fetcher.client == nil || !canonicalHTTPSURL(endpoint) {
		return nil, errors.New("credential broker JWKS endpoint is invalid")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errors.New("credential broker JWKS request is invalid")
	}
	request.Header.Set("Accept", "application/jwk-set+json, application/json")
	response, err := fetcher.client.Do(request)
	if err != nil {
		return nil, errors.New("credential broker JWKS request failed")
	}
	defer response.Body.Close() //nolint:errcheck // A failed read already rejects this ephemeral response.
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != http.StatusOK || response.TLS == nil || response.Request.URL.String() != endpoint ||
		(mediaType != "application/json" && mediaType != "application/jwk-set+json") || mediaErr != nil ||
		response.ContentLength > maximumJWKSBytes {
		return nil, errors.New("credential broker JWKS response is invalid")
	}
	document, err := io.ReadAll(io.LimitReader(response.Body, maximumJWKSBytes+1))
	if err != nil || len(document) == 0 || len(document) > maximumJWKSBytes {
		erase(document)
		return nil, errors.New("credential broker JWKS response exceeded its authority")
	}
	return document, nil
}
