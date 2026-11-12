package desktop

import (
	"net"
	"testing"
	"time"
)

func TestHostReadinessAndJoinedShutdownAreIndependentOfConfiguration(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("WINDSHARE_CONFIG", "missing-settings.json")
	t.Setenv("WINDSHARE_TRACE", "1")
	host, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	firstToken := host.Endpoint.Token
	reply := host.app.execute(createRequest())
	if reply.Snapshot.Shares[0].Error.Code != "configuration_required" {
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
