//go:build windows

package outputwindows

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"golang.org/x/sys/windows"
)

func TestWindowsV3CreateRootPinsEveryExternalAndCreatedComponent(t *testing.T) {
	base := windowsV3NativeTestTempDir(t)
	external := filepath.Join(base, "external")
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
	target := filepath.Join(external, "missing-parent", "output")
	var placementCuts, componentCuts int
	observer := windowsV3OutputRootCreateObserverFunc(func(
		pinned string,
		cut windowsV3OutputRootCreateCut,
	) error {
		switch cut {
		case windowsV3OutputRootCreatePlacementPinned:
			placementCuts++
		case windowsV3OutputRootCreateComponentPinned:
			componentCuts++
		default:
			return fmt.Errorf("unexpected output-root creation cut %d", cut)
		}
		for _, path := range []string{external, pinned} {
			moved := path + "-moved"
			err := os.Rename(path, moved)
			if err == nil {
				_ = os.Rename(moved, path)
				return fmt.Errorf("creation cut %d allowed rename of pinned path %q", cut, path)
			}
			if !windowsV3IsBlockedAncestorReplacement(err) {
				return fmt.Errorf("creation cut %d rename of %q failed ambiguously: %w", cut, path, err)
			}
		}
		return nil
	})
	platform, err := windowsCreateCertifiedOutputRootWithObserver(target, observer)
	if err != nil {
		t.Fatal(err)
	}
	if placementCuts != 1 || componentCuts != 2 {
		t.Fatalf("root creation cuts placement=%d components=%d", placementCuts, componentCuts)
	}
	guard, err := platform.acquirePublicOperationGuard()
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	if entries, readErr := os.ReadDir(target); readErr != nil || len(entries) != 0 {
		_ = guard.Close()
		_ = platform.Close()
		t.Fatalf("certified root contains state/content entries=%v error=%v", entries, readErr)
	}
	if err := errors.Join(guard.Close(), platform.Close()); err != nil {
		t.Fatal(err)
	}
	externalMoved := external + "-moved"
	if err := os.Rename(external, externalMoved); err != nil {
		t.Fatalf("created-root ancestry remained pinned after cleanup: %v", err)
	}
	if err := os.Rename(externalMoved, external); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsV3PrivatePublicationRootCreationPinsHostileInheritedParent(t *testing.T) {
	base := windowsV3NativeTestTempDir(t)
	parentPath := filepath.Join(base, "hostile-parent")
	if err := os.Mkdir(parentPath, 0o700); err != nil {
		t.Fatal(err)
	}
	policy, err := newWindowsV3PrivatePolicy()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, foreign := windowsV3HostileInheritedParentDescriptor(t, policy)
	if err := windowsV3SetTestDirectoryDACL(parentPath, descriptor, policy); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(parentPath, ".guard-uploads")
	var placementCuts, componentCuts int
	observer := windowsV3OutputRootCreateObserverFunc(func(
		pinned string,
		cut windowsV3OutputRootCreateCut,
	) error {
		if err := windowsV3AssertRenameBlocked(parentPath); err != nil {
			return fmt.Errorf("creation cut %d: %w", cut, err)
		}
		switch cut {
		case windowsV3OutputRootCreatePlacementPinned:
			placementCuts++
			if pinned != parentPath {
				return fmt.Errorf("placement cut pinned %q, want %q", pinned, parentPath)
			}
			if err := windowsV3AssertRemoveBlocked(parentPath); err != nil {
				return fmt.Errorf("placement cut: %w", err)
			}
		case windowsV3OutputRootCreateComponentPinned:
			componentCuts++
			if pinned != target {
				return fmt.Errorf("component cut pinned %q, want %q", pinned, target)
			}
			if err := windowsV3AssertRenameBlocked(target); err != nil {
				return fmt.Errorf("component cut: %w", err)
			}
			if err := windowsV3AssertRemoveBlocked(target); err != nil {
				return fmt.Errorf("component cut: %w", err)
			}
		default:
			return fmt.Errorf("unexpected private publication-root creation cut %d", cut)
		}
		return nil
	})
	platform, err := createWindowsV3PrivatePublicationRootWithObserver(target, observer)
	if err != nil {
		t.Fatal(err)
	}
	if placementCuts != 1 || componentCuts != 1 {
		_ = platform.Close()
		t.Fatalf("private root creation cuts placement=%d components=%d", placementCuts, componentCuts)
	}
	if err := platform.root.native.verify(true); err != nil {
		_ = platform.Close()
		t.Fatalf("created publication root is not private: %v", err)
	}
	installed, err := windows.GetNamedSecurityInfo(
		target,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	inherited, err := windowsV3DescriptorHasInheritedAllowForSID(installed, foreign)
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	if inherited {
		_ = platform.Close()
		t.Fatal("protected publication root inherited foreign mutation authority")
	}
	if entries, readErr := os.ReadDir(target); readErr != nil || len(entries) != 0 {
		_ = platform.Close()
		t.Fatalf("created publication root entries=%v error=%v", entries, readErr)
	}
	if err := platform.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatalf("publication root remained pinned after platform close: %v", err)
	}
	if entries, readErr := os.ReadDir(parentPath); readErr != nil || len(entries) != 0 {
		t.Fatalf("private publication-root test left residue entries=%v error=%v", entries, readErr)
	}
}

func TestWindowsV3PrivatePublicationRootFailureRemovesOwnedTarget(t *testing.T) {
	base := windowsV3NativeTestTempDir(t)
	parentPath := filepath.Join(base, "hostile-parent")
	if err := os.Mkdir(parentPath, 0o700); err != nil {
		t.Fatal(err)
	}
	policy, err := newWindowsV3PrivatePolicy()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, _ := windowsV3HostileInheritedParentDescriptor(t, policy)
	if err := windowsV3SetTestDirectoryDACL(parentPath, descriptor, policy); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(parentPath, ".guard-uploads")
	injected := errors.New("injected post-certification failure")
	observer := windowsV3OutputRootCreateObserverFunc(func(
		_ string,
		cut windowsV3OutputRootCreateCut,
	) error {
		if cut == windowsV3OutputRootCreateComponentPinned {
			return injected
		}
		return nil
	})
	platform, err := createWindowsV3PrivatePublicationRootWithObserver(target, observer)
	if platform != nil {
		_ = platform.Close()
		t.Fatal("failed private publication-root creation transferred a live platform")
	}
	if !errors.Is(err, injected) {
		t.Fatalf("private publication-root failure = %v", err)
	}
	if _, statErr := os.Stat(target); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("failed private publication-root creation left target: %v", statErr)
	}
	if entries, readErr := os.ReadDir(parentPath); readErr != nil || len(entries) != 0 {
		t.Fatalf("failed private publication-root creation left entries=%v error=%v", entries, readErr)
	}
	movedParent := parentPath + "-moved"
	if err := os.Rename(parentPath, movedParent); err != nil {
		t.Fatalf("failed creation retained a parent placement handle: %v", err)
	}
	if err := os.Rename(movedParent, parentPath); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsV3PrivatePublicationRootRejectsUnsafeExistingRootWithoutRepair(t *testing.T) {
	base := windowsV3NativeTestTempDir(t)
	parentPath := filepath.Join(base, "hostile-parent")
	if err := os.Mkdir(parentPath, 0o700); err != nil {
		t.Fatal(err)
	}
	policy, err := newWindowsV3PrivatePolicy()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, foreign := windowsV3HostileInheritedParentDescriptor(t, policy)
	if err := windowsV3SetTestDirectoryDACL(parentPath, descriptor, policy); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(parentPath, ".guard-uploads")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	before, err := windows.GetNamedSecurityInfo(
		target,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatal(err)
	}
	inherited, err := windowsV3DescriptorHasInheritedAllowForSID(before, foreign)
	if err != nil {
		t.Fatal(err)
	}
	if !inherited {
		t.Fatal("unsafe existing-root fixture did not inherit foreign authority")
	}
	beforeSDDL := before.String()
	if beforeSDDL == "" {
		t.Fatal("encode unsafe existing-root descriptor")
	}
	if verifyErr := windowsV3VerifyAncestryAuthorityDescriptor(before, policy); verifyErr == nil {
		t.Fatalf("unsafe existing-root fixture was certifiable: descriptor=%q", beforeSDDL)
	}
	native, err := openWindowsV3OutputPlatform(target)
	if err != nil {
		t.Fatal(err)
	}
	guard, guardErr := native.acquirePublicOperationGuard()
	if guard != nil {
		_ = guard.Close()
	}
	if closeErr := native.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if guardErr == nil {
		t.Fatalf("native public guard accepted unsafe fixture: descriptor=%q", beforeSDDL)
	}

	platform, err := OpenPrivatePublicationRoot(target, true)
	if platform != nil {
		_ = platform.Close()
		t.Fatalf("unsafe existing publication root was accepted: descriptor=%q", beforeSDDL)
	}
	if !errors.Is(err, outputcap.ErrUnsafeNamespace) ||
		!errors.Is(err, outputfault.ErrAncestryAuthorityDenied) {
		t.Fatalf("unsafe existing publication-root error = %v", err)
	}
	after, readErr := windows.GetNamedSecurityInfo(
		target,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if after.String() != beforeSDDL {
		t.Fatalf("unsafe existing publication-root DACL was repaired: before=%q after=%q", beforeSDDL, after.String())
	}
	if verifyErr := windowsV3VerifyAncestryAuthorityDescriptor(after, policy); verifyErr == nil {
		t.Fatal("unsafe existing publication root became certifiable after rejection")
	}
	if entries, readErr := os.ReadDir(target); readErr != nil || len(entries) != 0 {
		t.Fatalf("unsafe existing publication-root rejection changed entries=%v error=%v", entries, readErr)
	}
}

func TestWindowsV3OutputRootCreateTransfersResultOnlyAfterCleanPinRelease(t *testing.T) {
	cleanupErr := errors.New("injected creation-pin cleanup failure")
	closeErr := errors.New("injected returned-platform close failure")
	closeCalls := 0
	keep, err := finishWindowsV3OutputRootCreate(nil, cleanupErr, func() error {
		closeCalls++
		return closeErr
	})
	if keep || closeCalls != 1 || !errors.Is(err, cleanupErr) || !errors.Is(err, closeErr) {
		t.Fatalf("failed constructor ownership = (keep=%t, closes=%d, err=%v)", keep, closeCalls, err)
	}

	closeCalls = 0
	keep, err = finishWindowsV3OutputRootCreate(nil, nil, func() error {
		closeCalls++
		return closeErr
	})
	if !keep || err != nil || closeCalls != 0 {
		t.Fatalf("successful constructor ownership = (keep=%t, closes=%d, err=%v)", keep, closeCalls, err)
	}
}

func windowsV3HostileInheritedParentDescriptor(
	t *testing.T,
	policy *windowsV3PrivatePolicy,
) (*windows.SECURITY_DESCRIPTOR, *windows.SID) {
	t.Helper()
	users, err := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
	if err != nil {
		t.Fatal(err)
	}
	entries := windowsV3InheritableFullAccessEntries([]*windows.SID{
		policy.userSID,
		policy.systemSID,
		policy.administratorsSID,
		users,
	})
	descriptor, err := windows.SecurityDescriptorFromString(
		"O:" + policy.userSID.String() + "D:P" + entries,
	)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor, users
}

func windowsV3DescriptorHasInheritedAllowForSID(
	descriptor *windows.SECURITY_DESCRIPTOR,
	principal *windows.SID,
) (bool, error) {
	if descriptor == nil || principal == nil || !principal.IsValid() {
		return false, errors.New("security descriptor or principal is absent")
	}
	dacl, defaulted, err := descriptor.DACL()
	if err != nil || dacl == nil || defaulted {
		return false, errors.Join(errors.New("security descriptor DACL is absent or defaulted"), err)
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return false, err
		}
		if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
			ace.Header.AceFlags&windows.INHERITED_ACE == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if sid.IsValid() && sid.Equals(principal) {
			return true, nil
		}
	}
	return false, nil
}

func windowsV3AssertRenameBlocked(path string) error {
	moved := path + "-moved"
	err := os.Rename(path, moved)
	if err == nil {
		_ = os.Rename(moved, path)
		return fmt.Errorf("pinned path %q was renameable", path)
	}
	if !windowsV3IsBlockedAncestorReplacement(err) {
		return fmt.Errorf("rename pinned path %q failed ambiguously: %w", path, err)
	}
	return nil
}

func windowsV3AssertRemoveBlocked(path string) error {
	err := os.Remove(path)
	if err == nil {
		return fmt.Errorf("pinned path %q was removable", path)
	}
	if !windowsV3IsBlockedAncestorReplacement(err) {
		return fmt.Errorf("remove pinned path %q failed ambiguously: %w", path, err)
	}
	return nil
}
