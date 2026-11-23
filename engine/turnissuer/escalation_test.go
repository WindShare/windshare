package turnissuer

import (
	"fmt"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"

	p "github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/turngrant"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
)

func TestFullEscalationDiscoversFallbackAfterFourParallelGrants(t *testing.T) {
	manual := []relayconfig.TURNService{staticService("a"), staticService("b"), staticService("c"), staticService("d")}
	for _, tc := range []struct {
		name            string
		managed, paused bool
	}{
		{"absent", false, false},
		{"enabled", true, false},
		{"paused", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var probes, credentials atomic.Int32
			client := &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet {
					if probes.Add(1) == 1 {
						return issuerReply(http.StatusServiceUnavailable, ""), nil
					}
					return issuerReply(http.StatusOK, fmt.Sprintf(`{"managed_turn":%t}`, tc.managed)), nil
				}
				credentials.Add(1)
				if tc.paused {
					return issuerReply(http.StatusForbidden, `{"error":"turn_disabled"}`), nil
				}
				return issuerReply(http.StatusOK, `{"urls":["turn:local"],"username":"u","password":"p","ttl_seconds":1800}`), nil
			})}
			private := relayprofile.Candidate{PublicURL: "https://private.example", AccessKey: "fixture"}
			r := testResolver(t, manual, []relayprofile.Registration{private.Registration()}, []relayprofile.Candidate{private}, Options{HTTP: client})
			r.probes.Wait()
			parallel := r.Issue(t.Context(), turngrant.IssueRequest{Mode: p.TURNParallel})
			if parallel.Status != p.TURNOK || len(parallel.Credentials) != 4 {
				t.Fatal("unconfirmed role displaced a manual parallel service")
			}
			reusable := make(map[string]bool)
			for _, grant := range parallel.Credentials {
				reusable[grant.ServiceID] = true
			}
			// A receiver may have exhausted one of these paths. Only its full
			// escalation signals room for fallback; the sender still has all four IDs.
			_ = r.Issue(t.Context(), turngrant.IssueRequest{Mode: p.TURNAll, ServiceIDs: []string{"a"}})
			if probes.Load() != 1 || credentials.Load() != 0 {
				t.Fatal("parallel demand or targeted renewal spent fallback quota")
			}
			all := r.Issue(t.Context(), turngrant.IssueRequest{Mode: p.TURNAll, Reusable: reusable})
			if all.Status != p.TURNOK || all.HasFallback != (tc.managed && !tc.paused) || probes.Load() != 2 {
				t.Fatal("full escalation did not settle the pending capability")
			}
			if tc.managed && !tc.paused {
				if credentials.Load() != 1 || !slices.Equal(all.Retained, []string{"a", "b", "c"}) || len(all.Credentials) != 1 || all.Credentials[0].ServiceID == "d" {
					t.Fatal("newly confirmed fallback was omitted or displaced multiple services")
				}
			} else if (credentials.Load() != 0) != tc.paused || !slices.Equal(all.Retained, []string{"a", "b", "c", "d"}) || len(all.Credentials) != 0 {
				t.Fatal("ordinary private relay displaced a confirmed parallel service")
			}
		})
	}
}

func TestDisabledFallbackUncoversFourthConfiguredParallelService(t *testing.T) {
	var posts atomic.Int32
	client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		posts.Add(1)
		return issuerReply(http.StatusForbidden, `{"error":"turn_disabled"}`), nil
	})}
	private := relayprofile.Candidate{PublicURL: "https://private.example", AccessKey: "fixture", ManagedTurn: true}
	manual := []relayconfig.TURNService{staticService("a"), staticService("b"), staticService("c"), staticService("d")}
	r := testResolver(t, manual, []relayprofile.Registration{private.Registration()}, []relayprofile.Candidate{private}, Options{HTTP: client})
	parallel := r.Issue(t.Context(), turngrant.IssueRequest{Mode: p.TURNParallel})
	if len(parallel.Credentials) != 3 {
		t.Fatal("known fallback has no reserved slot")
	}
	demand := turngrant.IssueRequest{Mode: p.TURNAll, Reusable: map[string]bool{"a": true, "b": true, "c": true}}
	partial := r.Issue(t.Context(), demand)
	if partial.Status != p.TURNPartialRetryable || len(partial.Retained) != 3 || partial.HasFallback {
		t.Fatal("permanently disabled fallback hid the available fourth parallel service")
	}
	recovered := r.Issue(t.Context(), demand)
	if recovered.Status != p.TURNOK || len(recovered.Credentials) != 1 || recovered.Credentials[0].ServiceID != "d" || posts.Load() != 1 {
		t.Fatal("retry did not fill the fourth slot or contacted a disabled endpoint again")
	}
}

func TestEscalationCanFillOpenPlacesFromLaterPrivateEndpoints(t *testing.T) {
	manual := []relayconfig.TURNService{staticService("a"), staticService("b"), staticService("c"), staticService("d")}
	for _, services := range [][]relayconfig.TURNService{nil, manual} {
		t.Run(fmt.Sprintf("%d manual", len(services)), func(t *testing.T) {
			var profiles []relayprofile.Candidate
			var registrations []relayprofile.Registration
			byHost := make(map[string]int)
			for i := range relayprofile.MaximumEndpoints {
				host := fmt.Sprintf("private-%d.example", i)
				profile := relayprofile.Candidate{PublicURL: "https://" + host, AccessKey: "fixture"}
				byHost[host] = i
				profiles = append(profiles, profile)
				registrations = append(registrations, profile.Registration())
			}
			var gets, posts atomic.Int32
			var recovered atomic.Bool
			client := &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
				i := byHost[req.URL.Host]
				if req.Method == http.MethodGet {
					gets.Add(1)
					if recovered.Load() && i >= p.MaxGrantsPerMessage {
						return issuerReply(http.StatusOK, `{"managed_turn":true}`), nil
					}
					return issuerReply(http.StatusServiceUnavailable, ""), nil
				}
				posts.Add(1)
				return issuerReply(http.StatusOK, `{"urls":["turn:local"],"username":"u","password":"p","ttl_seconds":1800}`), nil
			})}
			r := testResolver(t, services, registrations, profiles, Options{HTTP: client})
			r.probes.Wait()
			initial := r.Issue(t.Context(), turngrant.IssueRequest{Mode: p.TURNParallel})
			reusable := make(map[string]bool)
			for _, c := range initial.Credentials {
				reusable[c.ServiceID] = true
			}
			if len(reusable) != len(services) || gets.Load() != int32(len(profiles)) || posts.Load() != 0 {
				t.Fatal("parallel demand lost a confirmed manual service or retried private endpoints")
			}
			recovered.Store(true)
			request := turngrant.IssueRequest{Mode: p.TURNAll, Reusable: reusable}
			got := r.Issue(t.Context(), request)
			r.probes.Wait()
			wantRetained := min(len(services), p.MaxGrantsPerMessage-1)
			wantNew := p.MaxGrantsPerMessage - wantRetained
			if got.Status != p.TURNOK || !got.HasFallback || len(got.Retained) != wantRetained ||
				len(got.Credentials) != wantNew || int(posts.Load()) != wantNew {
				t.Fatalf("full demand coverage: status=%v retained=%d grants=%d credential requests=%d", got.Status, len(got.Retained), len(got.Credentials), posts.Load())
			}
			for i, c := range got.Credentials {
				want := r.sources[len(services)+p.MaxGrantsPerMessage+i].ID
				if c.ServiceID != want {
					t.Fatalf("confirmed fallback %d = %s, want configured service %s", i, c.ServiceID, want)
				}
				reusable[c.ServiceID] = true
			}
			lastGets := gets.Load()
			got = r.Issue(t.Context(), request)
			if got.Status != p.TURNOK || len(got.Retained) != p.MaxGrantsPerMessage || gets.Load() != lastGets || int(posts.Load()) != wantNew {
				t.Fatal("four known targets caused redundant discovery or issuance")
			}
		})
	}
}
