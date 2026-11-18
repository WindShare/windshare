package relaysetup

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/core/serviceauthority"
)

func TestNativeDiscoveryVerifiesEndpointAndUsesLocalKeyAdmission(t *testing.T) {
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
	const base = "https://node.example/v2/ws//"
	const endpoint = "wss://node.example/v2/ws//v2/ws"
	for _, anonymous := range []bool{true, false} {
		t.Run(map[bool]string{true: "anonymous", false: "local-key"}[anonymous], func(t *testing.T) {
			payload, err := base64.RawURLEncoding.DecodeString(fixture.Descriptor.Descriptor)
			if err != nil {
				t.Fatal(err)
			}
			var descriptor map[string]any
			if err := json.Unmarshal(payload, &descriptor); err != nil {
				t.Fatal(err)
			}
			descriptor["relay_endpoint"] = endpoint
			descriptor["anonymous"] = anonymous
			payload, err = json.Marshal(descriptor)
			if err != nil {
				t.Fatal(err)
			}
			signature := ed25519.Sign(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{43}, 32)),
				append([]byte(serviceauthority.DescriptorDomain), payload...))
			envelope := map[string]any{"version": 2, "descriptor": base64.RawURLEncoding.EncodeToString(payload),
				"signature": base64.RawURLEncoding.EncodeToString(signature), "relay_url": "https://unrelated.example",
				"account_login_page": "https://unsigned.example/device"}
			store := &memoryStore{saved: prior()}
			service := New(Dependencies{Store: store, Now: func() time.Time { return time.Unix(fixture.Now, 0) },
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
