package networkruntime

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/publicdial"
	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/serviceauthority"
	"github.com/windshare/windshare/engine/internal/community"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
	"github.com/windshare/windshare/transport/relayv2"
)

func TestObserverRecordsCommunityDialOriginForbiddenWithoutPoisoningHealthOrOtherOrigins(t *testing.T) {
	r := fixtureRuntime(t)
	now := r.now()
	r.directory.Close()
	r.directory = nil

	source := &monitorSource{}
	watcher := networkstate.NewWatcher(networkstate.WatcherConfig{
		Source:   source,
		Debounce: time.Millisecond,
		Interval: time.Hour,
		Now:      func() time.Time { return now },
	})
	defer watcher.Close()
	r.watcher = watcher
	_, _, _ = r.watcher.Step(t.Context())

	var gossip, live json.RawMessage
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/discovery/v1/peers":
			_ = json.NewEncoder(w).Encode([]json.RawMessage{gossip})
		case "/.well-known/windshare":
			_, _ = w.Write(live)
		case "/v2/ws":
			ws, err := websocket.Accept(w, req, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				return
			}
			_ = ws.CloseNow()
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	seed := "https://warm-peer.example.com:" + port
	identity := "wss://warm-peer.example.com:" + port + "/v2/ws"
	gossip = makeCommunityPeerEnvelope(t, 92, identity, "warm-peer.example.com", serviceauthority.GossipDomain, now, 24*time.Hour, true)
	live = makeCommunityPeerEnvelope(t, 92, identity, "warm-peer.example.com", serviceauthority.DescriptorDomain, now, 5*time.Minute, true)

	local := netip.MustParseAddr("127.0.0.1")
	r.community = community.New("", publicdial.Policy{
		Permit:          func(a netip.Addr) bool { return a == local },
		Lookup:          func(context.Context, string, string) ([]netip.Addr, error) { return []netip.Addr{local}, nil },
		TLSClientConfig: srv.Client().Transport.(*http.Transport).TLSClientConfig,
	}, func() time.Time { return now }, diagnostics.Sink{})
	r.config.BootstrapNodes = []string{seed}

	const altFrontend = "https://alt.example"
	scopeDefault := r.scope(r.config.LinkHost)
	scopeAlt := r.scope(altFrontend)
	r.community.Refresh(t.Context(), scopeDefault, r.seeds(relayconfig.Config{}))
	if peers := r.community.Candidates(scopeDefault); len(peers) != 1 || !peers[0].Verified {
		t.Fatalf("expected warm peer before dial 403: %+v", peers)
	}
	if stun := r.community.STUN(scopeDefault.Generation); len(stun) != 1 {
		t.Fatalf("expected verified STUN before dial 403: %+v", stun)
	}

	selection, err := r.Plan(t.Context(), relayprofile.Plan(nil), "")
	if err != nil || len(selection.Plan.Network) != 1 {
		t.Fatalf("Plan = (%+v, %v)", selection.Plan, err)
	}
	peerURL := selection.Plan.Network[0].URL
	observe := r.Observer(selection, diagnostics.Sink{})

	observe(peerURL, relayset.Attempt{State: relayset.AttemptStarted})
	observe(peerURL, relayset.Attempt{
		State:    relayset.AttemptFailed,
		Terminal: true,
		Failure:  &relayv2.RegistrationNotSentError{Cause: &relayv2.WebSocketDialError{StatusCode: http.StatusForbidden}},
	})

	if facts := r.health.Snapshot(r.generation(), now); len(facts) != 0 {
		t.Fatalf("community dial 403 poisoned global nodeHealth: %+v", facts)
	}
	if peers := r.community.Candidates(scopeDefault); len(peers) != 0 {
		t.Fatalf("default origin candidate survived dial 403: %+v", peers)
	}
	if peers := r.community.Candidates(scopeAlt); len(peers) != 1 || peers[0].ProbeFailed {
		t.Fatalf("alternate origin candidate poisoned by default origin dial 403: %+v", peers)
	}
	if stun := r.community.STUN(scopeDefault.Generation); len(stun) != 1 {
		t.Fatalf("dial 403 hid origin-independent STUN: %+v", stun)
	}

	// A stale 403 callback started under generation 1 must not poison generation 2.
	observeStale := r.Observer(selection, diagnostics.Sink{})
	observeStale(peerURL, relayset.Attempt{State: relayset.AttemptStarted})
	source.state.ResumeSequence++
	now = now.Add(time.Millisecond)
	_, _, _ = r.watcher.Step(t.Context())
	now = now.Add(time.Millisecond)
	_, _, _ = r.watcher.Step(t.Context())
	if r.generation() != 2 {
		t.Fatalf("generation = %d, want 2", r.generation())
	}
	observeStale(peerURL, relayset.Attempt{
		State:   relayset.AttemptFailed,
		Failure: &relayv2.RegistrationNotSentError{Cause: &relayv2.WebSocketDialError{StatusCode: http.StatusForbidden}},
	})
	scopeGen2 := r.scope(r.config.LinkHost)
	if peers := r.community.Candidates(scopeGen2); len(peers) != 1 || peers[0].ProbeFailed {
		t.Fatalf("stale generation dial 403 poisoned generation 2: %+v", peers)
	}
}
