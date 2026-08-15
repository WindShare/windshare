//go:build windows

package outputwindows

import (
	"errors"

	"golang.org/x/sys/windows"
)

type windowsV3AncestryAuthorityVerifier interface {
	Verify(windows.Handle) error
}

type windowsV3AncestryAuthorityVerifierFunc func(windows.Handle) error

func (verify windowsV3AncestryAuthorityVerifierFunc) Verify(handle windows.Handle) error {
	return verify(handle)
}

type windowsV3NativeAncestryAuthorityVerifier struct{}

func (windowsV3NativeAncestryAuthorityVerifier) Verify(handle windows.Handle) error {
	if handle == 0 || handle == windows.InvalidHandle {
		return errors.New("windows public directory handle is invalid")
	}
	// Public output deliberately retains the caller's ordinary inherited DACL.
	// Opening the purpose-specific handle is the OS access check; this verifies
	// only that the exact retained authority remains live.
	var information windows.ByHandleFileInformation
	return windows.GetFileInformationByHandle(handle, &information)
}

func (directory *windowsV3Directory) verifyPublicIdentityAuthority() error {
	if err := directory.usable(); err != nil {
		return err
	}
	if directory.private || !directory.placementGuard || !directory.selfPlacementGuard {
		return errors.New("public directory identity lacks a scoped no-delete-sharing placement guard")
	}
	facts, err := directory.inspector.Inspect(directory.handle())
	if err != nil {
		return err
	}
	if err := windowsV3ValidateOpenedObject(facts, directory.volume, true); err != nil {
		return err
	}
	if directory.ancestryAuthority == nil {
		return errors.New("public directory handle verifier is absent")
	}
	return directory.ancestryAuthority.Verify(directory.handle())
}

func windowsV3AuthorityFailureClass(err error) error {
	if errors.Is(err, errWindowsV3OutputUnsupported) {
		return errWindowsV3OutputUnsupported
	}
	return errWindowsV3OutputUnsafe
}
