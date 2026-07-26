//go:build windows

package testoutputroot

import (
	"fmt"
	"testing"

	"golang.org/x/sys/windows"
)

func newCertifiedPlacement(t testing.TB) string {
	t.Helper()
	placement := t.TempDir()
	if err := protectWindowsPlacement(placement); err != nil {
		t.Fatalf("protect Windows durable-output placement: %v", err)
	}
	return placement
}

func protectWindowsPlacement(path string) error {
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil {
		return err
	}
	userSID, err := windows.StringToSid(user.User.Sid.String())
	if err != nil {
		return err
	}
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	administratorsSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}

	entries := ""
	principals := make([]*windows.SID, 0, 3)
	for _, principal := range []*windows.SID{userSID, systemSID, administratorsSID} {
		duplicate := false
		for _, existing := range principals {
			if principal.Equals(existing) {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		principals = append(principals, principal)
		entries += fmt.Sprintf("(A;OICI;GA;;;%s)", principal.String())
	}
	descriptor, err := windows.SecurityDescriptorFromString("O:" + userSID.String() + "D:P" + entries)
	if err != nil {
		return err
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	// The existing placement is the production creator's trust anchor. Protect
	// inheritance here; the output child itself is still created and certified by
	// the production platform path exercised by the integration test.
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|
			windows.PROTECTED_DACL_SECURITY_INFORMATION,
		owner,
		nil,
		dacl,
		nil,
	)
}
