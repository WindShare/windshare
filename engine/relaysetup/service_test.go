package relaysetup

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/engine/relayprofile"
)

type memoryStore struct {
	saved       Snapshot
	failInstall bool
}

func clone(config Snapshot) Snapshot {
	config.Profiles = slices.Clone(config.Profiles)
	config.ActiveRelays = slices.Clone(config.ActiveRelays)
	if config.Pending != nil {
		pending := *config.Pending
		config.Pending = &pending
	}
	return config
}
func (store *memoryStore) Load() (Snapshot, error) { return clone(store.saved), nil }
func (store *memoryStore) Save(config Snapshot) error {
	if store.failInstall && config.Pending == nil {
		return errors.New("disk unavailable")
	}
	if config.SettingsRevision() != store.saved.SettingsRevision() {
		if err := config.Config.Validate(); err != nil {
			return err
		}
	}
	if config.Pending != nil {
		if err := config.Pending.validate(); err != nil {
			return err
		}
	}
	store.saved = clone(config)
	return nil
}
func (store *memoryStore) Update(ctx context.Context, change func(*Snapshot) error) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	current := clone(store.saved)
	if err := change(&current); err != nil {
		return Snapshot{}, err
	}
	if err := store.Save(current); err != nil {
		return Snapshot{}, err
	}
	return clone(current), nil
}

type fakeHTTP func(*http.Request) (*http.Response, error)

func (client fakeHTTP) Do(request *http.Request) (*http.Response, error) { return client(request) }
func response(status int, value any) (*http.Response, error) {
	body, _ := json.Marshal(value)
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
}
func prior() Snapshot {
	return Snapshot{FrontURL: "https://chosen.example", Profiles: []relayprofile.Candidate{{PublicURL: "https://public.example"}},
		ActiveRelays: []string{"https://public.example"}}
}
func TestOrdinaryAdditionPreservesSelectionAndFrontend(t *testing.T) {
	store := &memoryStore{saved: prior()}
	service := New(Dependencies{Store: store, HTTP: fakeHTTP(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/.well-known/windshare" {
			t.Fatal(req.URL)
		}
		return response(200, map[string]any{"version": 1, "access": "open", "relay_url": "https://open.example", "frontend_url": "https://suggested.example"})
	})})
	result, err := service.Run(t.Context(), Request{URL: "https://open.example", Operation: AddProfile}, nil)
	if err != nil || len(store.saved.Profiles) != 2 || result.FrontURL != "https://chosen.example" ||
		!slices.Equal(result.ActiveRelays, []string{"https://public.example"}) {
		t.Fatalf("unexpected installation: %+v %v", result, err)
	}
	store.saved.ActiveRelays = nil
	result, err = service.Run(t.Context(), Request{URL: "https://open.example", Operation: AddProfile}, nil)
	if err != nil || len(result.ActiveRelays) != 0 {
		t.Fatalf("ordinary addition replaced an explicit empty selection: %+v %v", result, err)
	}
	if _, err = service.Run(t.Context(), Request{URL: "https://open.example", Operation: PrivateOnboarding}, nil); err == nil {
		t.Fatal("open relay accepted as private onboarding")
	}
}
func TestPrivateEnrollmentResumesClaimedInstallationWithoutChangingSelectionsEarly(t *testing.T) {
	store := &memoryStore{saved: prior(), failInstall: true}
	now := time.Unix(100, 0)
	claims, confirmations, requests, waits := 0, 0, 0, 0
	var claimDigest string
	client := fakeHTTP(func(req *http.Request) (*http.Response, error) {
		var body map[string]string
		if req.Body != nil {
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
		}
		if req.Header.Get("Authorization") != "" {
			t.Fatal("setup acquired administrator authority")
		}
		switch req.URL.Path {
		case "/.well-known/windshare":
			return response(200, map[string]any{"version": 1, "access": "restricted", "relay_url": "https://private.example",
				"console_url": "https://console.example", "frontend_url": "https://suggested.example"})
		case "/api/enrollments":
			requests++
			claimDigest = body["claim_digest"]
			if store.saved.Pending == nil || claimDigest == "" {
				t.Fatal("request not durably bound to client")
			}
			return response(200, map[string]any{"enrollment_id": "enrollment", "user_code": "ABCD-EFGH", "verification_uri_complete": "https://console.example/?code=ABCD-EFGH", "expires_at": now.Add(time.Minute).UnixMilli(), "interval_ms": 5000})
		case "/api/enrollments/claim":
			claims++
			if claims == 1 {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(interruptedBody{})}, nil
			}
			if claims == 2 {
				return response(503, map[string]any{"error": "temporarily_unavailable", "retry_after_ms": 1000})
			}
			if body["claim_secret"] == "" || store.saved.Pending.enrollment().ID != "enrollment" {
				t.Fatal("claim preceded durable enrollment")
			}
			return response(200, map[string]string{"access_key": "client-only-key", "credential_id": "credential"})
		case "/api/enrollments/installed":
			confirmations++
			if store.saved.Pending.credential().AccessKey != "client-only-key" || len(store.saved.Profiles) != 1 ||
				!slices.Equal(store.saved.ActiveRelays, []string{"https://public.example"}) {
				t.Fatal("incomplete setup changed the active selection")
			}
			return response(200, map[string]bool{"ok": true})
		default:
			t.Fatalf("unexpected endpoint: %s", req.URL)
			return nil, nil
		}
	})
	dependencies := Dependencies{Store: store, HTTP: client, Now: func() time.Time { return now },
		Wait: func(_ context.Context, delay time.Duration) error {
			waits++
			if delay < 5*time.Second || delay > maximumRetry {
				t.Fatal(delay)
			}
			now = now.Add(delay)
			return nil
		}}
	request := Request{URL: "https://private.example", DeviceName: "Laptop", Operation: PrivateOnboarding}
	if _, err := New(dependencies).Run(t.Context(), request, nil); err == nil {
		t.Fatal("disk failure was not surfaced")
	}
	if store.saved.Pending == nil || store.saved.Pending.credential().AccessKey != "client-only-key" || len(store.saved.Profiles) != 1 {
		t.Fatal("lost pending installation")
	}
	store.failInstall = false
	result, err := New(dependencies).Run(t.Context(), request, nil)
	if err != nil || result.CredentialID != "credential" || requests != 1 || claims != 3 || confirmations != 2 || waits != 2 {
		t.Fatalf("recovery repeated enrollment: %+v %v", result, err)
	}
	if len(store.saved.Profiles) != 2 || store.saved.Pending != nil || store.saved.FrontURL != "https://chosen.example" ||
		!slices.Equal(store.saved.ActiveRelays, []string{"https://private.example"}) {
		t.Fatal("private selection did not commit atomically")
	}
}
func TestCancelledRequestKeepsPriorConfigurationAndCanBeExplicitlyAbandoned(t *testing.T) {
	store := &memoryStore{saved: prior()}
	now := time.Unix(100, 0)
	client := fakeHTTP(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/.well-known/windshare":
			return response(200, map[string]any{"version": 1, "access": "restricted", "relay_url": "https://private.example", "console_url": "https://console.example"})
		case "/api/enrollments":
			return response(200, map[string]any{"enrollment_id": "pending", "user_code": "ABCD-EFGH", "verification_uri_complete": "https://console.example/", "expires_at": now.Add(time.Minute).UnixMilli(), "interval_ms": 5000})
		case "/api/enrollments/claim":
			return response(202, map[string]any{"error": "authorization_pending", "retry_after_ms": 5000})
		case "/api/enrollments/cancel":
			return response(200, map[string]bool{"ok": true})
		default:
			t.Fatal(req.URL)
			return nil, nil
		}
	})
	service := New(Dependencies{Store: store, HTTP: client, Now: func() time.Time { return now }, Wait: func(context.Context, time.Duration) error { return context.Canceled }})
	if _, err := service.Run(t.Context(), Request{URL: "https://private.example", DeviceName: "Laptop", Operation: AddProfile}, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if store.saved.Pending == nil || len(store.saved.Profiles) != 1 {
		t.Fatal("interruption lost transaction or changed configuration")
	}
	if err := service.Cancel(t.Context(), "https://private.example"); err != nil || store.saved.Pending != nil || len(store.saved.Profiles) != 1 {
		t.Fatal(err)
	}
}
