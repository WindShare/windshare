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

func TestWindowsV3PublicGuardAcceptsOrdinaryCrossPrincipalDACL(t *testing.T) {
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
	if err != nil {
		t.Fatalf("ordinary public DACL was rejected: %v", err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsV3PublicGuardReportsDeniedNativeAuthority(t *testing.T) {
	rootPath := t.TempDir()
	policy, err := newWindowsV3PrivatePolicy()
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
	if err := windowsV3SetTestDirectoryDACL(rootPath, denied, policy); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := windowsV3SetTestDirectoryDACL(rootPath, restore, policy); err != nil {
			t.Errorf("restore test directory DACL: %v", err)
		}
	}()

	if platform, err := openWindowsV3OutputPlatform(rootPath); platform != nil {
		_ = platform.Close()
		t.Fatal("read-only destination unexpectedly admitted mutation authority")
	} else if !errors.Is(err, outputfault.ErrAncestryAuthorityDenied) ||
		!errors.Is(err, errWindowsV3OutputUnsafe) ||
		!errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("denied native authority error = %v", err)
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
	if err := os.Rename(external, moved); !windowsV3IsBlockedAncestorReplacement(err) {
		_ = guard.Close()
		t.Fatalf("external placement guard allowed rename: %v", err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	windowsV3RequireReleasedRename(t, external, moved, "external placement after guard close")
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

func windowsV3RequireReleasedRename(t *testing.T, path, moved, authority string) {
	t.Helper()
	if err := os.Rename(path, moved); err != nil {
		t.Fatalf("%s remained pinned: %v", authority, err)
	}
	// The one-way move is the complete release proof. Reversing it exercises an
	// unrelated rename that Windows can transiently deny while nested closes settle.
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
	windowsV3RequireReleasedRename(t, rootPath, rootMoved, "root after guard close")
	if err := platform.Close(); err != nil {
		t.Fatal(err)
	}
	windowsV3RequireReleasedRename(
		t, externalParent, externalMoved, "external ancestor after all authorities closed",
	)
}
