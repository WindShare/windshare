//go:build windows

package runtrace

import (
	"runtime"
	"syscall"
	"testing"
	"unsafe"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
	"golang.org/x/sys/windows"
)

const unrelatedTraceTestSID = "S-1-5-21-111111111-222222222-333333333-4242"

var getEffectiveRightsFromACL = windows.NewLazySystemDLL("advapi32.dll").NewProc("GetEffectiveRightsFromAclW")

func TestTraceTargetsCreateProtectedOwnerOnlyWindowsFiles(t *testing.T) {
	parent := t.TempDir()
	ownerSID, unrelatedSID := configureBroadTraceParent(t, parent)
	probePath := parent + `\inherited-probe.ndjson`
	probe, err := windows.UTF16PtrFromString(probePath)
	if err != nil {
		t.Fatal(err)
	}
	probeHandle, err := windows.CreateFile(
		probe, windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.CloseHandle(probeHandle); err != nil {
		t.Fatal(err)
	}
	probeDescriptor := traceSecurityDescriptor(t, probePath)
	probeACL, _, err := probeDescriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if rights := effectiveACLRights(t, probeACL, unrelatedSID); rights == 0 {
		t.Fatal("broad parent fixture did not grant the unrelated principal inherited access")
	}

	for _, test := range []struct {
		name   string
		target Target
	}{
		{name: "exact file", target: mustExactTarget(t, parent+`\exact.ndjson`)},
		{name: "run directory", target: mustRunDirectory(t, parent)},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder, err := Open(test.target, clievent.CommandShare, Config{})
			if err != nil {
				t.Fatal(err)
			}
			path := recorder.Path()
			if status := recorder.Close(); !status.Complete {
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
	if ownerSID.Equals(unrelatedSID) {
		t.Fatal("unrelated trace test principal unexpectedly equals the owner")
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

func assertProtectedOwnerOnlyWindowsFile(
	t *testing.T,
	path string,
	ownerSID *windows.SID,
	unrelatedSID *windows.SID,
) {
	t.Helper()
	descriptor := traceSecurityDescriptor(t, path)
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("trace DACL at %q is not protected from parent inheritance", path)
	}
	owner, defaulted, err := descriptor.Owner()
	if err != nil || defaulted || owner == nil || !owner.Equals(ownerSID) {
		t.Fatalf("trace owner at %q = %v defaulted=%t, want %s: %v", path, owner, defaulted, ownerSID, err)
	}
	dacl, defaulted, err := descriptor.DACL()
	if err != nil || defaulted || dacl == nil {
		t.Fatalf("trace DACL at %q = %v defaulted=%t: %v", path, dacl, defaulted, err)
	}
	if dacl.AceCount != 1 {
		t.Fatalf("trace DACL at %q has %d entries, want only the owner grant", path, dacl.AceCount)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		t.Fatal(err)
	}
	aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
		ace.Header.AceFlags&windows.INHERITED_ACE != 0 || !aceSID.Equals(ownerSID) {
		t.Fatalf(
			"trace DACL entry at %q has type=%d flags=%#x sid=%s, want a non-inherited owner grant",
			path, ace.Header.AceType, ace.Header.AceFlags, aceSID,
		)
	}
	if rights := effectiveACLRights(t, dacl, unrelatedSID); rights != 0 {
		t.Fatalf("unrelated principal retains trace access %#x at %q", rights, path)
	}
}

func traceSecurityDescriptor(t *testing.T, path string) *windows.SECURITY_DESCRIPTOR {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor == nil {
		t.Fatalf("trace at %q has no security descriptor", path)
	}
	return descriptor
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
