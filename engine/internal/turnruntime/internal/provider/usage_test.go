package provider

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCloudflareUsageIsExplicitSampledAndSeparatelyAuthorized(t *testing.T) {
	requests := 0
	source, err := NewCloudflareUsage(CloudflareUsageConfig{AccountID: strings.Repeat("a", 32), KeyID: strings.Repeat("b", 32), APIToken: "analytics-only",
		Client: clientFunc(func(request *http.Request) (*http.Response, error) {
			requests++
			if request.Header.Get("Authorization") != "Bearer analytics-only" || request.URL.String() != CloudflareAnalyticsEndpoint {
				t.Fatal("incorrect analytics authority")
			}
			body, _ := io.ReadAll(request.Body)
			if !strings.Contains(string(body), "callsTurnUsageAdaptiveGroups") || !strings.Contains(string(body), "2026-10-20") {
				t.Fatal("incorrect date query")
			}
			return response(200, `{"data":{"viewer":{"accounts":[{"callsTurnUsageAdaptiveGroups":[{"sum":{"egressBytes":1234,"ingressBytes":567}}]}]}}}`), nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatal("setup queried billing")
	}
	date := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	usage, err := source.Query(context.Background(), date, date)
	if err != nil || !usage.Sampled || usage.EgressBytes != 1234 || usage.IngressBytes != 567 {
		t.Fatal(usage, err)
	}
	if _, err := source.Query(context.Background(), date, date.Add(-time.Hour)); err == nil {
		t.Fatal("inverted window")
	}
	source.config.Client = clientFunc(func(*http.Request) (*http.Response, error) {
		return response(200, `{"errors":[{"message":"rate limited"}]}`), nil
	})
	if _, err := source.Query(context.Background(), date, date); err == nil {
		t.Fatal("missing usage became zero bill")
	}
}
