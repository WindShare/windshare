package httpapi

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/windshare/windshare/internal/testnetwork"
	"github.com/windshare/windshare/relay/admission"
	"github.com/windshare/windshare/relay/protocol"
	"github.com/windshare/windshare/relay/signaling"
)

func startServer(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	testnetwork.RequireOSNetwork(t)
	if cfg.Hub == nil {
		hub := signaling.NewHub(signaling.Config{})
		t.Cleanup(hub.Close)
		cfg.Hub = hub
	}
	ts := httptest.NewServer(NewHandler(cfg))
	t.Cleanup(ts.Close)
	return ts
}

func TestHealth(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	ts := startServer(t, Config{})
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var h protocol.Health
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil || h.Status != protocol.HealthOK {
		t.Fatalf("health = %+v, err=%v", h, err)
	}
}

func TestConfigAdvertisesVersionsAndLimits(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	ts := startServer(t, Config{})
	resp, err := http.Get(ts.URL + "/v1/config")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var c protocol.ServerConfig
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Fatal(err)
	}
	if len(c.ProtocolVersions) != 1 || c.ProtocolVersions[0] != protocol.ProtocolVersion {
		t.Fatalf("versions = %v", c.ProtocolVersions)
	}
	if c.Limits.MaxFrameSize <= 0 || c.Limits.MaxManifestSize <= 0 || c.Limits.MaxSignalingMessageBytes <= 0 ||
		c.Limits.Timeouts.WebSocketRoleMilliseconds <= 0 || c.Limits.Timeouts.SenderReconnectGraceMilliseconds <= 0 {
		t.Fatalf("limits were not advertised: %+v", c.Limits)
	}
}

func TestConfigDerivesHubPolicyAndRejectsMismatch(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	admissionConfig := admission.DefaultConfig()
	admissionConfig.MaxConnections = 7
	controller, err := admission.NewController(admissionConfig)
	if err != nil {
		t.Fatal(err)
	}
	hub := signaling.NewHub(signaling.Config{
		Admission:            controller,
		MaxFrameSize:         1024,
		MaxManifestSize:      2048,
		RoleTimeout:          2 * time.Second,
		KeepaliveTimeout:     3 * time.Second,
		SenderReconnectGrace: 4 * time.Second,
	})
	t.Cleanup(hub.Close)
	ts := startServer(t, Config{Hub: hub})
	resp, err := http.Get(ts.URL + "/v1/config")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got protocol.ServerConfig
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Limits.MaxFrameSize != 1024 || got.Limits.MaxManifestSize != 2048 ||
		got.Limits.Admission.MaxConnections != 7 || got.Limits.Timeouts.WebSocketRoleMilliseconds != 2000 ||
		got.Limits.Timeouts.SenderReconnectGraceMilliseconds != 4000 {
		t.Fatalf("derived limits = %+v", got.Limits)
	}
	if err := ValidateConfig(Config{
		Hub: hub,
		Limits: protocol.ServerLimits{
			MaxFrameSize: 4096,
		},
	}); err == nil {
		t.Fatal("mismatched advertised Hub policy was accepted")
	}
}

func TestUnknownProtocolVersionIs404(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	ts := startServer(t, Config{})
	for _, path := range []string{"/v2/config", "/v2/ws/AAAAAAAAAAAA", "/v1/ws/bad*id"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s → %d, want 404", path, resp.StatusCode)
		}
	}
}

func dialOrigin(t *testing.T, ts *httptest.Server, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	testnetwork.RequireOSNetwork(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/ws/AAAAAAAAAAAA"
	var hdr http.Header
	if origin != "" {
		hdr = http.Header{"Origin": []string{origin}}
	}
	ws, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: hdr})
	if ws != nil {
		t.Cleanup(func() { _ = ws.CloseNow() })
	}
	return ws, resp, err
}

func TestOriginWhitelist(t *testing.T) {
	ts := startServer(t, Config{
		AllowedOrigins: []string{"https://windshare.top", "HTTPS://Mirror.Example:8443"},
		AllowLocalhost: true,
	})
	allowed := []string{
		"", // 无 Origin = 非浏览器客户端(CLI),放行
		"https://windshare.top",
		"HTTPS://WINDSHARE.TOP",     // 大小写不敏感
		"https://windshare.top:443", // default ports identify the same origin
		"https://windshare.top:0443",
		"https://mirror.example:8443",
		"http://localhost:5173", // dev 放行 localhost 任意端口
		"http://127.0.0.1:4000",
	}
	for _, o := range allowed {
		if _, _, err := dialOrigin(t, ts, o); err != nil {
			t.Errorf("origin %q should be allowed: %v", o, err)
		}
	}
	denied := []string{
		"https://evil.example",
		"https://windshare.top.evil.example",
		"http://windsharetop", // 无法解析出可匹配的 host 也不能漏放
	}
	for _, o := range denied {
		_, resp, err := dialOrigin(t, ts, o)
		if err == nil {
			t.Errorf("origin %q should be rejected", o)
			continue
		}
		if resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Errorf("origin %q should return 403, resp=%v", o, resp)
		}
	}
}

func TestOriginConfigurationCanonicalizesDefaultPorts(t *testing.T) {
	hub := signaling.NewHub(signaling.Config{})
	t.Cleanup(hub.Close)
	ts := startServer(t, Config{
		Hub: hub,
		AllowedOrigins: []string{
			"https://windshare.top:443",
			"http://example.test:80",
			"https://[::1]:443",
		},
	})
	for _, origin := range []string{
		"https://windshare.top",
		"http://example.test",
		"https://[::1]",
		"https://[0:0:0:0:0:0:0:1]",
	} {
		if _, _, err := dialOrigin(t, ts, origin); err != nil {
			t.Errorf("canonical origin %q was denied: %v", origin, err)
		}
	}
}

func TestOriginDeniedWithoutLocalhostAllowance(t *testing.T) {
	ts := startServer(t, Config{AllowedOrigins: []string{"https://windshare.top"}})
	if _, _, err := dialOrigin(t, ts, "http://localhost:5173"); err == nil {
		t.Fatal("localhost should be rejected when AllowLocalhost is disabled")
	}
}

func TestInvalidOriginConfigurationFailsBeforeServing(t *testing.T) {
	hub := signaling.NewHub(signaling.Config{})
	t.Cleanup(hub.Close)
	for _, origin := range []string{
		"windshare.top",
		"ftp://windshare.top",
		"https://windshare.top/path",
		"https://user@windshare.top",
		"https://windshare.top?query=1",
		"https://windshare.top?",
		"https://windshare.top#",
		"https://windshare.top:",
		"https://windshare.top:65536",
		"https://[fe80::1%25eth0]",
		"https://127.1",
	} {
		t.Run(origin, func(t *testing.T) {
			cfg := Config{Hub: hub, AllowedOrigins: []string{origin}}
			if err := ValidateConfig(cfg); err == nil {
				t.Fatal("invalid origin was accepted")
			}
			defer func() {
				if recover() == nil {
					t.Fatal("NewHandler must fail immediately for invalid configuration")
				}
			}()
			_ = NewHandler(cfg)
		})
	}
}

func TestMultipleOriginHeadersAreRejected(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	ts := startServer(t, Config{AllowedOrigins: []string{"https://windshare.top"}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/ws/AAAAAAAAAAAA"
	ws, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"https://windshare.top", "https://evil.example"}},
	})
	if ws != nil {
		_ = ws.CloseNow()
	}
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("multiple origins were not rejected: err=%v resp=%v", err, resp)
	}
}

func TestRemoteIPCanonicalizesNetworkIdentity(t *testing.T) {
	for remote, want := range map[string]string{
		"127.0.0.1:1234":       "127.0.0.1",
		"[0:0:0:0:0:0:0:1]:80": "::1",
		"malformed":            "malformed",
	} {
		if got := remoteIP(&http.Request{RemoteAddr: remote}); got != want {
			t.Errorf("remoteIP(%q) = %q, want %q", remote, got, want)
		}
	}
}

type recordingAdmission struct{ sources chan string }

func (*recordingAdmission) Limits() admission.Limits { return admission.Limits{} }

func (a *recordingAdmission) AdmitConnection(source string) (*admission.Lease, admission.Decision) {
	a.sources <- source
	return nil, admission.Allowed
}

func (*recordingAdmission) BeginRegister(string) (*admission.Registration, admission.Decision) {
	return nil, admission.Allowed
}

func (*recordingAdmission) BeginJoin(string) (*admission.JoinAttempt, admission.Decision) {
	return nil, admission.Allowed
}

func TestSourceIdentityIsInjectable(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	recorder := &recordingAdmission{sources: make(chan string, 1)}
	hub := signaling.NewHub(signaling.Config{Admission: recorder})
	ts := startServer(t, Config{
		Hub: hub,
		SourceIdentity: func(r *http.Request) string {
			return r.Header.Get("X-Verified-Source")
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/ws/AAAAAAAAAAAA"
	ws, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"X-Verified-Source": []string{"trusted-proxy-source"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	select {
	case got := <-recorder.sources:
		if got != "trusted-proxy-source" {
			t.Fatalf("source = %q", got)
		}
	case <-ctx.Done():
		t.Fatal("admission did not receive injected source identity")
	}
}

func TestDefaultSourceIdentityIgnoresForwardingHeaders(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	recorder := &recordingAdmission{sources: make(chan string, 1)}
	hub := signaling.NewHub(signaling.Config{Admission: recorder})
	t.Cleanup(hub.Close)
	ts := startServer(t, Config{Hub: hub})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/ws/AAAAAAAAAAAA"
	ws, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Forwarded":       []string{"for=203.0.113.10"},
			"X-Forwarded-For": []string{"203.0.113.10"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	select {
	case got := <-recorder.sources:
		if got == "203.0.113.10" || net.ParseIP(got) == nil {
			t.Fatalf("default source trusted a forwarding header: %q", got)
		}
	case <-ctx.Done():
		t.Fatal("admission did not receive the TCP peer identity")
	}
}

func TestFailedUpgradeKeepsListenerAdmittedTCPLease(t *testing.T) {
	controller, err := admission.NewController(admission.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	hub := signaling.NewHub(signaling.Config{Admission: controller})
	t.Cleanup(hub.Close)
	lease, decision := hub.AdmitConnection("tcp-peer")
	if !decision.Allowed() {
		t.Fatal(decision)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/ws/AAAAAAAAAAAA", nil)
	req = req.WithContext(WithConnectionLease(req.Context(), lease))
	response := httptest.NewRecorder()
	NewHandler(Config{Hub: hub}).ServeHTTP(response, req)
	if response.Code == http.StatusSwitchingProtocols {
		t.Fatal("request without upgrade headers unexpectedly switched protocols")
	}
	if snapshot := controller.Snapshot(); snapshot.Connections != 1 {
		t.Fatalf("failed upgrade released listener-owned TCP lease: %+v", snapshot)
	}
	lease.Release()
	if snapshot := controller.Snapshot(); snapshot.Connections != 0 {
		t.Fatalf("listener-owned TCP lease did not release: %+v", snapshot)
	}
}
