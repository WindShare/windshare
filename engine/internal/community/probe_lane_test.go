package community

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/serviceauthority"
)

type blockingCommunityPeer struct {
	server   *httptest.Server
	identity string
	gossip   json.RawMessage
	live     json.RawMessage
	probes   atomic.Int32
}

func newBlockingCommunityPeer(t *testing.T, host string, seedByte byte, now time.Time, entered chan<- string, release <-chan struct{}, succeed bool) *blockingCommunityPeer {
	t.Helper()
	bp := &blockingCommunityPeer{}
	var once sync.Once
	bp.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case exchangePath:
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/.well-known/windshare":
			bp.probes.Add(1)
			once.Do(func() { entered <- host })
			select {
			case <-r.Context().Done():
				return
			case <-release:
			}
			if !succeed {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write(bp.live)
		case "/v2/ws":
			ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"share.example"}})
			if err != nil {
				return
			}
			_ = ws.CloseNow()
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(bp.server.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(bp.server.URL, "https://"))
	bp.identity = "wss://" + host + ":" + port + "/v2/ws"
	bp.gossip = envelopeWithSeed(t, seedByte, bp.identity, serviceauthority.GossipDomain, now, 24*time.Hour, serviceauthority.RelayAnonymous)
	bp.live = envelopeWithSeed(t, seedByte, bp.identity, serviceauthority.DescriptorDomain, now, 5*time.Minute, serviceauthority.RelayAnonymous)
	return bp
}

func TestForegroundVerifyIsolatedFromSaturatedBackgroundWarmAndCoalesces(t *testing.T) {
	now := time.Now()
	const front = "https://share.example"
	scope := Scope{Origin: front, Generation: 1}

	// 1. Saturated background warmProbes must not block concurrent foreground Verify() calls.
	warmEntered := make(chan string, fanout)
	releaseWarm := make(chan struct{})
	warmA := newBlockingCommunityPeer(t, "a-warm.example", 81, now, warmEntered, releaseWarm, false)
	warmB := newBlockingCommunityPeer(t, "b-warm.example", 82, now, warmEntered, releaseWarm, false)
	fgC := newFakeCommunityNode(t, "c-fg.example", 83, front, now, true)
	fgD := newFakeCommunityNode(t, "d-fg.example", 84, front, now, true)

	pool := New("", insecureLocalPolicy(fgC.server), func() time.Time { return now }, diagnostics.Sink{})
	defer pool.Close()
	for _, raw := range []json.RawMessage{warmA.gossip, warmB.gossip, fgC.gossip, fgD.gossip} {
		if !pool.accept(raw, "cache") {
			t.Fatal("failed to seed community cache")
		}
	}

	refreshDone := make(chan struct{})
	go func() {
		pool.Refresh(t.Context(), scope, Seeds{})
		close(refreshDone)
	}()
	for range fanout {
		select {
		case <-warmEntered:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for background warm() to saturate warmProbes")
		}
	}

	var fgWG sync.WaitGroup
	fgErrs := make(chan error, 2)
	for _, id := range []string{fgC.identity, fgD.identity} {
		fgWG.Go(func() {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			fgErrs <- pool.Verify(ctx, scope, id)
		})
	}
	fgWG.Wait()
	close(fgErrs)
	for err := range fgErrs {
		if err != nil {
			t.Fatalf("foreground Verify blocked or failed while warmProbes saturated: %v", err)
		}
	}
	close(releaseWarm)
	<-refreshDone
	if fgC.probes.Load() != 1 || fgD.probes.Load() != 1 {
		t.Fatalf("unexpected foreground probe counts: c=%d d=%d", fgC.probes.Load(), fgD.probes.Load())
	}

	// 2. Foreground Verify() targeting a peer already being warmed coalesces
	// onto the in-flight background probe instead of dialing twice.
	coalEntered := make(chan string, fanout)
	releaseCoal := make(chan struct{})
	coalA := newBlockingCommunityPeer(t, "a-coal.example", 85, now, coalEntered, releaseCoal, true)
	coalB := newBlockingCommunityPeer(t, "b-coal.example", 86, now, coalEntered, releaseCoal, true)

	coalPool := New("", insecureLocalPolicy(coalA.server), func() time.Time { return now }, diagnostics.Sink{})
	defer coalPool.Close()
	for _, raw := range []json.RawMessage{coalA.gossip, coalB.gossip} {
		if !coalPool.accept(raw, "cache") {
			t.Fatal("failed to seed coalescing cache")
		}
	}

	coalRefreshDone := make(chan struct{})
	go func() {
		coalPool.Refresh(t.Context(), scope, Seeds{})
		close(coalRefreshDone)
	}()
	for range fanout {
		select {
		case <-coalEntered:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for coalescing warm probes")
		}
	}

	coalErr := make(chan error, 1)
	go func() {
		coalErr <- coalPool.Verify(t.Context(), scope, coalA.identity)
	}()
	close(releaseCoal)
	if err := <-coalErr; err != nil {
		t.Fatalf("coalesced foreground Verify failed: %v", err)
	}
	<-coalRefreshDone
	if got := coalA.probes.Load(); got != 1 {
		t.Fatalf("foreground Verify duplicated in-flight warm probe: got %d probes, want 1", got)
	}
}
