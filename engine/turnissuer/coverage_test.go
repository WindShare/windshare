package turnissuer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"

	p "github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/turngrant"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
)

func staticService(id string) relayconfig.TURNService {
	return relayconfig.TURNService{ID: id, Kind: relayconfig.TURNStatic, URLs: []string{"turn:local"}, Username: "user", Password: "fixture"}
}

func issuerReply(code int, content string) *http.Response {
	recorder := httptest.NewRecorder()
	recorder.WriteHeader(code)
	_, _ = recorder.WriteString(content)
	return recorder.Result()
}

func TestPartialCoverageKeepsFreshAndRetainedGrants(t *testing.T) {
	var calls int
	client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls < 3 {
			return issuerReply(http.StatusTooManyRequests, ""), nil
		}
		return issuerReply(http.StatusOK, `{"iceServers":{"urls":["turn:local"],"username":"u","credential":"p"}}`), nil
	})}
	manual := []relayconfig.TURNService{staticService("home"), {ID: "cloud", Kind: relayconfig.TURNCloudflare, KeyID: "key", APIToken: "token"}}
	r := testResolver(t, manual, nil, nil, Options{HTTP: client})
	fresh := r.Issue(t.Context(), turngrant.IssueRequest{Mode: p.TURNAll})
	if fresh.Status != p.TURNPartialRetryable || !fresh.HasFallback || len(fresh.Credentials) != 1 || fresh.Credentials[0].ServiceID != "home" {
		t.Fatal("partial response lost a new grant or its retry")
	}
	request := turngrant.IssueRequest{Mode: p.TURNAll, Reusable: map[string]bool{"home": true}}
	partial := r.Issue(t.Context(), request)
	if partial.Status != p.TURNPartialRetryable || len(partial.Retained) != 1 || len(partial.Credentials) != 0 {
		t.Fatal("a retained grant hid a transient fallback failure")
	}
	recovered := r.Issue(t.Context(), request)
	if recovered.Status != p.TURNOK || len(recovered.Retained) != 1 || len(recovered.Credentials) != 1 || recovered.Credentials[0].ServiceID != "cloud" {
		t.Fatal("fallback unavailable after issuer recovery")
	}
}

func TestConfirmedFallbackCanReserveTheFourthService(t *testing.T) {
	manual := []relayconfig.TURNService{staticService("a"), staticService("b"), staticService("c"), staticService("d")}
	unknown := relayprofile.Candidate{PublicURL: "https://unknown.example", AccessKey: "fixture"}
	known := relayprofile.Candidate{PublicURL: "https://known.example", AccessKey: "fixture", ManagedTurn: true}
	client := &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			return issuerReply(http.StatusServiceUnavailable, ""), nil
		}
		if req.URL.Host != "known.example" {
			t.Error("unconfirmed fallback selected over confirmed service")
		}
		return issuerReply(http.StatusOK, `{"urls":["turn:local"],"username":"u","password":"p","ttl_seconds":1800}`), nil
	})}
	for _, tc := range []struct {
		name     string
		profiles []relayprofile.Candidate
		parallel int
	}{
		{"unknown only", []relayprofile.Candidate{unknown}, 4},
		{"confirmed after unknown", []relayprofile.Candidate{unknown, known}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var registrations []relayprofile.Registration
			for _, profile := range tc.profiles {
				registrations = append(registrations, profile.Registration())
			}
			r := testResolver(t, manual, registrations, tc.profiles, Options{HTTP: client})
			result := r.Issue(t.Context(), turngrant.IssueRequest{Mode: p.TURNParallel})
			if result.Status != p.TURNOK || len(result.Credentials) != tc.parallel || result.HasFallback != (tc.parallel < 4) {
				t.Fatal("unexpected parallel grant coverage")
			}
			reusable := map[string]bool{}
			for i, grant := range result.Credentials {
				if grant.ServiceID != manual[i].ID {
					t.Fatal("manual service order changed")
				}
				reusable[grant.ServiceID] = true
			}
			result = r.Issue(t.Context(), turngrant.IssueRequest{Mode: p.TURNAll, Reusable: reusable})
			want := p.TURNOK
			if tc.parallel == 4 {
				want = p.TURNPartialRetryable
			}
			if result.Status != want || len(result.Retained) != tc.parallel || len(result.Credentials) != 4-tc.parallel {
				t.Fatal("escalation failed to honor four confirmed services")
			}
		})
	}
}

func TestDiscoveryRetrySurvivesItsFirstSessionAndSkipsTargetedRenewal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var gets atomic.Int32
		retrying, release := make(chan struct{}), make(chan struct{})
		client := &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodPost {
				return issuerReply(http.StatusOK, `{"urls":["turn:local"],"username":"u","password":"p","ttl_seconds":1800}`), nil
			}
			switch gets.Add(1) {
			case 1:
				return issuerReply(http.StatusServiceUnavailable, ""), nil
			case 2:
				close(retrying)
			default:
				t.Error("duplicate discovery flight")
			}
			select {
			case <-release:
				return issuerReply(http.StatusOK, `{"managed_turn":true}`), nil
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		})}
		relay := relayprofile.Candidate{PublicURL: "https://private.example", AccessKey: "fixture"}
		r := testResolver(t, []relayconfig.TURNService{staticService("home")}, []relayprofile.Registration{relay.Registration()}, []relayprofile.Candidate{relay}, Options{HTTP: client})
		r.probes.Wait()
		target := r.Issue(t.Context(), turngrant.IssueRequest{Mode: p.TURNAll, ServiceIDs: []string{"home"}})
		if target.Status != p.TURNOK || len(target.Credentials) != 1 || gets.Load() != 1 {
			t.Fatal("targeted manual renewal restarted unrelated discovery")
		}
		demand := turngrant.IssueRequest{Mode: p.TURNAll, Reusable: map[string]bool{"home": true}}
		first, cancel := context.WithCancel(t.Context())
		defer cancel()
		abandoned := make(chan turngrant.IssueResult, 1)
		go func() { abandoned <- r.Issue(first, demand) }()
		<-retrying
		joined := make(chan turngrant.IssueResult, 1)
		go func() { joined <- r.Issue(t.Context(), demand) }()
		synctest.Wait()
		cancel()
		if got := <-abandoned; got.Status != p.TURNPartialRetryable {
			t.Fatal("canceled waiter failed to return its retained grant")
		}
		close(release)
		if got := <-joined; got.Status != p.TURNOK || len(got.Credentials) != 1 || len(got.Retained) != 1 || gets.Load() != 2 {
			t.Fatal("another session canceled shared capability discovery")
		}
	})
}
