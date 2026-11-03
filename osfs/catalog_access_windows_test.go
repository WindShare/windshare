package osfs

import (
	"errors"
	"io/fs"
	"os"
	"runtime"
	"testing"

	"golang.org/x/sys/windows"
)

func denyCatalogRead(t *testing.T, path string) {
	t.Helper()
	security, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if security == nil {
		t.Skip("filesystem does not support access control")
	}
	original, _, err := security.DACL()
	if err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	var pinned runtime.Pinner
	pinned.Pin(user.User.Sid)
	defer pinned.Unpin()
	// FILE_READ_DATA also denotes FILE_LIST_DIRECTORY. Metadata remains readable,
	// so the fixture exercises child-open failure instead of hiding its name.
	denied, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.FILE_READ_DATA, AccessMode: windows.DENY_ACCESS,
		Inheritance: windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
		},
	}}, original)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, original, nil); err != nil {
			t.Error(err)
		}
	})
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, denied, nil); err != nil {
		t.Fatal(err)
	}
	if probe, err := os.Open(path); err == nil {
		_ = probe.Close()
		t.Fatal("read-denial ACL did not deny read access")
	} else if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("read-denial fixture: %v", err)
	}
}
