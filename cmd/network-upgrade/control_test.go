package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/windshare/windshare/internal/networkprobe"
)

func TestMaintenanceHTTPBindsRequestsAndRejectsReassignedProbes(t *testing.T) {
	const token = "test-maintenance-only-credential"
	op := operation{NodeKey: "enrolled-key", ID: "runner-operation"}
	authority := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "POST" || req.Header.Get("Authorization") != "Bearer "+token {
			t.Error("missing scoped credential")
		}
		switch req.URL.Path {
		case "/v1/maintenance/status":
			var got operation
			if err := json.NewDecoder(req.Body).Decode(&got); err != nil || got != op {
				t.Errorf("unexpected operation: %+v", got)
			}
			_ = json.NewEncoder(w).Encode(drainStatus{Held: true, Confirmed: true, Version: 19})
		case "/v1/maintenance/probe":
			var got struct {
				Operation operation
				Shares    [][12]byte
			}
			if err := json.NewDecoder(req.Body).Decode(&got); err != nil || got.Operation != op || len(got.Shares) != networkprobe.OriginCount || got.Shares[0] == got.Shares[1] {
				t.Errorf("unbound probe: %+v", got)
			}
			_ = json.NewEncoder(w).Encode(networkprobe.Work{OperationID: "another-operation"})
		default:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}
	}))
	defer authority.Close()
	r := runner{authority: authority.URL, api: authority.Client(), token: token, op: op}
	status, err := r.policy(t.Context(), "status")
	if err != nil || !status.Held || !status.Confirmed || status.Version != 19 {
		t.Fatalf("%+v, %v", status, err)
	}
	if err := r.verify(t.Context()); err == nil || !strings.Contains(err.Error(), "assignment changed") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := r.policy(t.Context(), "release"); err == nil {
		t.Fatal("HTTP failure accepted")
	}
}

func TestRunnerRejectsUnboundInputsBeforeSSH(t *testing.T) {
	key := base64.RawURLEncoding.EncodeToString(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)).Public().(ed25519.PublicKey))
	rev := strings.Repeat("a", 40)
	op := strings.Repeat("b", 32)
	path := filepath.Join(t.TempDir(), "missing-binary")
	t.Setenv("FLEET_MAINTENANCE_TOKEN", strings.Repeat("c", 42))
	for _, args := range [][6]string{
		{"-ssh", key, path, rev, "", ""},
		{"relay", key, path, "bad", "", ""},
		{"relay", "bad", path, rev, "", ""},
		{"relay", key, path, rev, op, ""},
		{"relay", key, "", "", "bad", ""},
		{"relay", key, path, rev, "", "http://staging.example"},
		{"relay", key, path, rev, "", "https://staging.example/v1"},
		{"relay", key, path, rev, "", "https://user:pass@staging.example"},
	} {
		if err := run(args[0], args[1], args[2], args[3], args[4], args[5]); err == nil {
			t.Fatalf("accepted invalid identifiers: %q", args)
		}
	}
	if err := run("relay", key, path, rev, "", ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preflight: %v", err)
	}
	if err := run("relay", key, path, rev, "", "https://staging-authority.example.workers.dev/"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preflight with authority override: %v", err)
	}
}

func TestResolveAuthorityDefaultAndOverride(t *testing.T) {
	defaultURL, err := resolveAuthority("")
	if err != nil || !strings.HasPrefix(defaultURL, "https://") {
		t.Fatalf("default authority = (%q, %v)", defaultURL, err)
	}
	overrideURL, err := resolveAuthority("https://authority.windshare.cc/")
	if err != nil || overrideURL != "https://authority.windshare.cc" {
		t.Fatalf("override authority = (%q, %v)", overrideURL, err)
	}
	for _, bad := range []string{"http://authority.windshare.cc", "https://authority.windshare.cc/v1", "https://authority.windshare.cc?a=1"} {
		if _, err := resolveAuthority(bad); err == nil {
			t.Fatalf("accepted invalid authority override %q", bad)
		}
	}
}

func TestRecoveryRequiresOwnershipAndVerifiesCurrentSelection(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f := &fakeTarget{}
	if err := (rollout{maintenanceTarget: f}).recover(context.Background(), log); err == nil || !slices.Equal(f.calls, []string{"authority status"}) {
		t.Fatalf("foreign recovery %v: %v", f.calls, err)
	}
	f.held = true
	f.calls = nil
	if err := (rollout{maintenanceTarget: f}).recover(t.Context(), log); err != nil || !slices.Equal(f.calls, []string{"authority status", "host recover", "host inspect", "probe", "host release", "authority release"}) {
		t.Fatalf("recovery %v: %v", f.calls, err)
	}
}

func TestParseRecoveryState(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want recoveryState
		ok   bool
	}{
		{"held\n", recoveryHeld, true},
		{"released\r\n", recoveryReleased, true},
		{"", "", false},
		{"unexpected\n", "", false},
		{"banner\nreleased\n", "", false},
	} {
		got, err := parseRecoveryState(tc.raw)
		if (err == nil) != tc.ok || got != tc.want {
			t.Fatalf("parseRecoveryState(%q) = (%q, %v), want (%q, ok=%v)", tc.raw, got, err, tc.want, tc.ok)
		}
	}
}

func TestMaintenanceCallSurfacesAuthorityFaultJSONAndFallsBackOnPlainText(t *testing.T) {
	t.Parallel()
	const token = "test-maintenance-only-credential"
	op := operation{NodeKey: "enrolled-key", ID: "runner-operation"}
	cases := []struct {
		action     string
		status     int
		body       string
		wantKind   string
		wantReason string
		wantError  string
	}{
		{
			action:     "drain",
			status:     http.StatusConflict,
			body:       `{"kind":"conflict","reason":"maintenance_in_progress"}`,
			wantKind:   "conflict",
			wantReason: "maintenance_in_progress",
			wantError:  "authority drain: HTTP 409 (conflict: maintenance_in_progress)",
		},
		{
			action:     "release",
			status:     http.StatusConflict,
			body:       `{"kind":"conflict","reason":"maintenance_owner"}`,
			wantKind:   "conflict",
			wantReason: "maintenance_owner",
			wantError:  "authority release: HTTP 409 (conflict: maintenance_owner)",
		},
		{
			action:     "status",
			status:     http.StatusForbidden,
			body:       `{"kind":"denied","reason":"maintenance_credential"}`,
			wantKind:   "denied",
			wantReason: "maintenance_credential",
			wantError:  "authority status: HTTP 403 (denied: maintenance_credential)",
		},
		{
			action:    "probe",
			status:    http.StatusServiceUnavailable,
			body:      "fleet temporarily unavailable",
			wantError: "authority probe: HTTP 503",
		},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			r := runner{authority: server.URL, api: server.Client(), token: token, op: op}
			err := r.call(t.Context(), tc.action, op, nil)
			var authorityErr *networkprobe.AuthorityError
			if !errors.As(err, &authorityErr) {
				t.Fatalf("error = %T (%v), want *networkprobe.AuthorityError", err, err)
			}
			if authorityErr.StatusCode != tc.status || authorityErr.Kind != tc.wantKind || authorityErr.Reason != tc.wantReason || err.Error() != tc.wantError {
				t.Fatalf("error = %+v (%q), want status=%d kind=%q reason=%q err=%q",
					authorityErr, err.Error(), tc.status, tc.wantKind, tc.wantReason, tc.wantError)
			}
		})
	}
}
