package desktop

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
	"github.com/windshare/windshare/engine/relaysetup"
	"github.com/windshare/windshare/engine/turncache"
)

func TestHostReadinessAndJoinedShutdownAreIndependentOfConfiguration(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("WINDSHARE_CONFIG", filepath.Join(t.TempDir(), "missing-settings.json"))
	t.Setenv("WINDSHARE_TRACE", "1")
	host, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	firstHost := host
	t.Cleanup(func() { _ = firstHost.Close() })
	firstToken := host.Endpoint.Token
	if host.app.snapshot().SenderTrace != SenderTraceOn {
		t.Fatal("the startup environment did not arm sender trace")
	}
	host.app.execute(createRequest())
	view := awaitShare(t, host.app, "operation", func(s Share) bool { return s.State == AwaitingConfiguration })
	if view.Error == nil || view.Error.Code != "settings_unavailable" {
		t.Fatal("missing configuration is not actionable")
	}
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	if conn, err := net.DialTimeout("tcp", host.Endpoint.Address, time.Second); err == nil {
		conn.Close()
		t.Fatal("listener survived close")
	}
	host, err = Start()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := host.Close(); err != nil {
			t.Error(err)
		}
	}()
	if firstToken == host.Endpoint.Token {
		t.Fatal("host start reused authority")
	}
}

func TestHostRetainsSelectionWhenEngineCannotPlanNetwork(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LOCALAPPDATA", root)
	t.Setenv("XDG_CACHE_HOME", root)
	configPath := filepath.Join(root, "config.json")
	t.Setenv("WINDSHARE_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte(`{"profiles":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	trustPath := filepath.Join(configPath+".state", "network", "directory-windshare.json")
	if err := os.MkdirAll(filepath.Dir(trustPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trustPath, []byte("{invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WINDSHARE_TRACE", "1")
	host, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	host.app.execute(createRequest())
	view := awaitShare(t, host.app, "operation", func(s Share) bool { return s.State == AwaitingConfiguration })
	if view.Blocking == nil || view.Blocking.Reason != CreationBlockingReason(engine.NetworkTrustStateInvalid) || view.TaskID != "" {
		t.Fatalf("engine planning failure lost its recovery contract: %+v", view)
	}
	if reply := host.app.execute(Request{Command: "cancel", OperationID: "operation"}); reply.Error != nil || reply.Snapshot.Shares[0].Result.Outcome != OutcomeCancelled {
		t.Fatal("blocked creation cannot be cancelled", reply)
	}
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(host.Endpoint.LogDirectory, "host-*.jsonl"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("host log missing: %v %v", matches, err)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"event":"engine.trace"`) || !strings.Contains(string(data), `"name":"network_directory_initialization_failed"`) {
		t.Fatalf("engine directory initialization failure did not reach host trace log: %s", data)
	}
}

func TestUnavailableLogStorageDoesNotPreventSharingHostStartup(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(blocked, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOCALAPPDATA", blocked)
	t.Setenv("XDG_CACHE_HOME", blocked)
	host, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	if host.Endpoint.Address == "" {
		t.Fatal("logging failure prevented control readiness")
	}
	if host.Endpoint.LogDirectory != "" || host.app.snapshot().SenderTrace != SenderTraceUnavailable {
		t.Fatal("host advertised tracing without log storage")
	}
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHostStartRoutesRelayEnrollmentThroughEngineAndPopulatesTURNCache(t *testing.T) {
	const (
		accessKey = "device-credential"
		serviceID = "cf-service"
	)
	var (
		mu         sync.Mutex
		deviceName string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := "http://" + r.Host
		var response any
		switch r.URL.Path {
		case "/.well-known/windshare":
			response = map[string]any{"version": 1, "access": "restricted", "relay_url": origin, "console_url": origin}
		case "/api/enrollments":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode enrollment body: %v", err)
			}
			mu.Lock()
			deviceName = body["device_name"]
			mu.Unlock()
			response = map[string]any{
				"enrollment_id": "approval", "user_code": "ABCD-EFGH",
				"verification_uri_complete": origin + "/approve",
				"expires_at":                time.Now().Add(time.Minute).UnixMilli(), "interval_ms": 1000,
			}
		case "/api/enrollments/claim":
			response = map[string]string{"access_key": accessKey, "credential_id": "device"}
		case "/api/enrollments/installed":
			response = map[string]any{"managed_turn": []any{map[string]any{
				"service_id": serviceID, "relay_url": origin, "enabled": true,
			}}}
		default:
			t.Errorf("unexpected setup request: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	t.Setenv("LOCALAPPDATA", root)
	t.Setenv("XDG_CACHE_HOME", root)
	t.Setenv(relayconfig.PathEnvironment, configPath)
	if err := os.WriteFile(configPath, []byte(`{"profiles":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	host, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := host.Close(); err != nil {
			t.Error(err)
		}
	})
	reply := host.app.execute(Request{
		Command: "configure",
		Setup: &SetupInput{
			ID: "enroll-1", RelayURL: server.URL, RelayOperation: relaysetup.AddProfile,
		},
	})
	if reply.Error != nil {
		t.Fatalf("configure rejected: %+v", reply.Error)
	}
	host.app.work.Wait()
	view := host.app.snapshot().Setup
	if view.State != "finished" || !view.RelaySaved {
		t.Fatalf("relay enrollment did not finish cleanly: %+v", view)
	}
	hostname, _ := os.Hostname()
	wantName := relaysetup.DefaultDeviceName(hostname)
	mu.Lock()
	gotName := deviceName
	mu.Unlock()
	if gotName != wantName {
		t.Fatalf("device_name = %q, want %q", gotName, wantName)
	}
	capability, err := (turncache.FileStore{Settings: relayconfig.FileStore{Path: configPath}}).Load(
		relayprofile.Candidate{PublicURL: server.URL, AccessKey: accessKey},
	)
	if err != nil || capability == nil || capability.ServiceID != serviceID || !capability.Enabled {
		t.Fatalf("managed TURN capability was not persisted to turncache: cap=%+v err=%v", capability, err)
	}
}
