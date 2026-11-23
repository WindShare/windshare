package relayv2_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/windshare/windshare/core/serviceauthority"
	"github.com/windshare/windshare/internal/testrelaynative"
	"github.com/windshare/windshare/transport/relayv2"
)

func TestEnrollmentThroughNativeTLSIngress(t *testing.T) {
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
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, proxy.URL+"/.well-known/windshare", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, serviceauthority.MaximumDescriptorBytes+1))
	if err != nil || response.StatusCode != http.StatusOK || len(raw) > serviceauthority.MaximumDescriptorBytes {
		t.Fatalf("descriptor response: %s (%d bytes): %v", response.Status, len(raw), err)
	}
	descriptor, err := serviceauthority.InspectDescriptor(raw, time.Now())
	if err != nil || !descriptor.Services.Relay || descriptor.RelayEndpoint != "wss://"+address+"/v2/ws" {
		t.Fatalf("signed relay descriptor: %+v, %v", descriptor, err)
	}

	material, err := newRelayProtocolFixture()
	if err != nil {
		t.Fatal(err)
	}
	day := now / serviceauthority.DaySeconds
	entitlement := []any{1, day, min(now+180, (day+1)*serviceauthority.DaySeconds), 4096}
	ticket := signed(onlineKey, serviceauthority.TicketDomain, []any{1, []byte(root), 1, []byte("ordinary-account"),
		[]byte("login-credential"), now, now + 180, "official", entitlement})
	sender, err := relayv2.DialSender(ctx, relayv2.SenderConfig{
		RelayBaseURL: proxy.URL, Init: material.init, SenderPrivateKey: material.privateKey,
		Descriptor: material.descriptor, ResumeToken: material.resumeToken,
		Dial: relayv2.DialOptions{HTTPClient: client, Header: http.Header{"Authorization": {"Bearer " + ticket}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	trace.RequireCleanup(t, "sender", func(context.Context) error { return closeRelayConnection("sender", sender) })
	trace.RequireSuccess(t)
}
