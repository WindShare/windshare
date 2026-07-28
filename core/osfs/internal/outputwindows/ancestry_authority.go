//go:build windows

package outputwindows

import (
	"errors"
	"fmt"
	"unsafe"

	"github.com/windshare/windshare/core/osfs/internal/outputfault"
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
	dacl, err := windowsV3CertifiedAncestryDACL(descriptor, policy)
	if err != nil {
		return err
	}
	denied := make(map[string]windows.ACCESS_MASK)
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		entry, relevant, err := windowsV3ReadAncestryMutationACE(dacl, index, policy)
		if err != nil {
			return err
		}
		if !relevant {
			continue
		}
		if !entry.allowed {
			denied[entry.trustee] |= entry.rights
			continue
		}
		if effective := entry.rights &^ denied[entry.trustee]; effective != 0 {
			return fmt.Errorf("unprivileged ancestry trustee %s has effective mutation rights %#x",
				entry.trustee, effective)
		}
	}
	return nil
}

func windowsV3CertifiedAncestryDACL(
	descriptor *windows.SECURITY_DESCRIPTOR,
	policy *windowsV3PrivatePolicy,
) (*windows.ACL, error) {
	if descriptor == nil || policy == nil || policy.userSID == nil || policy.systemSID == nil ||
		policy.administratorsSID == nil || policy.trustedInstallerSID == nil {
		return nil, errors.New("windows ancestry authority policy is unavailable")
	}
	owner, ownerDefaulted, err := descriptor.Owner()
	if err != nil || owner == nil || ownerDefaulted || !owner.IsValid() {
		return nil, errors.Join(errors.New("ancestry directory owner is absent, invalid, or defaulted"), err)
	}
	if !policy.ancestryExempts(owner) {
		return nil, fmt.Errorf("unprivileged ancestry owner %s can rewrite directory authority", owner.String())
	}
	dacl, daclDefaulted, err := descriptor.DACL()
	if err != nil || dacl == nil || daclDefaulted {
		return nil, errors.Join(errors.New("ancestry directory DACL is absent, null, or defaulted"), err)
	}
	return dacl, nil
}

type windowsV3AncestryMutationACE struct {
	trustee string
	rights  windows.ACCESS_MASK
	allowed bool
}

func windowsV3ReadAncestryMutationACE(
	dacl *windows.ACL,
	index uint32,
	policy *windowsV3PrivatePolicy,
) (windowsV3AncestryMutationACE, bool, error) {
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, index, &ace); err != nil {
		return windowsV3AncestryMutationACE{}, false, err
	}
	if ace == nil || ace.Header.AceSize < uint16(unsafe.Offsetof(ace.SidStart))+8 {
		return windowsV3AncestryMutationACE{}, false, errors.Join(errWindowsV3OutputUnsupported,
			errors.New("ancestry DACL contains a malformed access entry"))
	}
	// Inherit-only entries constrain descendants but confer no authority on the
	// directory whose placement is being certified.
	if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
		return windowsV3AncestryMutationACE{}, false, nil
	}
	if ace.Header.AceFlags&^uint8(windowsV3KnownAccessACEFlags) != 0 {
		return windowsV3AncestryMutationACE{}, false, errors.Join(errWindowsV3OutputUnsupported,
			fmt.Errorf("ancestry DACL access entry has unsupported flags %#x", ace.Header.AceFlags))
	}
	allowed, err := windowsV3AncestryACEAllowsAccess(ace.Header.AceType)
	if err != nil {
		return windowsV3AncestryMutationACE{}, false, err
	}
	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if !sid.IsValid() || windows.GetLengthSid(sid) > uint32(ace.Header.AceSize)-uint32(unsafe.Offsetof(ace.SidStart)) {
		return windowsV3AncestryMutationACE{}, false, errors.Join(errWindowsV3OutputUnsupported,
			errors.New("ancestry DACL contains a malformed trustee SID"))
	}
	if policy.ancestryExempts(sid) {
		return windowsV3AncestryMutationACE{}, false, nil
	}
	if ace.Mask&windowsV3AncestryGenericRights != 0 {
		return windowsV3AncestryMutationACE{}, false, errors.Join(errWindowsV3OutputUnsupported,
			fmt.Errorf("ancestry DACL trustee %s uses an unmapped generic access mask %#x", sid.String(), ace.Mask))
	}
	rights := ace.Mask & windowsV3AncestryMutationRights
	if rights == 0 {
		return windowsV3AncestryMutationACE{}, false, nil
	}
	return windowsV3AncestryMutationACE{trustee: sid.String(), rights: rights, allowed: allowed}, true, nil
}

func windowsV3AncestryACEAllowsAccess(aceType uint8) (bool, error) {
	switch aceType {
	case windows.ACCESS_ALLOWED_ACE_TYPE:
		return true, nil
	case windows.ACCESS_DENIED_ACE_TYPE:
		return false, nil
	case windowsV3AccessAllowedObjectACEType,
		windowsV3AccessDeniedObjectACEType,
		windowsV3AccessAllowedCallbackACEType,
		windowsV3AccessDeniedCallbackACEType,
		windowsV3AccessAllowedCallbackObjectACEType,
		windowsV3AccessDeniedCallbackObjectACEType:
		return false, errors.Join(errWindowsV3OutputUnsupported,
			fmt.Errorf("ancestry DACL contains conditional or object-specific ACE type %d", aceType))
	default:
		return false, errors.Join(errWindowsV3OutputUnsupported,
			fmt.Errorf("ancestry DACL contains unsupported ACE type %d", aceType))
	}
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
		return errors.Join(outputfault.ErrAncestryAuthorityDenied, err)
	}
	return nil
}

func windowsV3AuthorityFailureClass(err error) error {
	if errors.Is(err, errWindowsV3OutputUnsupported) {
		return errWindowsV3OutputUnsupported
	}
	return errWindowsV3OutputUnsafe
}
