//go:build windows

package testoutputroot

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

func TestProtectWindowsPlacementWithHooksCoversSecurityBoundaries(t *testing.T) {
	sid, err := windows.StringToSid("S-1-5-18")
	if err != nil {
		t.Fatal(err)
	}
	descriptor := &windows.SECURITY_DESCRIPTOR{}
	cause := errors.New("injected placement security failure")
	base := func() windowsPlacementSecurityHooks {
		return windowsPlacementSecurityHooks{
			currentUser:          func() (string, error) { return "S-1-5-18", nil },
			stringToSID:          func(string) (*windows.SID, error) { return sid, nil },
			wellKnownSID:         func(windows.WELL_KNOWN_SID_TYPE) (*windows.SID, error) { return sid, nil },
			descriptorFromString: func(string) (*windows.SECURITY_DESCRIPTOR, error) { return descriptor, nil },
			owner:                func(*windows.SECURITY_DESCRIPTOR) (*windows.SID, error) { return sid, nil },
			dacl:                 func(*windows.SECURITY_DESCRIPTOR) (*windows.ACL, error) { return nil, nil },
			setNamedSecurityInfo: func(string, *windows.SID, *windows.ACL) error { return nil },
		}
	}
	for _, test := range []struct {
		name   string
		mutate func(*windowsPlacementSecurityHooks)
	}{
		{name: "current-user", mutate: func(hooks *windowsPlacementSecurityHooks) {
			hooks.currentUser = func() (string, error) { return "", cause }
		}},
		{name: "string-to-sid", mutate: func(hooks *windowsPlacementSecurityHooks) {
			hooks.stringToSID = func(string) (*windows.SID, error) { return nil, cause }
		}},
		{name: "system-sid", mutate: func(hooks *windowsPlacementSecurityHooks) {
			hooks.wellKnownSID = func(windows.WELL_KNOWN_SID_TYPE) (*windows.SID, error) { return nil, cause }
		}},
		{name: "administrator-sid", mutate: func(hooks *windowsPlacementSecurityHooks) {
			calls := 0
			hooks.wellKnownSID = func(windows.WELL_KNOWN_SID_TYPE) (*windows.SID, error) {
				calls++
				if calls == 2 {
					return nil, cause
				}
				return sid, nil
			}
		}},
		{name: "descriptor", mutate: func(hooks *windowsPlacementSecurityHooks) {
			hooks.descriptorFromString = func(string) (*windows.SECURITY_DESCRIPTOR, error) { return nil, cause }
		}},
		{name: "owner", mutate: func(hooks *windowsPlacementSecurityHooks) {
			hooks.owner = func(*windows.SECURITY_DESCRIPTOR) (*windows.SID, error) { return nil, cause }
		}},
		{name: "dacl", mutate: func(hooks *windowsPlacementSecurityHooks) {
			hooks.dacl = func(*windows.SECURITY_DESCRIPTOR) (*windows.ACL, error) { return nil, cause }
		}},
		{name: "set-security-info", mutate: func(hooks *windowsPlacementSecurityHooks) {
			hooks.setNamedSecurityInfo = func(string, *windows.SID, *windows.ACL) error { return cause }
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			hooks := base()
			test.mutate(&hooks)
			if err := protectWindowsPlacementWithHooks("C:\\placement", hooks); !errors.Is(err, cause) {
				t.Fatalf("hook failure = %v, want injected cause", err)
			}
		})
	}
	called := false
	hooks := base()
	hooks.setNamedSecurityInfo = func(path string, owner *windows.SID, dacl *windows.ACL) error {
		called = path == "C:\\placement" && owner == sid && dacl == nil
		return nil
	}
	if err := protectWindowsPlacementWithHooks("C:\\placement", hooks); err != nil || !called {
		t.Fatalf("successful hooked placement = %v, callback=%t", err, called)
	}
}
