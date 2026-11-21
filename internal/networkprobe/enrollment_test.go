package networkprobe

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pion/stun/v4"
	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/core/serviceauthority"
)

func TestEnrollmentChecksPinnedDescriptorAndSTUNOverTLSAndUDP(t *testing.T) {
	socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	if err := socket.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	go func() {
		var packet [2048]byte
		n, addr, err := socket.ReadFrom(packet[:])
		if err != nil {
			return
		}
		request := &stun.Message{Raw: packet[:n]}
		if err := request.Decode(); err != nil {
			t.Error(err)
			return
		}
		peer := addr.(*net.UDPAddr)
		reply := stun.MustBuild(request, stun.BindingSuccess, &stun.XORMappedAddress{IP: peer.IP, Port: peer.Port})
		if _, err := socket.WriteTo(reply.Raw, addr); err != nil {
			t.Error(err)
		}
	}()

	pub, signing, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity := sha256.Sum256(append([]byte("windshare/node-identity/v1\x00"), pub...))
	b64 := base64.RawURLEncoding
	address := "stun:" + socket.LocalAddr().String()
	mux := http.NewServeMux()
	host := httptest.NewTLSServer(mux)
	defer host.Close()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	now := uint64(time.Now().Unix())
	descriptor := serviceauthority.Descriptor{Version: 2, NodeID: b64.EncodeToString(identity[:]), NodeKey: b64.EncodeToString(pub),
		RelayEndpoint: strings.Replace(host.URL, "https://", "wss://", 1) + "/v2/ws", STUN: []string{address}, IssuedAt: now, ExpiresAt: now + 120}
	descriptor.Services.STUN = true
	body, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(signing, append([]byte(serviceauthority.DescriptorDomain), body...))
	mux.HandleFunc("GET /.well-known/windshare", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"version": 2, "descriptor": b64.EncodeToString(body), "signature": b64.EncodeToString(signature)})
	})

	work := Work{Node: nodedirectory.Node{NodeKey: descriptor.NodeKey, Relay: host.URL, Roles: []string{"stun"}, STUN: []string{address}}}
	var shares [OriginCount]SyntheticShare
	for i := range shares {
		shares[i], err = NewShare()
		if err != nil {
			t.Fatal(err)
		}
	}
	if shares[0].ID() == shares[1].ID() {
		t.Fatal("probe origins reused a share")
	}
	bind := func(ctx context.Context, addr string) error {
		conn, err := bindingDial((&net.Dialer{}).DialContext, bindingRTO)(ctx, "udp4", addr)
		if err != nil {
			return err
		}
		return conn.Close()
	}
	if err := check(t.Context(), host.Client(), work, shares, bind); err != nil {
		t.Fatal(err)
	}
	work.Node.STUN[0] = "stun:other.example:3478"
	if err := check(t.Context(), host.Client(), work, shares, bind); err == nil {
		t.Fatal("unadvertised STUN accepted")
	}
	work.Node.NodeKey = b64.EncodeToString(ed25519.NewKeyFromSeed(make([]byte, 32)).Public().(ed25519.PublicKey))
	if err := Check(t.Context(), host.Client(), work, shares); err == nil {
		t.Fatal("descriptor with a different node key accepted")
	}
}

// Mainland installations may resolve the fixed authority into their own network.
// Untrusted node checks must still reject that address before making a request.
func TestReleaseTransportUsesHostRoutingAndRefusesRedirects(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /directory", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("release")) })
	mux.HandleFunc("GET /moved", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/directory", http.StatusFound) })
	host := httptest.NewTLSServer(mux)
	defer host.Close()

	release, probe := ReleaseClient(), Client()
	for _, client := range []*http.Client{release, probe} {
		transport := client.Transport
		if transport == nil {
			transport = http.DefaultTransport
		}
		local := transport.(*http.Transport).Clone()
		local.TLSClientConfig = host.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
		client.Transport = local
		defer client.CloseIdleConnections()
	}
	if data, err := Fetch(t.Context(), release, host.URL+"/directory", 32); err != nil || string(data) != "release" {
		t.Fatalf("trusted mirror fetch: %q, %v", data, err)
	}
	if _, err := Fetch(t.Context(), release, host.URL+"/moved", 32); err == nil {
		t.Fatal("release fetch followed a redirect")
	}
	if _, err := Fetch(t.Context(), probe, host.URL+"/directory", 32); err == nil {
		t.Fatal("public node probe accepted a local address")
	}
}
