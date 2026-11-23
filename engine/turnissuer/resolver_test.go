package turnissuer

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session/turngrant"

	p "github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
func TestManualIssuersAndRoleBudget(t *testing.T) {
	var calls atomic.Int32
	httpClient := &http.Client{Transport: transportFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		if request.URL.Host != "rtc.live.cloudflare.com" || request.Header.Get("Authorization") != "Bearer api" {
			t.Error("unexpected issuer destination or authorization")
		}
		response := httptest.NewRecorder()
		response.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(response).Encode(map[string]any{"iceServers": []any{map[string]any{"urls": []string{"stun:public"}}, map[string]any{"urls": []string{"turn:one?transport=udp", "turns:two:443"}, "username": "temporary", "credential": "password"}}})
		return response.Result(), nil
	})}
	specs := []relayconfig.TURNService{
		{ID: "static", Kind: "shared_static", URLs: []string{"turn:one"}, Username: "u", Password: "p"},
		{ID: "rest", Kind: "coturn", URLs: []string{"turn:two"}, SharedSecret: "fixture", TTLSeconds: 60},
		{ID: "cloud", Kind: "cloudflare", KeyID: "key", APIToken: "api", URLs: []string{"turn:one"}},
	}
	r, err := New(specs, nil, nil, Options{HTTP: httpClient, Now: func() time.Time { return time.Unix(100, 0) }, Random: bytes.NewReader(make([]byte, 32))})
	if err != nil {
		t.Fatal(err)
	}
	specs[0].URLs[0] = "turn:changed"
	result := r.Issue(t.Context(), turngrant.IssueRequest{Mode: p.TURNParallel})
	if calls.Load() != 0 || result.Status != p.TURNOK || !result.HasFallback || len(result.Credentials) != 2 {
		t.Fatal("parallel role selection")
	}
	a, b := result.Credentials[0], result.Credentials[1]
	if a.URLs[0] != "turn:one" || a.ValidFor != 0 || b.Username != "160:"+strings.Repeat("0", 32) || b.ValidFor != time.Minute || b.Password != "ftAC/mcvKxRdSoEbctcvfTo8UtQ=" {
		t.Fatalf("manual grant mismatch, coturn proof %s", b.Password)
	}
	result = r.Issue(t.Context(), turngrant.IssueRequest{Mode: p.TURNAll, Reusable: map[string]bool{"static": true, "rest": true}})
	if calls.Load() != 1 || len(result.Retained) != 2 || len(result.Credentials) != 1 || result.Credentials[0].URLs[0] != "turn:one?transport=udp" || len(result.Credentials[0].URLs) != 1 {
		t.Fatal("escalation did not preserve existing grants and select the cloud filter")
	}
}
func TestManagedDiscoveryFailureAndRevocation(t *testing.T) {
	var state, issued, redirected atomic.Int32
	thirdParty := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer thirdParty.Close()
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if r.Header.Get("Authorization") != "" {
				t.Error("discovery received secret")
			}
			if state.Load() == 0 {
				w.WriteHeader(503)
				return
			}
			_, _ = w.Write([]byte(`{"managed_turn":true}`))
			return
		}
		if r.URL.Path != "/v2/turn/credentials" || r.Header.Get("Authorization") != "Bearer enrolled" {
			t.Error("credential issue origin or auth")
		}
		issued.Add(1)
		switch state.Load() {
		case 1:
			http.Redirect(w, r, thirdParty.URL, 307)
		case 2:
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"error":"turn_disabled"}`))
		default:
			_, _ = w.Write([]byte(`{"urls":["turn:one"],"username":"u","password":"p","ttl_seconds":1800}`))
		}
	}))
	defer host.Close()
	candidate := relayprofile.Candidate{PublicURL: host.URL, AccessKey: "enrolled"}
	r, err := New(nil, []relayprofile.Registration{candidate.Registration()}, []relayprofile.Candidate{candidate}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	issue := func(mode p.TURNMode) turngrant.IssueResult {
		return r.Issue(context.Background(), turngrant.IssueRequest{Mode: mode})
	}
	if issue(p.TURNParallel).Status != p.TURNRetryLater {
		t.Fatal("unknown capability is retryable")
	}
	<-r.Discover(t.Context(), turngrant.Trace{})
	state.Store(1)
	<-r.Discover(t.Context(), turngrant.Trace{})
	if g := issue(p.TURNParallel); g.Status != p.TURNOK || !g.HasFallback || len(g.Credentials) != 0 || issued.Load() != 0 {
		t.Fatal("fallback charged during parallel demand")
	}
	if issue(p.TURNAll).Status != p.TURNRetryLater || redirected.Load() != 0 {
		t.Fatal("redirect leaked outside the relay origin")
	}
	state.Store(3)
	if g := issue(p.TURNAll); len(g.Credentials) != 1 || g.Credentials[0].ValidFor != 30*time.Minute {
		t.Fatal("managed grant missing")
	}
	state.Store(2)
	_ = issue(p.TURNAll)
	count := issued.Load()
	if issue(p.TURNAll).Status != p.TURNUnconfigured || issued.Load() != count {
		t.Fatal("disabled issuer contacted again")
	}
}

func TestUnknownCapabilityDoesNotDelayKnownIssuer(t *testing.T) {
	started := make(chan struct{})
	client := &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
		reply := httptest.NewRecorder()
		if req.URL.Host == "rtc.live.cloudflare.com" {
			close(started)
			_, _ = reply.WriteString(`{"iceServers":{"urls":["turn:example.org"],"username":"u","credential":"p"}}`)
		} else {
			select {
			case <-started:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
			_, _ = reply.WriteString(`{"managed_turn":false}`)
		}
		return reply.Result(), nil
	})}
	source := relayprofile.Candidate{PublicURL: "https://private.example", AccessKey: "enrolled"}
	r, err := New([]relayconfig.TURNService{{ID: "known", Kind: relayconfig.TURNCloudflare, KeyID: "key", APIToken: "api"}}, []relayprofile.Registration{source.Registration()}, []relayprofile.Candidate{source}, Options{HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result := r.Issue(ctx, turngrant.IssueRequest{Mode: p.TURNAll})
	if ctx.Err() != nil || len(result.Credentials) != 1 || result.Credentials[0].ServiceID != "known" {
		t.Fatal("independent issuer blocked by discovery")
	}
}
