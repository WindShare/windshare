//go:build windows

package diaglog

import (
	"runtime"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

const unrelatedTraceTestSID = "S-1-5-21-111111111-222222222-333333333-4242"

var getEffectiveRightsFromACL = windows.NewLazySystemDLL("advapi32.dll").NewProc("GetEffectiveRightsFromAclW")

func TestRunTargetsCreateProtectedOwnerOnlyWindowsFiles(t *testing.T) {
	parent := t.TempDir()
	ownerSID, unrelatedSID := configureBroadTraceParent(t, parent)
	exactTarget, err := ExactFile(parent + `\exact.ndjson`)
	if err != nil {
		t.Fatal(err)
	}
	dirTarget, err := RunDirectory(parent + `\generated-traces`)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		target Target
	}{
		{name: "exact file", target: exactTarget},
		{name: "run directory", target: dirTarget},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder, err := OpenRun(test.target, "share", RunOptions{})
			if err != nil {
				t.Fatal(err)
			}
			path := recorder.Path()
			if status := recorder.CloseRun(); !status.Complete() {
				t.Fatalf("trace close status = %+v", status)
			}
			assertProtectedOwnerOnlyWindowsFile(t, path, ownerSID, unrelatedSID)
		})
	}
}

func configureBroadTraceParent(t *testing.T, path string) (*windows.SID, *windows.SID) {
	t.Helper()
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	ownerSID, err := windows.StringToSid(user.User.Sid.String())
	if err != nil {
		t.Fatal(err)
	}
	unrelatedSID, err := windows.StringToSid(unrelatedTraceTestSID)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString(
		"O:" + ownerSID.String() + "D:P" +
			"(A;OICI;GA;;;" + ownerSID.String() + ")" +
			"(A;OICI;GRGW;;;" + unrelatedSID.String() + ")",
	)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|
			windows.PROTECTED_DACL_SECURITY_INFORMATION,
		owner,
		nil,
		dacl,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	return ownerSID, unrelatedSID
}

func assertProtectedOwnerOnlyWindowsFile(t *testing.T, path string, ownerSID, unrelatedSID *windows.SID) {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil || descriptor == nil {
		t.Fatalf("security descriptor at %q: %v", path, err)
	}
	control, _, err := descriptor.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("trace DACL at %q is not protected: %v", path, err)
	}
	owner, defaulted, err := descriptor.Owner()
	if err != nil || defaulted || owner == nil || !owner.Equals(ownerSID) {
		t.Fatalf("trace owner at %q = %v defaulted=%t, want %s: %v", path, owner, defaulted, ownerSID, err)
	}
	dacl, defaulted, err := descriptor.DACL()
	if err != nil || defaulted || dacl == nil || dacl.AceCount != 1 {
		t.Fatalf("trace DACL at %q = %v defaulted=%t: %v", path, dacl, defaulted, err)
	}
	if rights := effectiveACLRights(t, dacl, unrelatedSID); rights != 0 {
		t.Fatalf("unrelated principal retains trace access %#x at %q", rights, path)
	}
}

func effectiveACLRights(t *testing.T, acl *windows.ACL, sid *windows.SID) windows.ACCESS_MASK {
	t.Helper()
	var pin runtime.Pinner
	pin.Pin(sid)
	defer pin.Unpin()
	trustee := windows.TRUSTEE{
		TrusteeForm:  windows.TRUSTEE_IS_SID,
		TrusteeType:  windows.TRUSTEE_IS_USER,
		TrusteeValue: windows.TrusteeValueFromSID(sid),
	}
	var rights windows.ACCESS_MASK
	result, _, _ := getEffectiveRightsFromACL.Call(
		uintptr(unsafe.Pointer(acl)),
		uintptr(unsafe.Pointer(&trustee)),
		uintptr(unsafe.Pointer(&rights)),
	)
	runtime.KeepAlive(trustee)
	if result != uintptr(windows.ERROR_SUCCESS) {
		t.Fatalf("evaluate ACL rights for %s: %v", sid, syscall.Errno(result))
	}
	return rights
}
