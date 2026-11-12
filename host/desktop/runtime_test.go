package desktop

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
}
