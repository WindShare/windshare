package relaysetup

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
)

func pendingCredential(now time.Time) Snapshot {
	saved := prior()
	saved.Pending = expiredPending(now)
	saved.Pending = saved.Pending.withPhase(installing{Enrollment: saved.Pending.enrollment(), Credential: credential{ID: "credential", AccessKey: "claimed-key"}})
	setExpiry(saved.Pending, now.Add(time.Minute).UnixMilli())
	return saved
}

func TestImportDuringApprovalCannotBeOverwrittenByCheckpointsOrInstallation(t *testing.T) {
	for _, operation := range []Operation{AddProfile, PrivateOnboarding} {
		t.Run(string(operation), func(t *testing.T) {
			settings := relayconfig.FileStore{Path: filepath.Join(t.TempDir(), "config.json")}
			store := FileStore{Settings: settings}
			if err := settings.Save(prior().Config); err != nil {
				t.Fatal(err)
			}
			now := time.Unix(100, 0)
			saved := prior()
			saved.Pending = expiredPending(now)
			setExpiry(saved.Pending, now.Add(time.Minute).UnixMilli())
			saved.Pending.Operation = operation
			if _, err := store.Update(t.Context(), func(current *Snapshot) error {
				current.Pending = saved.Pending
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			imported := relayconfig.Config{FrontURL: "https://new-front.example",
				Profiles:     []relayprofile.Candidate{{PublicURL: "https://only-private.example"}},
				ActiveRelays: []string{"https://only-private.example"}}
			confirmations := 0
			service := New(Dependencies{Store: store, Now: func() time.Time { return now },
				HTTP: fakeHTTP(func(req *http.Request) (*http.Response, error) {
					switch req.URL.Path {
					case "/api/enrollments/claim":
						// A distinct store models a separate CLI importer. The OS lock
						// must be released throughout approval and HTTP calls.
						if err := (relayconfig.FileStore{Path: settings.Path}).Save(imported); err != nil {
							t.Fatal(err)
						}
						return response(200, map[string]string{"credential_id": "credential", "access_key": "key"})
					case "/api/enrollments/installed":
						confirmations++
						return response(200, map[string]bool{"ok": true})
					case "/api/enrollments/cancel":
						return response(200, map[string]bool{"ok": true})
					default:
						t.Fatalf("unexpected request: %s", req.URL)
						return nil, nil
					}
				})})
			_, err := service.Run(t.Context(), Request{URL: saved.Pending.RelayURL, Operation: operation}, nil)
			if !errors.Is(err, ErrSettingsChanged) || confirmations != 0 {
				t.Fatalf("stale installation was acknowledged: %v, confirmations=%d", err, confirmations)
			}
			current, err := store.Load()
			if err != nil || current.SettingsRevision() != imported.SettingsRevision() ||
				current.Pending == nil || current.Pending.credential().AccessKey != "key" {
				t.Fatalf("import or cancellation authority was lost: %+v, %v", current, err)
			}
			if err := service.Cancel(t.Context(), saved.Pending.RelayURL); err != nil {
				t.Fatal(err)
			}
			current, err = store.Load()
			if err != nil || current.Pending != nil || current.SettingsRevision() != imported.SettingsRevision() {
				t.Fatalf("cancellation reverted imported settings: %+v, %v", current, err)
			}
		})
	}
}

func TestProfileCapacityIsCheckedBeforeEnrollmentAndAcknowledgement(t *testing.T) {
	now := time.Unix(100, 0)
	full := prior()
	for i := len(full.Profiles); i < relayconfig.MaximumProfiles; i++ {
		full.Profiles = append(full.Profiles, relayprofile.Candidate{PublicURL: fmt.Sprintf("https://relay-%d.example", i)})
	}
	for _, claimed := range []bool{false, true} {
		t.Run(fmt.Sprint(claimed), func(t *testing.T) {
			saved := clone(full)
			if claimed {
				saved.Pending = pendingCredential(now).Pending
				saved.Pending.SettingsRevision = saved.SettingsRevision()
			}
			store := &memoryStore{saved: saved}
			service := New(Dependencies{Store: store, Now: func() time.Time { return now },
				HTTP: fakeHTTP(func(req *http.Request) (*http.Response, error) {
					if req.URL.Path == "/.well-known/windshare" {
						return response(200, map[string]any{"version": 1, "access": "restricted",
							"relay_url": "https://private.example", "console_url": "https://console.example"})
					}
					if req.URL.Path == "/api/enrollments/cancel" {
						return response(200, map[string]bool{"ok": true})
					}
					t.Fatalf("capacity failure contacted enrollment endpoint: %s", req.URL)
					return nil, nil
				})})
			_, err := service.Run(t.Context(), Request{URL: "https://private.example", DeviceName: "Laptop", Operation: AddProfile}, nil)
			if !errors.Is(err, relayconfig.ErrConfig) {
				t.Fatal(err)
			}
			if err := service.Cancel(t.Context(), "https://private.example"); err != nil {
				t.Fatal(err)
			}
			if store.saved.Pending != nil || len(store.saved.Profiles) != relayconfig.MaximumProfiles {
				t.Fatal("capacity failure could not be abandoned without changing settings")
			}
		})
	}
}

func TestAcknowledgedDiskFailureCanBeCancelledAndCancellationCanResume(t *testing.T) {
	now := time.Unix(100, 0)
	store := &memoryStore{saved: pendingCredential(now), failInstall: true}
	confirmed, cancellationRequests := false, 0
	dependencies := Dependencies{Store: store, HTTP: fakeHTTP(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/enrollments/installed":
			confirmed = true
			return response(200, map[string]bool{"ok": true})
		case "/api/enrollments/cancel":
			cancellationRequests++
			if !confirmed || store.saved.Pending.phase.stage() != (cancelling{}).stage() {
				t.Fatal("cancellation lost acknowledgement or durable cancellation fence")
			}
			return response(200, map[string]bool{"ok": true})
		default:
			t.Fatal(req.URL)
			return nil, nil
		}
	})}
	service := New(dependencies)
	if _, err := service.Run(t.Context(), Request{URL: "https://private.example", Operation: AddProfile}, nil); err == nil || !confirmed {
		t.Fatalf("expected acknowledged local failure: %v", err)
	}
	if err := service.Cancel(t.Context(), "https://private.example"); err == nil {
		t.Fatal("expected failure to clear cancellation on disk")
	}
	store.failInstall = false
	// Restarting resumes cancellation instead of accidentally installing the key.
	if _, err := New(dependencies).Run(t.Context(), Request{URL: "https://private.example", Operation: AddProfile}, nil); !errors.Is(err, ErrSetupSuperseded) {
		t.Fatal(err)
	}
	if cancellationRequests != 2 || store.saved.Pending != nil || !slices.Equal(store.saved.Profiles, prior().Profiles) {
		t.Fatal("abandoned installation changed settings or remained blocking")
	}
}

func TestAnotherServiceCancelsWhileAcknowledgementIsInFlight(t *testing.T) {
	store := &memoryStore{saved: pendingCredential(time.Unix(100, 0))}
	var dependencies Dependencies
	dependencies.Store = store
	dependencies.HTTP = fakeHTTP(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/api/enrollments/installed" {
			if err := New(dependencies).Cancel(t.Context(), "https://private.example"); err != nil {
				t.Fatal(err)
			}
		}
		return response(200, map[string]bool{"ok": true})
	})
	_, err := New(dependencies).Run(context.Background(), Request{URL: "https://private.example", Operation: AddProfile}, nil)
	if !errors.Is(err, ErrSetupSuperseded) || store.saved.Pending != nil || !slices.Equal(store.saved.Profiles, prior().Profiles) {
		t.Fatalf("late acknowledgement resurrected cancelled installation: %v", err)
	}
}

func TestSettingsChangedDuringAcknowledgementCannotPublishOldIntent(t *testing.T) {
	store := &memoryStore{saved: pendingCredential(time.Unix(100, 0))}
	imported := prior()
	imported.FrontURL = "https://later-choice.example"
	service := New(Dependencies{Store: store, HTTP: fakeHTTP(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/api/enrollments/installed" {
			store.saved.FrontURL = imported.FrontURL
		}
		return response(200, map[string]bool{"ok": true})
	})})
	_, err := service.Run(t.Context(), Request{URL: "https://private.example", Operation: AddProfile}, nil)
	if !errors.Is(err, ErrSettingsChanged) || store.saved.SettingsRevision() != imported.SettingsRevision() {
		t.Fatalf("acknowledgement bypassed the settings conflict: %v", err)
	}
	if err := service.Cancel(t.Context(), "https://private.example"); err != nil || store.saved.Pending != nil {
		t.Fatalf("acknowledged conflict could not be abandoned: %v", err)
	}
}

func TestExpiryAfterClaimClearsTheCurrentCheckpoint(t *testing.T) {
	now := time.Unix(100, 0)
	saved := prior()
	saved.Pending = expiredPending(now)
	setExpiry(saved.Pending, now.Add(time.Minute).UnixMilli())
	store := &memoryStore{saved: saved}
	service := New(Dependencies{Store: store, Now: func() time.Time { return now },
		HTTP: fakeHTTP(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == "/api/enrollments/claim" {
				return response(200, map[string]string{"credential_id": "credential", "access_key": "key"})
			}
			return response(410, map[string]string{"error": "expired_token"})
		})})
	_, err := service.Run(t.Context(), Request{URL: "https://private.example", Operation: AddProfile}, nil)
	if !errors.Is(err, ErrExpired) || errors.Is(err, ErrSetupSuperseded) || store.saved.Pending != nil {
		t.Fatalf("expiry tried to clear a stale approval checkpoint: %v", err)
	}
}
