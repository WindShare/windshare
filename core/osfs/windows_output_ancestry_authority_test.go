//go:build windows

package osfs

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

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
	if err := windowsV3SetTestDirectoryDescriptor(rootPath, descriptor); err != nil {
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
		!errors.Is(err, errOutputAncestryAuthorityDenied) ||
		outputAncestryTraceDecision(err) != FilesystemOutputAncestryAuthorityDenied {
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
	if err := windowsV3SetTestDirectoryDescriptor(
		external, windowsV3HostileExternalDescriptor(t, policy),
	); err != nil {
		t.Fatal(err)
	}
	platform, err := openWindowsV3OutputPlatform(external)
	if err != nil {
		t.Fatal(err)
	}
	if guard, guardErr := platform.acquirePublicOperationGuard(); guardErr == nil ||
		!errors.Is(guardErr, errOutputAncestryAuthorityDenied) {
		if guard != nil {
			_ = guard.Close()
		}
		t.Fatalf("hostile external directory was certified as an output root: %v", guardErr)
	}
	guard, err := platform.acquireExternalPlacementGuard()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(external, moved); !v3RecoveryIsBlockedAncestorReplacement(err) {
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
	if err := windowsV3SetTestDirectoryDescriptor(
		external, windowsV3HostileExternalDescriptor(t, policy),
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
			if !v3RecoveryIsBlockedAncestorReplacement(err) {
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

type windowsV3CountingObjectIDProvider struct {
	identity windowsV3PersistentObjectID
	err      error
	calls    atomic.Int64
}

func (provider *windowsV3CountingObjectIDProvider) CreateOrGet(
	windows.Handle,
) (windowsV3PersistentObjectID, error) {
	provider.calls.Add(1)
	return provider.identity, provider.err
}

func windowsV3OpenGuardedTestRoot(t *testing.T) (*windowsV3OutputPlatform, *windowsV3PublicOperationGuard) {
	t.Helper()
	platform, err := openWindowsV3OutputPlatform(windowsV3NativeTestTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := platform.Close(); err != nil {
			t.Errorf("close native test platform: %v", err)
		}
	})
	guard, err := platform.acquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := guard.Close(); err != nil {
			t.Errorf("close native ancestry guard: %v", err)
		}
	})
	return platform, guard
}

func TestWindowsV3IdentityPreparationIsTheOnlyObjectIDMutationBoundary(t *testing.T) {
	_, guard := windowsV3OpenGuardedTestRoot(t)
	root := guard.Root()
	provider := &windowsV3CountingObjectIDProvider{identity: windowsV3PersistentObjectID{0x41}}
	root.objectIDs = provider
	root.objectIDState = newWindowsV3PersistentObjectIDState()

	if _, err := root.identityClaim(); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("unprepared read-only claim error = %v", err)
	}
	if calls := provider.calls.Load(); calls != 0 {
		t.Fatalf("read-only claim invoked CreateOrGet %d times", calls)
	}
	claim, err := root.prepareIdentityClaim()
	if err != nil {
		t.Fatal(err)
	}
	if len(claim) == 0 || len(claim) > windowsV3DirectoryClaimMaxBytes {
		t.Fatalf("prepared claim length = %d", len(claim))
	}
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("first preparation invoked CreateOrGet %d times", calls)
	}
	repeated, err := root.identityClaim()
	if err != nil || !bytes.Equal(claim, repeated) {
		t.Fatalf("read-only claim differs after preparation: equal=%t error=%v", bytes.Equal(claim, repeated), err)
	}
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("read-only prepared claim invoked CreateOrGet %d times", calls)
	}
	preparedAgain, err := root.prepareIdentityClaim()
	if err != nil || !bytes.Equal(claim, preparedAgain) {
		t.Fatalf("idempotent preparation differs: equal=%t error=%v", bytes.Equal(claim, preparedAgain), err)
	}
	if calls := provider.calls.Load(); calls != 2 {
		t.Fatalf("second preparation invoked CreateOrGet %d times", calls)
	}
}

func TestWindowsV3IdentityPreparationValidatesBeforeAndAfterCreateOrGet(t *testing.T) {
	_, guard := windowsV3OpenGuardedTestRoot(t)
	root := guard.Root()
	provider := &windowsV3CountingObjectIDProvider{identity: windowsV3PersistentObjectID{0x42}}
	root.objectIDs = provider
	root.objectIDState = newWindowsV3PersistentObjectIDState()

	injected := errors.New("injected ancestry authority failure")
	root.ancestryAuthority = windowsV3AncestryAuthorityVerifierFunc(func(windows.Handle) error { return injected })
	if _, err := root.prepareIdentityClaim(); !errors.Is(err, errWindowsV3OutputUnsafe) ||
		!errors.Is(err, errOutputAncestryAuthorityDenied) ||
		outputAncestryTraceDecision(err) != FilesystemOutputAncestryAuthorityDenied {
		t.Fatalf("pre-authority failure = %v", err)
	}
	if calls := provider.calls.Load(); calls != 0 {
		t.Fatalf("failed pre-authority check invoked CreateOrGet %d times", calls)
	}

	nativeInspector := nativeWindowsV3HandleInspector{}
	facts, err := nativeInspector.Inspect(root.handle())
	if err != nil {
		t.Fatal(err)
	}
	var inspections atomic.Int64
	root.ancestryAuthority = windowsV3AncestryAuthorityVerifierFunc(func(windows.Handle) error { return nil })
	root.inspector = windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) {
		if inspections.Add(1) >= 3 {
			return windowsV3HandleFacts{}, nil
		}
		return facts, nil
	})
	_, err = root.prepareIdentityClaim()
	mapped := windowsOutputV3Error(err)
	if !errors.Is(err, errWindowsV3OutputUnsafe) ||
		errors.Is(err, errOutputAncestryAuthorityDenied) ||
		!errors.Is(mapped, errOutputV3Unsafe) ||
		outputAncestryTraceDecision(mapped) != FilesystemOutputAncestryStructuralUnsafe {
		t.Fatalf("post-CreateOrGet incarnation failure = %v", mapped)
	}
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("post-validation failure CreateOrGet calls = %d", calls)
	}
	root.inspector = nativeInspector
	if identity, prepared, identityErr := root.cachedPersistentObjectID(); identityErr != nil || prepared || identity.valid() {
		t.Fatalf("post-validation failure published identity=%x prepared=%t error=%v", identity, prepared, identityErr)
	}
}

func TestWindowsV3ReadOnlyIdentityClaimPreservesAuthorityTraceTaxonomy(t *testing.T) {
	_, guard := windowsV3OpenGuardedTestRoot(t)
	root := guard.Root()
	provider := &windowsV3CountingObjectIDProvider{identity: windowsV3PersistentObjectID{0x45}}
	root.objectIDs = provider
	root.objectIDState = newWindowsV3PersistentObjectIDState()
	root.ancestryAuthority = windowsV3AncestryAuthorityVerifierFunc(func(windows.Handle) error { return nil })
	if _, err := root.prepareIdentityClaim(); err != nil {
		t.Fatal(err)
	}

	root.ancestryAuthority = windowsV3AncestryAuthorityVerifierFunc(func(windows.Handle) error {
		return errors.Join(errWindowsV3OutputUnsupported, errors.New("injected ACL ambiguity"))
	})
	if _, err := root.identityClaim(); !errors.Is(err, errOutputAncestryAuthorityDenied) ||
		!errors.Is(err, errWindowsV3OutputUnsupported) ||
		outputAncestryTraceDecision(err) != FilesystemOutputAncestryAuthorityDenied {
		t.Fatalf("read-only authority trace taxonomy = %v", err)
	}
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("read-only authority denial invoked CreateOrGet %d times", calls)
	}

	structural := errors.New("injected handle inspection failure")
	root.ancestryAuthority = windowsV3AncestryAuthorityVerifierFunc(func(windows.Handle) error { return nil })
	root.inspector = windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) {
		return windowsV3HandleFacts{}, structural
	})
	_, err := root.identityClaim()
	mapped := windowsOutputV3Error(err)
	if !errors.Is(err, structural) ||
		errors.Is(err, errOutputAncestryAuthorityDenied) ||
		!errors.Is(mapped, errOutputV3Unsafe) ||
		outputAncestryTraceDecision(mapped) != FilesystemOutputAncestryStructuralUnsafe {
		t.Fatalf("read-only structural trace taxonomy = %v", mapped)
	}
}

func TestWindowsV3IdentityClaimsAreRaceSafeAndDeterministic(t *testing.T) {
	_, guard := windowsV3OpenGuardedTestRoot(t)
	root := guard.Root()
	root.ancestryAuthority = windowsV3AncestryAuthorityVerifierFunc(func(windows.Handle) error { return nil })
	provider := &windowsV3CountingObjectIDProvider{identity: windowsV3PersistentObjectID{0x43}}
	root.objectIDs = provider
	root.objectIDState = newWindowsV3PersistentObjectIDState()
	want, err := root.prepareIdentityClaim()
	if err != nil {
		t.Fatal(err)
	}

	const workers = 48
	errorsByWorker := make(chan error, workers)
	var group sync.WaitGroup
	for index := range workers {
		group.Add(1)
		go func(prepare bool) {
			defer group.Done()
			var claim []byte
			var err error
			if prepare {
				claim, err = root.prepareIdentityClaim()
			} else {
				claim, err = root.identityClaim()
			}
			if err != nil || !bytes.Equal(claim, want) {
				errorsByWorker <- fmt.Errorf("claim equal=%t: %w", bytes.Equal(claim, want), err)
			}
		}(index%2 == 0)
	}
	group.Wait()
	close(errorsByWorker)
	for err := range errorsByWorker {
		t.Error(err)
	}
}

func TestWindowsV3FreshWrapperMustPrepareDespiteSameFileID(t *testing.T) {
	_, guard := windowsV3OpenGuardedTestRoot(t)
	root := guard.Root()
	root.ancestryAuthority = windowsV3AncestryAuthorityVerifierFunc(func(windows.Handle) error { return nil })
	provider := &windowsV3CountingObjectIDProvider{identity: windowsV3PersistentObjectID{0x44}}
	root.objectIDs = provider
	root.objectIDState = newWindowsV3PersistentObjectIDState()
	const name = "fresh-selected-directory"
	if err := os.Mkdir(filepath.Join(root.path, name), 0o700); err != nil {
		t.Fatal(err)
	}
	first, err := root.OpenDirectory(name)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := first.prepareIdentityClaim()
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := root.OpenDirectory(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.identityClaim(); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("fresh wrapper inherited FileID-keyed authority: %v", err)
	}
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("unprepared fresh wrapper invoked CreateOrGet %d times", calls)
	}
	rebound, err := reopened.prepareIdentityClaim()
	if err != nil || !bytes.Equal(prepared, rebound) {
		t.Fatalf("fresh wrapper preparation equal=%t error=%v", bytes.Equal(prepared, rebound), err)
	}
	if calls := provider.calls.Load(); calls != 2 {
		t.Fatalf("fresh wrapper preparation invoked CreateOrGet %d times", calls)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root.path, name)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root.path, name), 0o700); err != nil {
		t.Fatal(err)
	}
	replacement, err := root.OpenDirectory(name)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if _, err := replacement.identityClaim(); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("replacement FileID inherited cached claim: %v", err)
	}
	if calls := provider.calls.Load(); calls != 2 {
		t.Fatalf("replacement read-only miss invoked CreateOrGet %d times", calls)
	}
}

func TestWindowsV3GuardedDirectoryProvenanceSurvivesEveryReopenLane(t *testing.T) {
	rootPath := windowsV3NativeTestTempDir(t)
	platform, err := openOutputV3Platform(rootPath, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := platform.Close(); err != nil {
			t.Errorf("close output platform: %v", err)
		}
	})
	guard, err := platform.AcquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := guard.Close(); err != nil {
			t.Errorf("close output ancestry guard: %v", err)
		}
	})
	root := guard.Root()
	assertWindowsV3DirectoryProvenance(t, "guard root", root, false)
	rootClaim, err := root.PrepareIdentityClaim()
	if err != nil {
		t.Fatal(err)
	}

	duplicate, err := root.Duplicate()
	if err != nil {
		t.Fatal(err)
	}
	assertWindowsV3DirectoryProvenance(t, "duplicate", duplicate, false)
	duplicateNative := duplicate.(*windowsOutputV3Directory).native
	rootNative := root.(*windowsOutputV3Directory).native
	if duplicateNative.objectIDState != rootNative.objectIDState {
		t.Fatal("true duplicate did not share the prepared handle-local identity state")
	}
	if claim, claimErr := duplicate.IdentityClaim(); claimErr != nil || !bytes.Equal(claim, rootClaim) {
		t.Fatalf("duplicate identity equal=%t error=%v", bytes.Equal(claim, rootClaim), claimErr)
	}
	if err := duplicate.Close(); err != nil {
		t.Fatal(err)
	}

	const publicName = "selected-provenance"
	created, err := root.CreateDirectory(publicName, false)
	if err != nil {
		t.Fatal(err)
	}
	assertWindowsV3DirectoryProvenance(t, "created public directory", created, false)
	createdClaim, err := created.PrepareIdentityClaim()
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := root.OpenDirectory(publicName, false)
	if err != nil {
		t.Fatal(err)
	}
	assertWindowsV3DirectoryProvenance(t, "reopened public directory", reopened, false)
	if _, err := reopened.IdentityClaim(); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("fresh public reopen inherited prepared identity state: %v", err)
	}
	reopenedClaim, err := reopened.PrepareIdentityClaim()
	if err != nil || !bytes.Equal(createdClaim, reopenedClaim) {
		t.Fatalf("reopened public identity equal=%t error=%v", bytes.Equal(createdClaim, reopenedClaim), err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	entry, err := root.OpenEntry(publicName)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := root.OpenPinnedDirectory(entry, false)
	if err != nil {
		_ = entry.Close()
		t.Fatal(err)
	}
	assertWindowsV3DirectoryProvenance(t, "pinned public reopen", pinned, false)
	if _, err := pinned.IdentityClaim(); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("pinned public reopen inherited prepared identity state: %v", err)
	}
	pinnedClaim, err := pinned.PrepareIdentityClaim()
	if err != nil || !bytes.Equal(createdClaim, pinnedClaim) {
		t.Fatalf("pinned public identity equal=%t error=%v", bytes.Equal(createdClaim, pinnedClaim), err)
	}
	if err := errors.Join(pinned.Close(), entry.Close()); err != nil {
		t.Fatal(err)
	}

	privateCandidate, err := root.CreateDirectory("private-provenance-candidate", true)
	if err != nil {
		t.Fatal(err)
	}
	assertWindowsV3DirectoryProvenance(t, "created private directory", privateCandidate, true)
	installed, err := root.InstallDirectoryNoReplace(privateCandidate, "private-provenance-installed")
	if err != nil {
		_ = privateCandidate.Close()
		t.Fatal(err)
	}
	assertWindowsV3DirectoryProvenance(t, "installed private directory", installed, true)
	if err := errors.Join(installed.Close(), privateCandidate.Close()); err != nil {
		t.Fatal(err)
	}
}

func assertWindowsV3DirectoryProvenance(
	t *testing.T,
	label string,
	directory outputV3Directory,
	wantPrivate bool,
) {
	t.Helper()
	wrapper, ok := directory.(*windowsOutputV3Directory)
	if !ok || wrapper == nil || wrapper.native == nil {
		t.Fatalf("%s type = %T", label, directory)
	}
	native := wrapper.native
	if native.private != wantPrivate {
		t.Fatalf("%s private=%t, want %t", label, native.private, wantPrivate)
	}
	if wantPrivate {
		if native.placementGuard || native.selfPlacementGuard {
			t.Fatalf("%s private provenance unexpectedly carries public placement flags", label)
		}
		return
	}
	if !native.placementGuard || !native.selfPlacementGuard {
		t.Fatalf("%s public provenance placement=%t self=%t", label, native.placementGuard, native.selfPlacementGuard)
	}
}

func TestWindowsV3ObjectIDClaimSurvivesReopenOnlyAfterPreparation(t *testing.T) {
	rootPath := windowsV3NativeTestTempDir(t)
	platform, err := openWindowsV3OutputPlatform(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := platform.acquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	first, err := guard.Root().prepareIdentityClaim()
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(guard.Close(), platform.Close()); err != nil {
		t.Fatal(err)
	}

	reopened, err := openWindowsV3OutputPlatform(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopenedGuard, err := reopened.acquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer reopenedGuard.Close()
	if _, err := reopenedGuard.Root().identityClaim(); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("reopened authority read an unprepared claim: %v", err)
	}
	recovered, err := reopenedGuard.Root().prepareIdentityClaim()
	if err != nil || !bytes.Equal(first, recovered) {
		t.Fatalf("reopened claim equal=%t error=%v", bytes.Equal(first, recovered), err)
	}
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

func TestWindowsV3GuardDetectsRootReplacementGapAndNewObjectID(t *testing.T) {
	base := windowsV3NativeTestTempDir(t)
	rootPath := filepath.Join(base, "output")
	retiredPath := filepath.Join(base, "retired-output")
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	platform, err := openWindowsV3OutputPlatform(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := platform.acquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	originalClaim, err := guard.Root().prepareIdentityClaim()
	if err != nil {
		t.Fatal(err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(rootPath, retiredPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	trap := &windowsV3ObjectIDMutationTrap{}
	platform.root.objectIDs = trap
	if replacementGuard, err := platform.acquirePublicOperationGuard(); err == nil || !errors.Is(err, errWindowsV3OutputUnsafe) {
		if replacementGuard != nil {
			_ = replacementGuard.Close()
		}
		t.Fatalf("primary authority accepted replacement root: %v", err)
	}
	if calls := trap.calls.Load(); calls != 0 {
		t.Fatalf("failed guard acquisition invoked CreateOrGet %d times", calls)
	}
	if entries, readErr := os.ReadDir(rootPath); readErr != nil || len(entries) != 0 {
		t.Fatalf("failed rebind left WindShare state/content entries=%v error=%v", entries, readErr)
	}
	if err := platform.Close(); err != nil {
		t.Fatal(err)
	}

	replacement, err := openWindowsV3OutputPlatform(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	replacementGuard, err := replacement.acquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer replacementGuard.Close()
	replacementClaim, err := replacementGuard.Root().prepareIdentityClaim()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(originalClaim, replacementClaim) {
		t.Fatal("replacement root reused the retired root Object ID claim")
	}
	if entries, readErr := os.ReadDir(rootPath); readErr != nil || len(entries) != 0 {
		t.Fatalf("replacement identity preparation left visible state/content entries=%v error=%v", entries, readErr)
	}
}
