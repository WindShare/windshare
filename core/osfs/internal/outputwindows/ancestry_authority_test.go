//go:build windows

package outputwindows

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"golang.org/x/sys/windows"
)

func windowsV3NativeTestTempDir(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", ".windshare-osfs-native-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove native test root: %v", err)
		}
	})
	return root
}

func windowsV3TestSecurityDescriptor(
	t *testing.T,
	owner *windows.SID,
	aces string,
) *windows.SECURITY_DESCRIPTOR {
	t.Helper()
	descriptor, err := windows.SecurityDescriptorFromString("O:" + owner.String() + "D:P" + aces)
	if err != nil {
		t.Fatalf("parse test security descriptor: %v", err)
	}
	return descriptor
}

func TestWindowsV3AncestryAuthorityRejectsCrossPrincipalMutation(t *testing.T) {
	policy, err := newWindowsV3PrivatePolicy()
	if err != nil {
		t.Fatal(err)
	}
	users, err := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
	if err != nil {
		t.Fatal(err)
	}
	dangerous := uint32(windowsV3AncestryMutationRights)
	allDangerous := fmt.Sprintf("0x%08x", dangerous)
	allow := func(sid *windows.SID, mask string, flags string) string {
		return fmt.Sprintf("(A;%s;%s;;;%s)", flags, mask, sid.String())
	}
	deny := func(sid *windows.SID, mask string) string {
		return fmt.Sprintf("(D;;%s;;;%s)", mask, sid.String())
	}

	exemptEntries := allow(policy.userSID, allDangerous, "") +
		allow(policy.systemSID, allDangerous, "") +
		allow(policy.administratorsSID, allDangerous, "") +
		allow(policy.trustedInstallerSID, allDangerous, "")
	if err := windowsV3VerifyAncestryAuthorityDescriptor(
		windowsV3TestSecurityDescriptor(t, policy.userSID, exemptEntries), policy,
	); err != nil {
		t.Fatalf("privileged and receiver authorities rejected: %v", err)
	}

	for _, test := range []struct {
		name string
		mask uint32
	}{
		{name: "delete directory", mask: windows.DELETE},
		{name: "delete child", mask: windowsV3DirectoryDeleteChild},
		{name: "rewrite DACL", mask: windows.WRITE_DAC},
		{name: "replace owner", mask: windows.WRITE_OWNER},
	} {
		t.Run(test.name, func(t *testing.T) {
			descriptor := windowsV3TestSecurityDescriptor(
				t, policy.userSID, allow(users, fmt.Sprintf("0x%08x", test.mask), ""),
			)
			if err := windowsV3VerifyAncestryAuthorityDescriptor(descriptor, policy); err == nil {
				t.Fatal("cross-principal mutation authority was accepted")
			}
		})
	}

	deniedFirst := windowsV3TestSecurityDescriptor(
		t, policy.userSID, deny(users, allDangerous)+allow(users, allDangerous, ""),
	)
	if err := windowsV3VerifyAncestryAuthorityDescriptor(deniedFirst, policy); err != nil {
		t.Fatalf("deny-before-allow authority was rejected: %v", err)
	}
	allowedFirst := windowsV3TestSecurityDescriptor(
		t, policy.userSID, allow(users, allDangerous, "")+deny(users, allDangerous),
	)
	if err := windowsV3VerifyAncestryAuthorityDescriptor(allowedFirst, policy); err == nil {
		t.Fatal("allow-before-deny authority was accepted")
	}

	inheritOnly := windowsV3TestSecurityDescriptor(
		t, policy.userSID, allow(users, "GA", "CIIO"),
	)
	if err := windowsV3VerifyAncestryAuthorityDescriptor(inheritOnly, policy); err != nil {
		t.Fatalf("inherit-only authority was treated as current-object access: %v", err)
	}
	inheritedCurrent := windowsV3TestSecurityDescriptor(
		t, policy.userSID, allow(users, fmt.Sprintf("0x%08x", uint32(windows.DELETE)), "ID"),
	)
	if err := windowsV3VerifyAncestryAuthorityDescriptor(inheritedCurrent, policy); err == nil {
		t.Fatal("applicable inherited delete authority was accepted")
	}
}

func TestWindowsV3AncestryAuthorityClassifiesAdministratorAccountByNativeSID(t *testing.T) {
	if windowsV3IsAdministratorAccount(nil) || windowsV3IsAdministratorAccount(new(windows.SID)) {
		t.Fatal("an absent or invalid SID was classified as an Administrator account")
	}
	policy, err := newWindowsV3PrivatePolicy()
	if err != nil {
		t.Fatal(err)
	}
	accountDomain, err := windows.StringToSid("S-1-5-21-111111111-222222222-333333333")
	if err != nil {
		t.Fatal(err)
	}
	administrator, err := windows.CreateWellKnownDomainSid(windows.WinAccountAdministratorSid, accountDomain)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := windows.CreateWellKnownDomainSid(windows.WinAccountGuestSid, accountDomain)
	if err != nil {
		t.Fatal(err)
	}
	extraSubauthority, err := windows.StringToSid("S-1-5-21-1-2-3-4-500")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		sid    *windows.SID
		exempt bool
	}{
		{name: "administrator account", sid: administrator, exempt: true},
		{name: "administrator group", sid: policy.administratorsSID, exempt: true},
		{name: "ordinary account", sid: guest, exempt: false},
		{name: "suffix lookalike", sid: extraSubauthority, exempt: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := policy.ancestryExempts(test.sid); got != test.exempt {
				t.Fatalf("ancestry exemption = %t, want %t for %s", got, test.exempt, test.sid.String())
			}
		})
	}

	dangerous := fmt.Sprintf("0x%08x", uint32(windowsV3AncestryMutationRights))
	allow := func(sid *windows.SID) string {
		return fmt.Sprintf("(A;;%s;;;%s)", dangerous, sid.String())
	}
	privilegedDescriptor := windowsV3TestSecurityDescriptor(
		t,
		administrator,
		allow(administrator),
	)
	if err := windowsV3VerifyAncestryAuthorityDescriptor(privilegedDescriptor, policy); err != nil {
		t.Fatalf("Administrator ancestry authority was rejected: %v", err)
	}
	ordinaryOwner := windowsV3TestSecurityDescriptor(t, guest, "")
	if err := windowsV3VerifyAncestryAuthorityDescriptor(ordinaryOwner, policy); err == nil {
		t.Fatal("ordinary ancestry owner was accepted")
	}
	ordinaryTrustee := windowsV3TestSecurityDescriptor(
		t,
		policy.userSID,
		allow(guest),
	)
	if err := windowsV3VerifyAncestryAuthorityDescriptor(ordinaryTrustee, policy); err == nil {
		t.Fatal("ordinary ancestry mutation authority was accepted")
	}
}

func TestWindowsV3AncestryAuthorityFailsClosedOnAmbiguousACLs(t *testing.T) {
	policy, err := newWindowsV3PrivatePolicy()
	if err != nil {
		t.Fatal(err)
	}
	users, err := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
	if err != nil {
		t.Fatal(err)
	}
	generic := windowsV3TestSecurityDescriptor(
		t, policy.userSID, fmt.Sprintf("(A;;GA;;;%s)", users.String()),
	)
	if err := windowsV3VerifyAncestryAuthorityDescriptor(generic, policy); !errors.Is(err, errWindowsV3OutputUnsupported) {
		t.Fatalf("generic ACE error = %v", err)
	}

	for _, aceType := range []uint8{
		windowsV3AccessAllowedObjectACEType,
		windowsV3AccessAllowedCallbackACEType,
		0x7f,
	} {
		t.Run(fmt.Sprintf("ace-type-%d", aceType), func(t *testing.T) {
			descriptor := windowsV3TestSecurityDescriptor(
				t, policy.userSID, fmt.Sprintf("(A;;SD;;;%s)", users.String()),
			)
			dacl, _, err := descriptor.DACL()
			if err != nil {
				t.Fatal(err)
			}
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(dacl, 0, &ace); err != nil {
				t.Fatal(err)
			}
			ace.Header.AceType = aceType
			if err := windowsV3VerifyAncestryAuthorityDescriptor(descriptor, policy); !errors.Is(err, errWindowsV3OutputUnsupported) {
				t.Fatalf("ambiguous ACE error = %v", err)
			}
		})
	}

	foreignOwner := windowsV3TestSecurityDescriptor(t, users, "")
	if err := windowsV3VerifyAncestryAuthorityDescriptor(foreignOwner, policy); err == nil {
		t.Fatal("unprivileged owner was accepted")
	}
	nullDACL, err := windows.SecurityDescriptorFromString("O:" + policy.userSID.String() + "D:NO_ACCESS_CONTROL")
	if err != nil {
		t.Fatal(err)
	}
	if err := windowsV3VerifyAncestryAuthorityDescriptor(nullDACL, policy); err == nil {
		t.Fatal("null DACL was accepted")
	}
}

func TestWindowsV3PublicGuardRejectsHostileOutputRootDACL(t *testing.T) {
	rootPath := t.TempDir()
	policy, err := newWindowsV3PrivatePolicy()
	if err != nil {
		t.Fatal(err)
	}
	users, err := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString(
		"O:" + policy.userSID.String() + "D:P" +
			fmt.Sprintf("(A;OICI;GA;;;%s)", policy.userSID.String()) +
			fmt.Sprintf("(A;;SD;;;%s)", users.String()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := windowsV3SetTestDirectoryDACL(rootPath, descriptor, policy); err != nil {
		t.Fatal(err)
	}
	platform, err := openWindowsV3OutputPlatform(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	guard, err := platform.acquirePublicOperationGuard()
	if guard != nil {
		_ = guard.Close()
	}
	if !errors.Is(err, errWindowsV3OutputUnsafe) ||
		!errors.Is(err, outputfault.ErrAncestryAuthorityDenied) {
		t.Fatalf("hostile output-root DACL error = %v", err)
	}
}

func TestWindowsV3ExternalPlacementGuardPinsWithoutCertifyingHostileAncestorDACL(t *testing.T) {
	base := windowsV3NativeTestTempDir(t)
	external := filepath.Join(base, "external")
	moved := filepath.Join(base, "external-moved")
	if err := os.Mkdir(external, 0o700); err != nil {
		t.Fatal(err)
	}
	policy, err := newWindowsV3PrivatePolicy()
	if err != nil {
		t.Fatal(err)
	}
	if err := windowsV3SetTestDirectoryDACL(
		external, windowsV3HostileExternalDescriptor(t, policy), policy,
	); err != nil {
		t.Fatal(err)
	}
	platform, err := openWindowsV3OutputPlatform(external)
	if err != nil {
		t.Fatal(err)
	}
	if guard, guardErr := platform.acquirePublicOperationGuard(); guardErr == nil ||
		!errors.Is(guardErr, outputfault.ErrAncestryAuthorityDenied) {
		if guard != nil {
			_ = guard.Close()
		}
		t.Fatalf("hostile external directory was certified as an output root: %v", guardErr)
	}
	guard, err := platform.acquireExternalPlacementGuard()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(external, moved); !windowsV3IsBlockedAncestorReplacement(err) {
		_ = guard.Close()
		t.Fatalf("external placement guard allowed rename: %v", err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(external, moved); err != nil {
		t.Fatalf("external placement remained pinned after guard close: %v", err)
	}
	if err := os.Rename(moved, external); err != nil {
		t.Fatal(err)
	}
	if err := platform.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsV3ExternalPlacementDoesNotApplyOutputLookupPolicy(t *testing.T) {
	facts := validWindowsV3CertificationFacts()
	facts.caseSensitive = true
	if err := validateWindowsV3Certification(facts); !errors.Is(err, errWindowsV3OutputUnsupported) {
		t.Fatalf("case-sensitive output-root certification = %v", err)
	}
	if err := validateWindowsV3ExternalPlacement(facts, facts.object.volume); err != nil {
		t.Fatalf("case-sensitive external placement was rejected: %v", err)
	}
	mismatched := facts.object.volume
	mismatched.serial++
	if err := validateWindowsV3ExternalPlacement(facts, mismatched); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("cross-volume external placement error = %v", err)
	}
}

type windowsV3RecordedAncestryOpen struct {
	root             windows.Handle
	path             string
	objectAttributes uint32
	result           windows.Handle
}

type windowsV3RecordingAncestryOpener struct {
	delegate windowsV3AncestryDirectoryOpener
	records  []windowsV3RecordedAncestryOpen
}

func (opener *windowsV3RecordingAncestryOpener) Open(
	root windows.Handle,
	path string,
	access uint32,
	objectAttributes uint32,
) (windows.Handle, uintptr, error) {
	handle, status, err := opener.delegate.Open(root, path, access, objectAttributes)
	opener.records = append(opener.records, windowsV3RecordedAncestryOpen{
		root: root, path: path, objectAttributes: objectAttributes, result: handle,
	})
	return handle, status, err
}

func TestWindowsV3PlacementLeafAuthorityAcceptsHandleReportedDOSAlias(t *testing.T) {
	for _, test := range []struct {
		name          string
		caseSensitive bool
		want          bool
	}{
		{name: "case-insensitive", want: true},
		{name: "case-sensitive", caseSensitive: true, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			match, err := windowsV3PlacementLeafNamesMatch(
				"RUNNER~1", "runneradmin", "RUNNER~1", test.caseSensitive,
			)
			if err != nil || match != test.want {
				t.Fatalf("handle-reported alias match = (%t, %v), want %t", match, err, test.want)
			}
		})
	}
	match, err := windowsV3PlacementLeafNamesMatch("RUNNER~1", "runneradmin", "different", false)
	if err != nil || match {
		t.Fatalf("unreported placement alias match = (%t, %v), want false", match, err)
	}
}

func TestWindowsV3AncestryGuardResolvesOnlyDriveRootAbsolutely(t *testing.T) {
	rootPath := filepath.Join(windowsV3NativeTestTempDir(t), "external", "output")
	if err := os.MkdirAll(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	platform, err := openWindowsV3OutputPlatform(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	opener := &windowsV3RecordingAncestryOpener{delegate: nativeWindowsV3AncestryDirectoryOpener{}}
	guard, err := platform.acquireDirectoryAncestryGuardWithOpener(
		windowsV3GuardPublicOutputRoot, opener,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	paths, err := windowsV3AbsoluteDirectoryAncestry(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(opener.records) != len(paths) {
		t.Fatalf("ancestry opens=%d, want %d", len(opener.records), len(paths))
	}
	for index, record := range opener.records {
		if record.objectAttributes&windows.OBJ_DONT_REPARSE == 0 {
			t.Fatalf("ancestry open %d omitted OBJ_DONT_REPARSE", index)
		}
		if index == 0 {
			if record.root != 0 || record.path != windowsV3NTPath(paths[index]) {
				t.Fatalf("drive-root open root=%#x path=%q", record.root, record.path)
			}
			continue
		}
		previous := opener.records[index-1]
		if record.root == 0 || record.root != previous.result {
			t.Fatalf("ancestry open %d root=%#x, previous handle=%#x", index, record.root, previous.result)
		}
		if record.path != filepath.Base(paths[index]) || filepath.IsAbs(record.path) {
			t.Fatalf("ancestry open %d re-resolved path %q", index, record.path)
		}
	}
}

func windowsV3HostileExternalDescriptor(
	t *testing.T,
	policy *windowsV3PrivatePolicy,
) *windows.SECURITY_DESCRIPTOR {
	t.Helper()
	users, err := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString(
		"O:" + policy.userSID.String() + "D:P" +
			fmt.Sprintf("(A;OICI;GA;;;%s)", policy.userSID.String()) +
			fmt.Sprintf("(A;OICI;GA;;;%s)", policy.systemSID.String()) +
			fmt.Sprintf("(A;OICI;GA;;;%s)", policy.administratorsSID.String()) +
			fmt.Sprintf("(A;;SD;;;%s)", users.String()),
	)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func windowsV3IsBlockedAncestorReplacement(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

func TestWindowsV3PublicGuardPinsRootAndEveryExternalAncestor(t *testing.T) {
	base := windowsV3NativeTestTempDir(t)
	externalParent := filepath.Join(base, "external-parent")
	rootPath := filepath.Join(externalParent, "output")
	if err := os.MkdirAll(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	platform, err := openWindowsV3OutputPlatform(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	guard, err := platform.acquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}

	rootMoved := filepath.Join(externalParent, "output-moved")
	if err := os.Rename(rootPath, rootMoved); err == nil {
		_ = guard.Close()
		t.Fatal("guard allowed output-root rename")
	}
	externalMoved := filepath.Join(base, "external-parent-moved")
	if err := os.Rename(externalParent, externalMoved); err == nil {
		_ = guard.Close()
		t.Fatal("guard allowed external-ancestor rename")
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(rootPath, rootMoved); err != nil {
		t.Fatalf("root rename remained blocked after guard close: %v", err)
	}
	if err := os.Rename(rootMoved, rootPath); err != nil {
		t.Fatal(err)
	}
	if err := platform.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(externalParent, externalMoved); err != nil {
		t.Fatalf("external-ancestor rename remained blocked after all authorities closed: %v", err)
	}
	if err := os.Rename(externalMoved, externalParent); err != nil {
		t.Fatal(err)
	}
}
