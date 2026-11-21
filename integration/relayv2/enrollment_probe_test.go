package relayv2_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/core/serviceauthority"
	"github.com/windshare/windshare/internal/networkprobe"
	"github.com/windshare/windshare/internal/testrelaynative"
)

func TestEnrollmentProbeThroughNativeTLSIngress(t *testing.T) {
	if testing.Short() {
		t.Skip("uses the native relay integration fixture")
	}
	prepareRelaySuite(t)
	directory := t.TempDir()
	proxy := httptest.NewUnstartedServer(nil)
	defer proxy.Close()
	address := proxy.Listener.Addr().String()
	fixture := testrelaynative.NewFixture(directory, "wss://"+address+"/v2/ws")
	// This disposable authority signs the wire contract consumed by the real Rust relay.
	root, rootKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	online, onlineKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := uint64(time.Now().Unix())
	signed := func(key ed25519.PrivateKey, domain string, value []any) string {
		raw, err := cbor.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		sig := ed25519.Sign(key, append([]byte(domain), raw...))
		return base64.RawURLEncoding.EncodeToString(append(raw, sig...))
	}
	fixture.TrustIssuer(base64.RawURLEncoding.EncodeToString(root),
		signed(rootKey, serviceauthority.CertificateDomain, []any{1, []byte(root), 1, []byte(online), now - 60, now + 3600}))
	trace, id := startRelayScenario(t, "integration/relayv2/"+t.Name())
	host, err := fixture.Start(t.Context(), relaySuite.owner, testrelaynative.Launch{
		Executable: relaySuite.binary, WorkingDirectory: relaySuite.root, Identity: id,
	})
	if err != nil {
		t.Fatal(err)
	}
	trace.RequireCleanup(t, "relay", host.Close)
	backend, err := url.Parse("http://" + host.Ready.Address)
	if err != nil {
		t.Fatal(err)
	}
	proxy.Config.Handler = httputil.NewSingleHostReverseProxy(backend)
	proxy.StartTLS()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	client := proxy.Client()
	raw, err := networkprobe.Fetch(ctx, client, proxy.URL+"/.well-known/windshare", serviceauthority.MaximumDescriptorBytes)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := serviceauthority.InspectDescriptor(raw, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	node, err := serviceauthority.PublicKey(descriptor.NodeKey)
	if err != nil {
		t.Fatal(err)
	}
	work := networkprobe.Work{Node: nodedirectory.Node{NodeKey: descriptor.NodeKey, Relay: proxy.URL, Roles: []string{"relay"}},
		Origins: []string{"https://ws.ourworld.top", "https://windshare.cc", "https://frontend.example"}}
	var shares [networkprobe.OriginCount]networkprobe.SyntheticShare
	for i := range shares {
		shares[i], err = networkprobe.NewShare()
		if err != nil {
			t.Fatal(err)
		}
		shareID := shares[i].ID()
		hash := sha256.New()
		hash.Write([]byte("windshare/probe-scope/v1\x00"))
		hash.Write(node[:])
		hash.Write(shareID[:])
		day := now / serviceauthority.DaySeconds
		entitlement := []any{1, day, min(now+180, (day+1)*serviceauthority.DaySeconds), 4096}
		ticket := signed(onlineKey, serviceauthority.TicketDomain, []any{1, []byte(root), 1, []byte("windshare-probe!"),
			hash.Sum(nil)[:16], now, now + 180, "official", entitlement})
		work.Tickets = append(work.Tickets, ticket)
	}
	if err := networkprobe.Check(ctx, client, work, shares); err != nil {
		t.Fatal(err)
	}
	trace.RequireSuccess(t)
}
