//go:build windows

package osfs

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	windowsV3AccessAllowedObjectACEType         = 0x05
	windowsV3AccessDeniedObjectACEType          = 0x06
	windowsV3AccessAllowedCallbackACEType       = 0x09
	windowsV3AccessDeniedCallbackACEType        = 0x0a
	windowsV3AccessAllowedCallbackObjectACEType = 0x0b
	windowsV3AccessDeniedCallbackObjectACEType  = 0x0c
)

const (
	windowsV3AncestryGenericRights = windows.GENERIC_READ | windows.GENERIC_WRITE |
		windows.GENERIC_EXECUTE | windows.GENERIC_ALL
	windowsV3AncestryMutationRights = windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER |
		windowsV3DirectoryDeleteChild
	windowsV3KnownAccessACEFlags = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE |
		windows.NO_PROPAGATE_INHERIT_ACE | windows.INHERIT_ONLY_ACE | windows.INHERITED_ACE
)

type windowsV3AncestryAuthorityVerifier interface {
	Verify(windows.Handle) error
}

type windowsV3AncestryAuthorityVerifierFunc func(windows.Handle) error

func (verify windowsV3AncestryAuthorityVerifierFunc) Verify(handle windows.Handle) error {
	return verify(handle)
}

type windowsV3NativeAncestryAuthorityVerifier struct {
	policy *windowsV3PrivatePolicy
}

func (verifier windowsV3NativeAncestryAuthorityVerifier) Verify(handle windows.Handle) error {
	descriptor, err := windows.GetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return err
	}
	return windowsV3VerifyAncestryAuthorityDescriptor(descriptor, verifier.policy)
}

func windowsV3IsAdministratorAccount(sid *windows.SID) bool {
	if sid == nil || !sid.IsValid() {
		return false
	}
	// The account name and account-domain components can vary, so only the
	// native well-known-SID classifier can identify the privileged RID-500
	// account without broad suffix matching or name resolution.
	return sid.IsWellKnown(windows.WinAccountAdministratorSid)
}

func (policy *windowsV3PrivatePolicy) ancestryExempts(sid *windows.SID) bool {
	if policy == nil || sid == nil {
		return false
	}
	return policy.userSID != nil && sid.Equals(policy.userSID) ||
		policy.systemSID != nil && sid.Equals(policy.systemSID) ||
		policy.administratorsSID != nil && sid.Equals(policy.administratorsSID) ||
		policy.trustedInstallerSID != nil && sid.Equals(policy.trustedInstallerSID) ||
		windowsV3IsAdministratorAccount(sid)
}

func windowsV3VerifyAncestryAuthorityDescriptor(
	descriptor *windows.SECURITY_DESCRIPTOR,
	policy *windowsV3PrivatePolicy,
) error {
	if descriptor == nil || policy == nil || policy.userSID == nil || policy.systemSID == nil ||
		policy.administratorsSID == nil || policy.trustedInstallerSID == nil {
		return errors.New("Windows ancestry authority policy is unavailable")
	}
	owner, ownerDefaulted, err := descriptor.Owner()
	if err != nil || owner == nil || ownerDefaulted || !owner.IsValid() {
		return errors.Join(errors.New("ancestry directory owner is absent, invalid, or defaulted"), err)
	}
	if !policy.ancestryExempts(owner) {
		return fmt.Errorf("unprivileged ancestry owner %s can rewrite directory authority", owner.String())
	}
	dacl, daclDefaulted, err := descriptor.DACL()
	if err != nil || dacl == nil || daclDefaulted {
		return errors.Join(errors.New("ancestry directory DACL is absent, null, or defaulted"), err)
	}

	denied := make(map[string]windows.ACCESS_MASK)
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return err
		}
		if ace == nil || ace.Header.AceSize < uint16(unsafe.Offsetof(ace.SidStart))+8 {
			return errors.Join(errWindowsV3OutputUnsupported,
				errors.New("ancestry DACL contains a malformed access entry"))
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			// Inherit-only entries constrain descendants but confer no authority on
			// the directory whose placement is being certified.
			continue
		}
		if ace.Header.AceFlags&^uint8(windowsV3KnownAccessACEFlags) != 0 {
			return errors.Join(errWindowsV3OutputUnsupported,
				fmt.Errorf("ancestry DACL access entry has unsupported flags %#x", ace.Header.AceFlags))
		}

		allow := false
		switch ace.Header.AceType {
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			allow = true
		case windows.ACCESS_DENIED_ACE_TYPE:
		case windowsV3AccessAllowedObjectACEType,
			windowsV3AccessDeniedObjectACEType,
			windowsV3AccessAllowedCallbackACEType,
			windowsV3AccessDeniedCallbackACEType,
			windowsV3AccessAllowedCallbackObjectACEType,
			windowsV3AccessDeniedCallbackObjectACEType:
			return errors.Join(errWindowsV3OutputUnsupported,
				fmt.Errorf("ancestry DACL contains conditional or object-specific ACE type %d", ace.Header.AceType))
		default:
			return errors.Join(errWindowsV3OutputUnsupported,
				fmt.Errorf("ancestry DACL contains unsupported ACE type %d", ace.Header.AceType))
		}

		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() || windows.GetLengthSid(sid) > uint32(ace.Header.AceSize)-uint32(unsafe.Offsetof(ace.SidStart)) {
			return errors.Join(errWindowsV3OutputUnsupported,
				errors.New("ancestry DACL contains a malformed trustee SID"))
		}
		if policy.ancestryExempts(sid) {
			continue
		}
		if ace.Mask&windowsV3AncestryGenericRights != 0 {
			return errors.Join(errWindowsV3OutputUnsupported,
				fmt.Errorf("ancestry DACL trustee %s uses an unmapped generic access mask %#x", sid.String(), ace.Mask))
		}
		dangerous := ace.Mask & windowsV3AncestryMutationRights
		if dangerous == 0 {
			continue
		}
		key := sid.String()
		if !allow {
			denied[key] |= dangerous
			continue
		}
		if effective := dangerous &^ denied[key]; effective != 0 {
			return fmt.Errorf("unprivileged ancestry trustee %s has effective mutation rights %#x", key, effective)
		}
	}
	return nil
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
		return errors.New("public directory ancestry authority verifier is absent")
	}
	if err := directory.ancestryAuthority.Verify(directory.handle()); err != nil {
		// ACL admission is an authority denial even when the native reason remains
		// unsupported. Structural handle, volume, and incarnation failures above
		// intentionally retain the structural-unsafe ancestry decision.
		return errors.Join(errOutputAncestryAuthorityDenied, err)
	}
	return nil
}

func windowsV3AuthorityFailureClass(err error) error {
	if errors.Is(err, errWindowsV3OutputUnsupported) {
		return errWindowsV3OutputUnsupported
	}
	return errWindowsV3OutputUnsafe
}
