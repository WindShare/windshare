package relaysetup

import (
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/engine/relayconfig"
)

// Invalid input must leave durable storage readable, including when JSON would
// otherwise replace invalid UTF-8 and expand a name beyond the byte limit.
func TestRestrictedSetupRejectsInvalidDeviceNameBeforeEnrollment(t *testing.T) {
	for _, operation := range []Operation{AddProfile, PrivateOnboarding} {
		for _, test := range []struct{ name, input string }{
			{"empty", ""},
			{"blank", "   "},
			{"too long", strings.Repeat("a", maximumDeviceNameBytes+1)},
			{"tab", "Lap\ttop"},
			{"newline", "Laptop\n"},
			{"invalid UTF-8", "\xff"},
			{"UTF-8 expansion at limit", strings.Repeat("a", maximumDeviceNameBytes-1) + "\xff"},
		} {
			t.Run(string(operation)+"/"+test.name, func(t *testing.T) {
				settings := relayconfig.FileStore{Path: filepath.Join(t.TempDir(), "config.json")}
				initial := prior()
				if err := settings.Save(initial.Config); err != nil {
					t.Fatal(err)
				}
				store := FileStore{Settings: settings}
				enrollments := 0
				client := fakeHTTP(func(req *http.Request) (*http.Response, error) {
					switch req.URL.Path {
					case "/.well-known/windshare":
						return response(http.StatusOK, map[string]any{"version": 1, "access": "restricted",
							"relay_url": "https://private.example", "console_url": "https://console.example"})
					case "/api/enrollments":
						enrollments++
						return response(http.StatusBadRequest, map[string]string{"error": "invalid_name"})
					default:
						t.Fatalf("unexpected endpoint: %s", req.URL)
						return nil, nil
					}
				})
				service := New(Dependencies{Store: store, HTTP: client})
				_, err := service.Run(t.Context(), Request{URL: "https://private.example",
					DeviceName: test.input, Operation: operation}, nil)
				if !errors.Is(err, ErrInvalidDeviceName) {
					t.Fatalf("error = %v, want ErrInvalidDeviceName", err)
				}
				saved, err := store.Load()
				if err != nil || enrollments != 0 || saved.Pending != nil ||
					saved.SettingsRevision() != initial.SettingsRevision() {
					t.Fatalf("rejected input changed setup: requests=%d pending=%v load=%v",
						enrollments, saved.Pending != nil, err)
				}
				if err := service.Cancel(t.Context(), "https://private.example"); !errors.Is(err, ErrNoPendingSetup) {
					t.Fatalf("rejected input blocked cancellation: %v", err)
				}
			})
		}
	}
}

func TestDeviceNameCheckpointRoundTrip(t *testing.T) {
	for _, name := range []string{
		strings.Repeat("a", maximumDeviceNameBytes),
		strings.Repeat("界", 42) + "ab",
		"Ricky's 笔记本",
		"  My laptop  ",
		"\ufffd",
	} {
		pending := expiredPending(time.Now())
		pending.DeviceName = name
		encoded, err := json.Marshal(pending)
		if err != nil {
			t.Fatal(err)
		}
		var restored PendingSetup
		if err := json.Unmarshal(encoded, &restored); err != nil || restored.DeviceName != name {
			t.Fatalf("name did not round-trip unchanged: before=%q after=%q error=%v",
				name, restored.DeviceName, err)
		}
	}
}

func TestDeviceNameCheckpointRejectsInvalidUTF8(t *testing.T) {
	for _, name := range []string{"\xff", strings.Repeat("a", maximumDeviceNameBytes-1) + "\xff"} {
		pending := expiredPending(time.Now())
		pending.DeviceName = name
		if _, err := json.Marshal(pending); err == nil {
			t.Fatal("checkpoint accepted a name JSON would silently replace")
		}
	}
}
