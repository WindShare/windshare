package platformsetup

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestStatusRequiresCurrentInstallation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	if got := ReadForExecutable(path, "wind"); got.Reason != "first-setup-not-run" {
		t.Fatal(got)
	}
	for _, tc := range []struct{ data, reason, state string }{
		{`{"schema":1,"state":"configured","reason":"application-udp-tcp-rules-created","executable":"wind"}`, "application-udp-tcp-rules-created", Configured},
		{`{"schema":1,"state":"declined","reason":"user-skipped","executable":"wind"}`, "user-skipped", Declined},
		{`{"schema":1,"state":"unavailable","reason":"firewall-command-unavailable-or-denied","executable":"wind"}`, "firewall-command-unavailable-or-denied", Unavailable},
		{`{"schema":1,"state":"configured","reason":"application-udp-tcp-rules-created","executable":"old-wind"}`, "install-path-changed", Unavailable},
		{`{"schema":1,"state":"configured","reason":"user-skipped","executable":"wind"}`, "status-invalid", Unavailable},
		{`{"schema":2,"state":"configured","executable":"wind"}`, "status-invalid", Unavailable},
		{`{"schema":1,"state":"other","executable":"wind"}`, "status-invalid", Unavailable},
		{`broken`, "status-invalid", Unavailable},
	} {
		if err := os.WriteFile(path, []byte(tc.data), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := ReadForExecutable(path, "wind"); got.State != tc.state || got.Reason != tc.reason {
			t.Fatalf("%s: %+v", tc.data, got)
		}
	}
	if got := ReadForExecutable(filepath.Dir(path), "wind"); got.Reason != "status-unreadable" {
		t.Fatal(got)
	}
}

func TestReadDoesNotCreateSetupState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	if got := Read(); got.State != Unavailable {
		t.Fatal(got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("runtime read mutated setup state: %v %v", entries, err)
	}
}

func TestReadUnavailableConfiguration(t *testing.T) {
	t.Setenv("LOCALAPPDATA", "")
	status := Read()
	expected := "platform-firewall-setup-unsupported"
	if runtime.GOOS == "windows" {
		expected = "config-directory-unavailable"
	}
	if status.State != Unavailable || status.Reason != expected {
		t.Fatal(status)
	}
}
