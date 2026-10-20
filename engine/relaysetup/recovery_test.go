package relaysetup

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/windshare/windshare/engine/relayconfig"
)

func expiredPending(now time.Time) *relayconfig.PendingSetup {
	return &relayconfig.PendingSetup{State: relayconfig.SetupEnrolling, SettingsRevision: prior().SettingsRevision(),
		OperationID: "operation", RelayURL: "https://private.example",
		ConsoleURL: "https://console.example", DeviceName: "Laptop", ClaimSecret: encode(make([]byte, 32)),
		Operation: string(AddProfile), EnrollmentID: "enrollment", UserCode: "ABCD-EFGH",
		VerificationURL: "https://console.example/", IntervalMS: 5000, ExpiresAt: now.Add(-time.Minute).UnixMilli()}
}
func TestExpiredEnrollmentCanBeRestartedWithoutChangingSelections(t *testing.T) {
	now := time.Unix(100, 0)
	saved := prior()
	saved.Pending = expiredPending(now)
	store := &memoryStore{saved: saved}
	service := New(Dependencies{Store: store, Now: func() time.Time { return now },
		HTTP: fakeHTTP(func(*http.Request) (*http.Response, error) {
			t.Fatal("expired enrollment contacted server")
			return nil, nil
		})})
	_, err := service.Run(t.Context(), Request{URL: saved.Pending.RelayURL, Operation: AddProfile}, nil)
	if !errors.Is(err, ErrExpired) || store.saved.Pending != nil || len(store.saved.Profiles) != 1 ||
		store.saved.ActiveRelays[0] != prior().ActiveRelays[0] {
		t.Fatalf("expired transaction remained installed or blocked: %v", err)
	}
}
func TestCancellationOfClaimedCredentialDoesNotInstallIt(t *testing.T) {
	now := time.Unix(100, 0)
	saved := prior()
	saved.Pending = expiredPending(now)
	saved.Pending.CredentialID, saved.Pending.AccessKey = "credential", "claimed-key"
	saved.Pending.State = relayconfig.SetupInstalling
	store := &memoryStore{saved: saved}
	service := New(Dependencies{Store: store, HTTP: fakeHTTP(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/api/enrollments/cancel" {
			t.Fatal(req.URL)
		}
		return response(200, map[string]bool{"ok": true})
	})})
	if err := service.Cancel(t.Context(), saved.Pending.RelayURL); err != nil {
		t.Fatal(err)
	}
	if store.saved.Pending != nil || len(store.saved.Profiles) != 1 {
		t.Fatal("cancellation installed credential")
	}
}
func TestCancellationInterruptsAnActiveApprovalAndSerializesStorage(t *testing.T) {
	now := time.Unix(100, 0)
	saved := prior()
	saved.Pending = expiredPending(now)
	saved.Pending.ExpiresAt = now.Add(time.Minute).UnixMilli()
	store := &memoryStore{saved: saved}
	waiting := make(chan struct{})
	service := New(Dependencies{Store: store, Now: func() time.Time { return now },
		HTTP: fakeHTTP(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == "/api/enrollments/cancel" {
				return response(200, map[string]bool{"ok": true})
			}
			return response(202, map[string]any{"error": "authorization_pending", "retry_after_ms": 5000})
		}),
		Wait: func(ctx context.Context, _ time.Duration) error { close(waiting); <-ctx.Done(); return ctx.Err() },
	})
	result := make(chan error, 1)
	request := Request{URL: saved.Pending.RelayURL, Operation: AddProfile, NoBrowser: true}
	go func() { _, err := service.Run(t.Context(), request, nil); result <- err }()
	<-waiting
	if _, err := service.Run(t.Context(), request, nil); !errors.Is(err, ErrSetupRunning) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := service.Cancel(ctx, request.URL); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(<-result, context.Canceled) || store.saved.Pending != nil {
		t.Fatal("approval was not cancelled")
	}
}
