package networkprobe

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("stream reset") }
func (failingReader) Close() error             { return nil }

func TestReadAuthorityErrorParsesStructuredFaultsAndFallsBackSafely(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		action     string
		status     int
		body       io.ReadCloser
		wantKind   string
		wantReason string
		wantError  string
	}{
		{
			name:       "denied with reason",
			action:     "poll",
			status:     http.StatusForbidden,
			body:       io.NopCloser(strings.NewReader(`{"kind":"denied","reason":"unknown_vantage"}`)),
			wantKind:   "denied",
			wantReason: "unknown_vantage",
			wantError:  "authority poll: HTTP 403 (denied: unknown_vantage)",
		},
		{
			name:       "conflict with reason",
			action:     "drain",
			status:     http.StatusConflict,
			body:       io.NopCloser(strings.NewReader(`{"kind":"conflict","reason":"maintenance_in_progress"}`)),
			wantKind:   "conflict",
			wantReason: "maintenance_in_progress",
			wantError:  "authority drain: HTTP 409 (conflict: maintenance_in_progress)",
		},
		{
			name:      "kind only",
			action:    "result",
			status:    http.StatusNotFound,
			body:      io.NopCloser(strings.NewReader(`{"kind":"node_not_found"}`)),
			wantKind:  "node_not_found",
			wantError: "authority result: HTTP 404 (node_not_found)",
		},
		{
			name:      "extra fault fields ignored",
			action:    "cycle",
			status:    http.StatusTooManyRequests,
			body:      io.NopCloser(strings.NewReader(`{"kind":"rate_limited","retry_after":30}`)),
			wantKind:  "rate_limited",
			wantError: "authority cycle: HTTP 429 (rate_limited)",
		},
		{
			name:      "plain text authority 503 falls back",
			action:    "cycle",
			status:    http.StatusServiceUnavailable,
			body:      io.NopCloser(strings.NewReader("fleet temporarily unavailable")),
			wantError: "authority cycle: HTTP 503",
		},
		{
			name:      "intermediary HTML error page falls back",
			action:    "drain",
			status:    http.StatusForbidden,
			body:      io.NopCloser(strings.NewReader("<html><body>Access Denied</body></html>")),
			wantError: "authority drain: HTTP 403",
		},
		{
			name:      "malformed JSON or trailing junk falls back",
			action:    "poll",
			status:    http.StatusBadRequest,
			body:      io.NopCloser(strings.NewReader(`{"kind":"invalid"} trailing`)),
			wantError: "authority poll: HTTP 400",
		},
		{
			name:      "unsafe characters in kind or reason fall back",
			action:    "poll",
			status:    http.StatusForbidden,
			body:      io.NopCloser(strings.NewReader(`{"kind":"denied","reason":"line1\nline2"}`)),
			wantError: "authority poll: HTTP 403",
		},
		{
			name:      "overlong token falls back",
			action:    "poll",
			status:    http.StatusForbidden,
			body:      io.NopCloser(strings.NewReader(`{"kind":"denied","reason":"` + strings.Repeat("a", maxFaultTokenBytes+1) + `"}`)),
			wantError: "authority poll: HTTP 403",
		},
		{
			name:      "oversized JSON body falls back",
			action:    "poll",
			status:    http.StatusBadRequest,
			body:      io.NopCloser(strings.NewReader(`{"kind":"invalid","pad":"` + strings.Repeat("x", maxAuthorityFaultBytes) + `"}`)),
			wantError: "authority poll: HTTP 400",
		},
		{
			name:      "empty body falls back",
			action:    "status",
			status:    http.StatusInternalServerError,
			body:      io.NopCloser(strings.NewReader("")),
			wantError: "authority status: HTTP 500",
		},
		{
			name:      "nil body falls back",
			action:    "status",
			status:    http.StatusBadGateway,
			body:      nil,
			wantError: "authority status: HTTP 502",
		},
		{
			name:      "body read error falls back",
			action:    "release",
			status:    http.StatusServiceUnavailable,
			body:      failingReader{},
			wantError: "authority release: HTTP 503",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ReadAuthorityError(tc.action, &http.Response{
				StatusCode: tc.status,
				Body:       tc.body,
			})
			if got.StatusCode != tc.status || got.Kind != tc.wantKind || got.Reason != tc.wantReason || got.Error() != tc.wantError {
				t.Fatalf("ReadAuthorityError = %+v (%q), want status=%d kind=%q reason=%q err=%q",
					got, got.Error(), tc.status, tc.wantKind, tc.wantReason, tc.wantError)
			}
		})
	}
}
