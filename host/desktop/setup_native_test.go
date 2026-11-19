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
				FrontURL: "https://chosen.example", RelayURL: relay, RelayOperation: relaysetup.AddProfile,
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

func TestNativeOpenRelayCanBeSavedBeforeAuthorizingTheDevice(t *testing.T) {
	const accessKey = "device-credential"
	var enrollments atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := "http://" + r.Host
		var response any
		switch r.URL.Path {
		case "/.well-known/windshare":
			response = map[string]any{"version": 1, "access": "open", "relay_url": origin, "console_url": origin}
		case "/api/enrollments":
			enrollments.Add(1)
			response = map[string]any{
				"enrollment_id": "approval", "user_code": "ABCD-EFGH", "verification_uri_complete": origin + "/approve",
				"expires_at": time.Now().Add(time.Minute).UnixMilli(), "interval_ms": 1000,
			}
		case "/api/enrollments/claim":
			response = map[string]string{"access_key": accessKey, "credential_id": "device"}
		case "/api/enrollments/installed":
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
	input := SetupInput{RelayURL: server.URL, RelayOperation: relaysetup.AddProfile}
	result, err := setup.run(t.Context(), input, nil)
	if err != nil || !result.RelaySaved || enrollments.Load() != 0 {
		t.Fatal("anonymous addition initiated approval or failed to save", result, err)
	}
	saved, err := store.Load()
	if err != nil || len(saved.Profiles) != 1 || saved.Profiles[0].ConsoleURL != server.URL || saved.Profiles[0].AccessKey != "" {
		t.Fatal("anonymous profile did not retain the console for later authorization", err)
	}
	input.RelayOperation = relaysetup.AuthorizeDevice
	var approvalURL string
	result, err = setup.run(t.Context(), input, func(progress relaysetup.Progress) {
		if progress.Stage == "approval_required" {
			approvalURL = progress.VerificationURL
		}
	})
	if err != nil || !result.RelaySaved || enrollments.Load() != 1 || approvalURL != server.URL+"/approve" {
		t.Fatal("explicit authorization did not complete device enrollment", result, err)
	}
	saved, err = store.Load()
	if err != nil || len(saved.Profiles) != 1 || saved.Profiles[0].AccessKey != accessKey {
		t.Fatal("authorization did not update the saved relay's credential", err)
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
	result, err := setup.run(ctx, SetupInput{RelayURL: server.URL, RelayOperation: relaysetup.AddProfile, FrontURL: "https://chosen.example"}, func(progress relaysetup.Progress) {
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
