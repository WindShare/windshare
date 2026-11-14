package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relaysetup"
)

func TestNativeFrontendCommitInitializesMissingSettingsAndSurvivesRelayFailure(t *testing.T) {
	for _, relay := range []string{"", "invalid relay"} {
		t.Run(relay, func(t *testing.T) {
			store := relayconfig.FileStore{Path: filepath.Join(t.TempDir(), "settings.json")}
			result, err := newNativeSetup(store).run(t.Context(), SetupInput{
				FrontURL: "https://chosen.example", RelayURL: relay,
			}, nil)
			if !result.FrontendSaved || result.RelaySaved || (err != nil) != (relay != "") {
				t.Fatal("incorrect commit receipt", result, err)
			}
			saved, err := store.Load()
			if err != nil || saved.Validate() != nil || saved.FrontURL != "https://chosen.example" || len(saved.Profiles) != 0 {
				t.Fatal("first settings edit was not retained", saved, err)
			}
		})
	}
}

func TestNativeCancellationRevokesPendingApprovalAndPreservesFrontend(t *testing.T) {
	var cancellations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := "http://" + r.Host
		var response any
		switch r.URL.Path {
		case "/.well-known/windshare":
			response = map[string]any{"version": 1, "access": "restricted", "relay_url": origin, "console_url": origin}
		case "/api/enrollments":
			response = map[string]any{
				"enrollment_id": "approval", "user_code": "ABCD-EFGH",
				"verification_uri_complete": origin + "/approve", "expires_at": time.Now().Add(time.Minute).UnixMilli(), "interval_ms": 1000,
			}
		case "/api/enrollments/cancel":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["enrollment_id"] != "approval" || body["claim_secret"] == "" {
				t.Error("cancellation lost enrollment authority", err)
			}
			cancellations.Add(1)
			response = map[string]bool{"ok": true}
		default:
			t.Errorf("unexpected setup request: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	store := relayconfig.FileStore{Path: filepath.Join(t.TempDir(), "settings.json")}
	setup := newNativeSetup(store)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result, err := setup.run(ctx, SetupInput{RelayURL: server.URL, FrontURL: "https://chosen.example"}, func(progress relaysetup.Progress) {
		if progress.Stage == "approval_required" {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || !result.FrontendSaved || result.RelaySaved {
		t.Fatal("approval was not interrupted", result, err)
	}
	if err := setup.cancel(t.Context(), result.RelayURL); err != nil {
		t.Fatal(err)
	}
	saved, err := (relaysetup.FileStore{Settings: store}).Load()
	if err != nil || saved.Pending != nil || saved.FrontURL != "https://chosen.example" || len(saved.Profiles) != 0 || cancellations.Load() != 1 {
		t.Fatal("cancellation lost settings or retained approval", saved, err, cancellations.Load())
	}
}
