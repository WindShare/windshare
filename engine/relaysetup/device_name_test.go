package relaysetup

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// A device name the checkpoint would reject must be refused before any
// enrollment request, with a diagnostic that names the offending input
// instead of an opaque checkpoint serialization failure.
func TestRestrictedSetupRejectsUnsavableDeviceNameBeforeEnrollment(t *testing.T) {
	for _, name := range []string{
		"",
		"   ",
		strings.Repeat("a", maximumDeviceNameBytes+1),
		"Lap\ttop",
		"Laptop\n",
	} {
		t.Run(strings.ReplaceAll(name, "\n", `\n`), func(t *testing.T) {
			store := &memoryStore{saved: prior()}
			enrollments := 0
			client := fakeHTTP(func(req *http.Request) (*http.Response, error) {
				switch req.URL.Path {
				case "/.well-known/windshare":
					return response(200, map[string]any{"version": 1, "access": "restricted", "relay_url": "https://private.example", "console_url": "https://console.example"})
				case "/api/enrollments":
					enrollments++
					return response(200, map[string]any{"enrollment_id": "pending", "user_code": "ABCD-EFGH", "verification_uri_complete": "https://console.example/", "expires_at": time.Now().Add(time.Minute).UnixMilli(), "interval_ms": 5000})
				default:
					t.Fatalf("unexpected endpoint: %s", req.URL)
					return nil, nil
				}
			})
			service := New(Dependencies{Store: store, HTTP: client})
			_, err := service.Run(t.Context(), Request{URL: "https://private.example", DeviceName: name, Operation: AddProfile}, nil)
			if err == nil {
				t.Fatal("unsavable device name was accepted")
			}
			if !strings.Contains(err.Error(), "device name") || strings.Contains(err.Error(), "checkpoint") {
				t.Fatalf("diagnostic does not name the device name: %v", err)
			}
			if enrollments != 0 || store.saved.Pending != nil {
				t.Fatalf("rejected name still started enrollment: requests=%d pending=%v", enrollments, store.saved.Pending != nil)
			}
		})
	}
}

// Names the checkpoint accepts must keep working, including the longest one.
func TestRestrictedSetupAcceptsLongestSavableDeviceName(t *testing.T) {
	if err := validateDeviceName(strings.Repeat("a", maximumDeviceNameBytes)); err != nil {
		t.Fatal(err)
	}
	if err := validateDeviceName("Ricky's 笔记本"); err != nil {
		t.Fatal(err)
	}
}
