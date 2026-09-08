package platformsetup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestStatusRecognizesShortInstallationPath(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "installed application.exe")
	if err := os.WriteFile(executable, []byte("installed"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := windows.UTF16PtrFromString(executable)
	if err != nil {
		t.Fatal(err)
	}
	size, err := windows.GetShortPathName(path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]uint16, size)
	if _, err := windows.GetShortPathName(path, &buffer[0], size); err != nil {
		t.Fatal(err)
	}
	shortExecutable := windows.UTF16ToString(buffer)
	if strings.EqualFold(shortExecutable, executable) {
		t.Skip("temporary volume does not provide an 8.3 alias")
	}
	for _, tc := range []struct{ name, saved, current string }{
		{"short-launch", executable, shortExecutable},
		{"short-status", shortExecutable, executable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertInstallationStatus(t, root, tc.saved, tc.current, "application-udp-tcp-rules-created")
		})
	}
}

func TestStatusKeepsDistinctInstallationLocations(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "wind.exe")
	other := filepath.Join(root, "other.exe")
	hardlink := filepath.Join(root, "linked.exe")
	for _, path := range []string{executable, other} {
		if err := os.WriteFile(path, []byte("same content"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(executable, hardlink); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, saved, current, reason string }{
		{"same-path", executable, executable, "application-udp-tcp-rules-created"},
		{"case", executable, strings.ToUpper(executable), "application-udp-tcp-rules-created"},
		{"different-file", executable, other, "install-path-changed"},
		{"hardlink", executable, hardlink, "install-path-changed"},
		{"missing-saved-file", filepath.Join(root, "missing.exe"), executable, "install-path-changed"},
		{"missing-current-file", executable, filepath.Join(root, "missing.exe"), "install-path-changed"},
		{"invalid-path", "invalid\x00path", executable, "install-path-changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertInstallationStatus(t, root, tc.saved, tc.current, tc.reason)
		})
	}
	// Replacing an executable during an upgrade preserves its path-scoped rules.
	if err := os.Remove(executable); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("upgrade"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertInstallationStatus(t, root, executable, executable, "application-udp-tcp-rules-created")
}

func assertInstallationStatus(t *testing.T, root, saved, current, reason string) {
	t.Helper()
	status := Status{Schema: 1, State: Configured, Reason: "application-udp-tcp-rules-created", Executable: saved}
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "status.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got := ReadForExecutable(path, current)
	if got.Reason != reason || (reason == status.Reason && got != status) ||
		(reason != status.Reason && got.State != Unavailable) {
		t.Fatalf("saved %q, current %q: %+v; want reason %q", saved, current, got, reason)
	}
}
