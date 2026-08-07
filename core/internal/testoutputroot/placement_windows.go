//go:build windows

package testoutputroot

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// windowsPlacementSecurityHooks isolate OS security calls from the placement
// policy. The fixture still uses native hooks, while tests can exercise each
// fail-closed boundary without depending on token state.
type windowsPlacementSecurityHooks struct {
	currentUser          func() (string, error)
	stringToSID          func(string) (*windows.SID, error)
	wellKnownSID         func(windows.WELL_KNOWN_SID_TYPE) (*windows.SID, error)
	descriptorFromString func(string) (*windows.SECURITY_DESCRIPTOR, error)
	owner                func(*windows.SECURITY_DESCRIPTOR) (*windows.SID, error)
	dacl                 func(*windows.SECURITY_DESCRIPTOR) (*windows.ACL, error)
	setNamedSecurityInfo func(string, *windows.SID, *windows.ACL) error
}

func newCertifiedPlacement(t testing.TB) string {
	t.Helper()
	placement := t.TempDir()
	if err := protectWindowsPlacement(placement); err != nil {
		t.Fatalf("protect Windows durable-output placement: %v", err)
	}
	return placement
}

func protectWindowsPlacement(path string) error {
	return protectWindowsPlacementWithHooks(path, windowsPlacementSecurityHooks{
		currentUser: func() (string, error) {
			user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
			if err != nil {
				return "", err
			}
			return user.User.Sid.String(), nil
		},
		stringToSID:          windows.StringToSid,
		wellKnownSID:         windows.CreateWellKnownSid,
		descriptorFromString: windows.SecurityDescriptorFromString,
		owner: func(descriptor *windows.SECURITY_DESCRIPTOR) (*windows.SID, error) {
			owner, _, err := descriptor.Owner()
			return owner, err
		},
		dacl: func(descriptor *windows.SECURITY_DESCRIPTOR) (*windows.ACL, error) {
			dacl, _, err := descriptor.DACL()
			return dacl, err
		},
		setNamedSecurityInfo: func(path string, owner *windows.SID, dacl *windows.ACL) error {
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
		},
	})
}

func protectWindowsPlacementWithHooks(path string, hooks windowsPlacementSecurityHooks) error {
	userSIDText, err := hooks.currentUser()
	if err != nil {
		return err
	}
	userSID, err := hooks.stringToSID(userSIDText)
	if err != nil {
		return err
	}
	systemSID, err := hooks.wellKnownSID(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	administratorsSID, err := hooks.wellKnownSID(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}

	var entries strings.Builder
	principals := make([]*windows.SID, 0, 3)
	for _, principal := range []*windows.SID{userSID, systemSID, administratorsSID} {
		if slices.ContainsFunc(principals, principal.Equals) {
			continue
		}
		principals = append(principals, principal)
		fmt.Fprintf(&entries, "(A;OICI;GA;;;%s)", principal.String())
	}
	descriptor, err := hooks.descriptorFromString("O:" + userSID.String() + "D:P" + entries.String())
	if err != nil {
		return err
	}
	owner, err := hooks.owner(descriptor)
	if err != nil {
		return err
	}
	dacl, err := hooks.dacl(descriptor)
	if err != nil {
		return err
	}
	// The existing placement is the production creator's trust anchor. Protect
	// inheritance here; the output child itself is still created and certified by
	// the production platform path exercised by the integration test.
	return hooks.setNamedSecurityInfo(path, owner, dacl)
}
