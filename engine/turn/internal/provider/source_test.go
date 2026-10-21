package provider

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
)

type clientFunc func(*http.Request) (*http.Response, error)

func (f clientFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }
func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
}

func TestCloudflareIssuanceRenewalAndConfirmedRevocation(t *testing.T) {
	now := time.Unix(1700000000, 0)
	calls := 0
	source, err := NewCloudflare(CloudflareConfig{KeyID: "key", APIToken: "owner-secret", Now: func() time.Time { return now }, Lifetime: time.Hour,
		Client: clientFunc(func(request *http.Request) (*http.Response, error) {
			calls++
			if request.Header.Get("Authorization") != "Bearer owner-secret" || request.URL.Host != "rtc.live.cloudflare.com" {
				t.Fatal("issuer authority missing")
			}
			if _, ok := request.Context().Deadline(); !ok {
				t.Fatal("unbounded provider request")
			}
			if strings.HasSuffix(request.URL.Path, "/revoke") {
				return response(204, ""), nil
			}
			body, _ := io.ReadAll(request.Body)
			if string(body) != `{"ttl":3600}` {
				t.Fatalf("request %s", body)
			}
			return response(201, `{"iceServers":[{"urls":["stun:discovery.example:3478"]},{"urls":["turn:relay.example:3478?transport=udp","turns:relay.example:443?transport=tcp"],"username":"issued-user","credential":"issued-password"}]}`), nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	issued, err := source.Issue(context.Background(), Subject{})
	if err != nil {
		t.Fatal(err)
	}
	if len(issued.URLs) != 2 || issued.Credentials.Password() != "issued-password" || !issued.Credentials.ExpiresAt().Equal(now.Add(time.Hour)) {
		t.Fatal("incorrect delegated authority")
	}
	now = now.Add(time.Minute)
	fresh, err := source.Renew(context.Background(), Subject{}, issued)
	if err != nil || !fresh.Credentials.ExpiresAt().After(issued.Credentials.ExpiresAt()) {
		t.Fatal("renewal failed", err)
	}
	if err := source.Revoke(context.Background(), issued); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatal(calls)
	}
}
func TestCloudflareFailuresAndCancellation(t *testing.T) {
	for _, body := range []string{`{}`, `{"iceServers":[{"urls":["turn:[::1]:3478"],"username":"u","credential":"p"}]}`,
		`{"iceServers":[{"urls":["turn:host"],"username":"u","credential":"p"},{"urls":["turn:other"],"username":"v","credential":"p"}]}`, strings.Repeat("x", MaximumResponseBytes+1)} {
		source, _ := NewCloudflare(CloudflareConfig{KeyID: "key", APIToken: "secret", Client: clientFunc(func(*http.Request) (*http.Response, error) { return response(200, body), nil })})
		if _, err := source.Issue(context.Background(), Subject{}); err == nil {
			t.Fatal("accepted invalid provider reply")
		}
		if err := source.Revoke(context.Background(), Issued{}); err == nil {
			t.Fatal("revoked absent credential")
		}
	}
	source, _ := NewCloudflare(CloudflareConfig{KeyID: "key", APIToken: "secret", Client: clientFunc(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := source.Issue(ctx, Subject{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestCoturnAndStaticCapabilities(t *testing.T) {
	now := time.Unix(1700000000, 0)
	source, err := NewCoturn(CoturnConfig{SharedSecret: "secret", Now: func() time.Time { return now }, Lifetime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	subject := Subject{SessionID: [16]byte{1}, PathID: [16]byte{2}}
	issued, err := source.Issue(context.Background(), subject)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha1.New, []byte("secret"))
	_, _ = mac.Write([]byte(issued.Credentials.Username()))
	if issued.Credentials.Password() != base64.StdEncoding.EncodeToString(mac.Sum(nil)) || !strings.HasPrefix(issued.Credentials.Username(), "1700000060:") {
		t.Fatal("incompatible coturn credential")
	}
	now = now.Add(time.Minute)
	fresh, err := source.Renew(context.Background(), Subject{subject.SessionID, [16]byte{3}}, issued)
	if err != nil || fresh.Credentials.Username() == issued.Credentials.Username() {
		t.Fatal("path renewal reused authority")
	}
	static, _ := NewStatic(issued.Credentials, func() time.Time { return now })
	if _, err := static.Issue(context.Background(), subject); !errors.Is(err, ErrExpired) {
		t.Fatal(err)
	}
	fixed, _ := iceconfig.Fixed("u", "p")
	static, _ = NewStatic(fixed, func() time.Time { return now })
	if _, err := static.Issue(context.Background(), subject); err != nil {
		t.Fatal(err)
	}
	if _, ok := any(static).(interface {
		Renew(context.Context, Subject, Issued) (Issued, error)
	}); ok {
		t.Fatal("static falsely claims renewal")
	}
}
