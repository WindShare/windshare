//go:build windows

package shareconfig

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"github.com/windshare/windshare/connectivity/senderrelay"
	"golang.org/x/sys/windows"
)

func TestWindowsReplacementKeepsOwnerOnlyAccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv(PathEnvironment, path)
	for range 2 {
		if err := Save(Config{Relays: []senderrelay.Candidate{{PublicURL: "https://relay.example", RegistrationToken: "private-token"}}}); err != nil {
			t.Fatal(err)
		}
		descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
			windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		control, _, err := descriptor.Control()
		if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
			t.Fatal("credential file inherits access:", err)
		}
		owner, _, err := descriptor.Owner()
		if err != nil {
			t.Fatal(err)
		}
		acl, _, err := descriptor.DACL()
		if err != nil || acl == nil || acl.AceCount != 1 {
			t.Fatal("credential file has unexpected access grants:", err)
		}
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, 0, &ace); err != nil {
			t.Fatal(err)
		}
		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !aceSID.Equals(owner) {
			t.Fatal("credential file grants access to a principal other than its owner")
		}
	}
}

func TestWindowsFailedReplacementPreservesPreviousConfiguration(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.json")
	t.Setenv(PathEnvironment, path)
	before := []byte("previous bytes")
	if err := os.WriteFile(path, before, privateFileMode); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	// Deny deletion while permitting reads so the final rename, rather than
	// validation or staging, fails with the old configuration still readable.
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	if err := Save(Config{Relays: []senderrelay.Candidate{{PublicURL: "https://replacement.example"}}}); err == nil {
		t.Fatal("replacement of a locked destination succeeded")
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(before) {
		t.Fatal("failed commit changed the prior configuration:", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatal("failed commit left staged credentials:", err)
	}
}
