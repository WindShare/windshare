//go:build windows

package outputwindows

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputfault"
	"golang.org/x/sys/windows"
)

func windowsNativeTestTempDir(t *testing.T) string {
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

func TestWindowsPublicGuardAcceptsOrdinaryCrossPrincipalDACL(t *testing.T) {
	rootPath := t.TempDir()
	policy, err := newWindowsPrivatePolicy()
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
	if err := windowsSetTestDirectoryDACL(rootPath, descriptor, policy); err != nil {
		t.Fatal(err)
	}
	platform, err := openWindowsOutputPlatform(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	guard, err := platform.acquirePublicOperationGuard()
	if err != nil {
		t.Fatalf("ordinary public DACL was rejected: %v", err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsPublicGuardReportsDeniedNativeAuthority(t *testing.T) {
	rootPath := t.TempDir()
	policy, err := newWindowsPrivatePolicy()
	if err != nil {
		t.Fatal(err)
	}
	denied, err := windows.SecurityDescriptorFromString(
		"O:" + policy.userSID.String() + "D:P" +
			fmt.Sprintf("(A;OICI;GRGX;;;%s)", policy.userSID.String()),
	)
	if err != nil {
		t.Fatal(err)
	}
	restore, err := policy.descriptor(true)
	if err != nil {
		t.Fatal(err)
	}
	if err := windowsSetTestDirectoryDACL(rootPath, denied, policy); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := windowsSetTestDirectoryDACL(rootPath, restore, policy); err != nil {
			t.Errorf("restore test directory DACL: %v", err)
		}
	}()

	if platform, err := openWindowsOutputPlatform(rootPath); platform != nil {
		_ = platform.Close()
		t.Fatal("read-only destination unexpectedly admitted mutation authority")
	} else if !errors.Is(err, outputfault.ErrAncestryAuthorityDenied) ||
		!errors.Is(err, errWindowsOutputUnsafe) ||
		!errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("denied native authority error = %v", err)
	}
}

func TestWindowsExternalPlacementGuardPinsWithoutCertifyingHostileAncestorDACL(t *testing.T) {
	base := windowsNativeTestTempDir(t)
	external := filepath.Join(base, "external")
	moved := filepath.Join(base, "external-moved")
	if err := os.Mkdir(external, 0o700); err != nil {
		t.Fatal(err)
	}
	policy, err := newWindowsPrivatePolicy()
	if err != nil {
		t.Fatal(err)
	}
	if err := windowsSetTestDirectoryDACL(
		external, windowsHostileExternalDescriptor(t, policy), policy,
	); err != nil {
		t.Fatal(err)
	}
	platform, err := openWindowsOutputPlatform(external)
	if err != nil {
		t.Fatal(err)
	}
	publicGuard, guardErr := platform.acquirePublicOperationGuard()
	if guardErr != nil {
		t.Fatalf("ordinary inherited DACL was rejected for public output: %v", guardErr)
	}
	if err := publicGuard.Close(); err != nil {
		t.Fatal(err)
	}
	guard, err := platform.acquireExternalPlacementGuard()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(external, moved); !windowsIsBlockedAncestorReplacement(err) {
		_ = guard.Close()
		t.Fatalf("external placement guard allowed rename: %v", err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	windowsRequireReleasedRename(t, external, moved, "external placement after guard close")
	if err := platform.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsExternalPlacementDoesNotApplyOutputLookupPolicy(t *testing.T) {
	facts := validWindowsObjectFacts()
	facts.caseSensitive = true
	if err := validateWindowsRootShape(facts); !errors.Is(err, errWindowsOutputUnsupported) {
		t.Fatalf("case-sensitive output-root identityScheme = %v", err)
	}
	if err := validateWindowsExternalPlacement(facts, facts.object.volume); err != nil {
		t.Fatalf("case-sensitive external placement was rejected: %v", err)
	}
	mismatched := facts.object.volume
	mismatched.serial++
	if err := validateWindowsExternalPlacement(facts, mismatched); !errors.Is(err, errWindowsOutputUnsafe) {
		t.Fatalf("cross-volume external placement error = %v", err)
	}
}

type windowsRecordedAncestryOpen struct {
	root             windows.Handle
	path             string
	objectAttributes uint32
	result           windows.Handle
}

type windowsRecordingAncestryOpener struct {
	delegate windowsAncestryDirectoryOpener
	records  []windowsRecordedAncestryOpen
}

func (opener *windowsRecordingAncestryOpener) Open(
	root windows.Handle,
	path string,
	access uint32,
	objectAttributes uint32,
) (windows.Handle, uintptr, error) {
	handle, status, err := opener.delegate.Open(root, path, access, objectAttributes)
	opener.records = append(opener.records, windowsRecordedAncestryOpen{
		root: root, path: path, objectAttributes: objectAttributes, result: handle,
	})
	return handle, status, err
}

func TestWindowsPlacementLeafAuthorityAcceptsHandleReportedDOSAlias(t *testing.T) {
	for _, test := range []struct {
		name          string
		caseSensitive bool
		want          bool
	}{
		{name: "case-insensitive", want: true},
		{name: "case-sensitive", caseSensitive: true, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			match, err := windowsPlacementLeafNamesMatch(
				"RUNNER~1", "runneradmin", "RUNNER~1", test.caseSensitive,
			)
			if err != nil || match != test.want {
				t.Fatalf("handle-reported alias match = (%t, %v), want %t", match, err, test.want)
			}
		})
	}
	match, err := windowsPlacementLeafNamesMatch("RUNNER~1", "runneradmin", "different", false)
	if err != nil || match {
		t.Fatalf("unreported placement alias match = (%t, %v), want false", match, err)
	}
}

func TestWindowsAncestryGuardResolvesOnlyDriveRootAbsolutely(t *testing.T) {
	rootPath := filepath.Join(windowsNativeTestTempDir(t), "external", "output")
	if err := os.MkdirAll(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	platform, err := openWindowsOutputPlatform(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	opener := &windowsRecordingAncestryOpener{delegate: nativeWindowsAncestryDirectoryOpener{}}
	guard, err := platform.acquireDirectoryAncestryGuardWithOpener(
		windowsGuardPublicOutputRoot, opener,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	paths, err := windowsAbsoluteDirectoryAncestry(rootPath)
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
			if record.root != 0 || record.path != windowsNTPath(paths[index]) {
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

func windowsHostileExternalDescriptor(
	t *testing.T,
	policy *windowsPrivatePolicy,
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

func windowsIsBlockedAncestorReplacement(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

func windowsRequireReleasedRename(t *testing.T, path, moved, authority string) {
	t.Helper()
	if err := os.Rename(path, moved); err != nil {
		t.Fatalf("%s remained pinned: %v", authority, err)
	}
	// The one-way move is the complete release proof. Reversing it exercises an
	// unrelated rename that Windows can transiently deny while nested closes settle.
}

func TestWindowsPublicGuardPinsRootAndEveryExternalAncestor(t *testing.T) {
	base := windowsNativeTestTempDir(t)
	externalParent := filepath.Join(base, "external-parent")
	rootPath := filepath.Join(externalParent, "output")
	if err := os.MkdirAll(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	platform, err := openWindowsOutputPlatform(rootPath)
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
	windowsRequireReleasedRename(t, rootPath, rootMoved, "root after guard close")
	if err := platform.Close(); err != nil {
		t.Fatal(err)
	}
	windowsRequireReleasedRename(
		t, externalParent, externalMoved, "external ancestor after all authorities closed",
	)
}
