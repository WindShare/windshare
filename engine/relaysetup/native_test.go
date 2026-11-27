package relaysetup

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/core/serviceauthority"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
)

func TestNativeDiscoveryVerifiesEndpointAndUsesLocalKeyAdmission(t *testing.T) {
	const base = "https://node.example/v2/ws//"
	const endpoint = "wss://node.example/v2/ws//v2/ws"
	for _, anonymous := range []bool{true, false} {
		t.Run(map[bool]string{true: "anonymous", false: "local-key"}[anonymous], func(t *testing.T) {
			now, descriptor := nativeFixture(t)
			descriptor["relay_endpoint"] = endpoint
			descriptor["public_relay"] = map[string]any{"state": "enabled", "anonymous": anonymous}
			envelope := signNativeFixture(t, descriptor)
			store := &memoryStore{saved: prior()}
			service := New(Dependencies{Store: store, Now: func() time.Time { return now },
				HTTP: fakeHTTP(func(request *http.Request) (*http.Response, error) {
					if request.URL.Path != "/.well-known/windshare" {
						t.Fatalf("native setup attempted enrollment: %s", request.URL)
					}
					return response(200, envelope)
				})})
			result, err := service.Run(t.Context(), Request{URL: base, Operation: AddProfile}, nil)
			if anonymous {
				if err != nil || result.RelayURL != base {
					t.Fatal(result, err)
				}
			} else if !errors.Is(err, ErrLocalCredentialRequired) || store.saved.Pending != nil {
				t.Fatal("native owner credential entered browser enrollment", err)
			}
			if _, err := service.Run(t.Context(), Request{URL: endpoint, Operation: AddProfile}, nil); err == nil || !strings.Contains(err.Error(), base) {
				t.Fatalf("full endpoint did not identify the signed relay base: %v", err)
			}
			if _, err := service.Run(t.Context(), Request{URL: "https://foreign.example", Operation: AddProfile}, nil); err == nil {
				t.Fatal("descriptor moved to an unrelated endpoint")
			}
			envelope["signature"] = base64.RawURLEncoding.EncodeToString(make([]byte, 64))
			if _, err := service.Run(t.Context(), Request{URL: base, Operation: AddProfile}, nil); err == nil {
				t.Fatal("unsigned node identity was admitted")
			}
		})
	}
}

func nativeFixture(t *testing.T) (time.Time, map[string]any) {
	t.Helper()
	data, err := os.ReadFile("../../testdata/network/authority.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Now        int64
		Descriptor struct {
			Version               int `json:"version"`
			Descriptor, Signature string
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(fixture.Descriptor.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	var descriptor map[string]any
	if err := json.Unmarshal(payload, &descriptor); err != nil {
		t.Fatal(err)
	}
	return time.Unix(fixture.Now, 0), descriptor
}

func signNativeFixture(t *testing.T, descriptor map[string]any) map[string]any {
	t.Helper()
	payload, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{43}, 32)),
		append([]byte(serviceauthority.DescriptorDomain), payload...))
	return map[string]any{"version": 2, "descriptor": base64.RawURLEncoding.EncodeToString(payload),
		"signature": base64.RawURLEncoding.EncodeToString(signature), "relay_url": "https://unrelated.example",
		"account_login_page": "https://unsigned.example/device"}
}

func TestNativeOwnerEnrollmentAndGuestCancellation(t *testing.T) {
	const address = "https://node.example"
	now, d := nativeFixture(t)
	d["console_url"] = address
	d["public_relay"] = serviceauthority.RelayAnonymous
	d["authority"] = nil
	envelope := signNativeFixture(t, d)
	store := FileStore{Settings: relayconfig.FileStore{Path: filepath.Join(t.TempDir(), "config.json")}}
	cancelled := false
	claims := 0
	role := relayprofile.Owner
	service := New(Dependencies{Store: store, Now: func() time.Time { return now },
		HTTP: fakeHTTP(func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case "/.well-known/windshare":
				return response(200, envelope)
			case "/api/enrollments":
				return response(201, enrollmentResponse{ID: "enrollment", UserCode: "PAIR", VerificationURL: address + "/?code=PAIR", IntervalMS: 1000, ExpiresAt: now.Add(time.Minute).UnixMilli()})
			case "/api/enrollments/claim":
				claims++
				return response(200, credentialResponse{Role: role, AccessKey: "device-key", ID: "credential"})
			case "/api/enrollments/installed":
				return response(200, struct{}{})
			case "/api/enrollments/cancel":
				var claimant map[string]string
				if json.NewDecoder(r.Body).Decode(&claimant) != nil || claimant["enrollment_id"] != "enrollment" || claimant["claim_secret"] == "" {
					t.Fatal("missing remote cancellation authority")
				}
				cancelled = true
				return response(200, struct{}{})
			default:
				t.Fatalf("unexpected endpoint %s", r.URL.Path)
				return nil, nil
			}
		})})
	request := Request{URL: address, Operation: AddProfile, DeviceName: "laptop"}
	if _, err := service.Run(t.Context(), request, nil); err != nil {
		t.Fatal(err)
	}
	owned, err := store.Load()
	if err != nil || len(owned.OwnedNative()) != 1 || owned.Profiles[0].AccessKey != "device-key" {
		t.Fatal(owned.OwnedNative(), err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err = service.Run(ctx, request, func(p Progress) {
		if p.Stage == "approval_required" && p.GuestAvailable {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || claims != 1 {
		t.Fatal("native address did not wait for owner approval", claims, err)
	}
	guest := request
	guest.Anonymous = true
	if _, err := service.Run(t.Context(), guest, nil); err != nil {
		t.Fatal(err)
	}
	saved, err := store.Load()
	if err != nil || !cancelled || saved.Pending != nil || len(saved.OwnedNative()) != 0 || len(saved.Profiles) != 1 || saved.Profiles[0].AccessKey != "" || saved.Profiles[0].Role != "" {
		t.Fatal("guest switch retained the pending enrollment or owner credentials", err)
	}
	role = ""
	if _, err := service.Run(t.Context(), request, nil); err == nil {
		t.Fatal("native enrollment installed a credential without an explicit role")
	}
}
