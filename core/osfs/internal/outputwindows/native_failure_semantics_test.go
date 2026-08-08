//go:build windows

package outputwindows

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"golang.org/x/sys/windows"
)

type windowsV3ObjectIDProviderStub struct {
	createID  windowsV3PersistentObjectID
	createErr error
	calls     *int
}

func (provider windowsV3ObjectIDProviderStub) CreateOrGet(
	windows.Handle,
) (windowsV3PersistentObjectID, error) {
	if provider.calls != nil {
		(*provider.calls)++
	}
	return provider.createID, provider.createErr
}

func TestWindowsV3OpenedObjectAndLeafIdentityFailuresAreExact(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	root := platform.Root()
	rootFacts, err := root.inspector.Inspect(root.handle())
	if err != nil {
		t.Fatal(err)
	}
	if err := windowsV3ValidateOpenedObject(rootFacts, root.volume, true); err != nil {
		t.Fatalf("valid root facts: %v", err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*windowsV3HandleFacts)
	}{
		{name: "filesystem", mutate: func(facts *windowsV3HandleFacts) { facts.filesystem = "ReFS" }},
		{name: "volume", mutate: func(facts *windowsV3HandleFacts) { facts.object.volume.serial++ }},
		{name: "reparse", mutate: func(facts *windowsV3HandleFacts) {
			facts.attributes |= windows.FILE_ATTRIBUTE_REPARSE_POINT
		}},
		{name: "type", mutate: func(facts *windowsV3HandleFacts) {
			facts.attributes &^= windows.FILE_ATTRIBUTE_DIRECTORY
		}},
		{name: "case-sensitive", mutate: func(facts *windowsV3HandleFacts) { facts.caseSensitive = true }},
		{name: "identity", mutate: func(facts *windowsV3HandleFacts) { facts.object.fileID = [16]byte{} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			facts := rootFacts
			test.mutate(&facts)
			if err := windowsV3ValidateOpenedObject(facts, root.volume, true); err == nil {
				t.Fatal("invalid opened-object facts were accepted")
			}
		})
	}

	const fileName = "Mixed-Identity.bin"
	file, err := root.CreatePrivateFile(fileName)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	fileFacts, err := file.inspector.Inspect(file.handle())
	if err != nil {
		t.Fatal(err)
	}
	if err := windowsV3ValidateOpenedObject(fileFacts, root.volume, false); err != nil {
		t.Fatalf("valid file facts: %v", err)
	}
	if err := windowsV3VerifyOpenedExactName(file.handle(), fileName); err != nil {
		t.Fatal(err)
	}
	if err := windowsV3VerifyOpenedExactName(file.handle(), strings.ToLower(fileName)); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("case-alias exact verification = %v", err)
	}
	if err := windowsV3VerifyOpenedLeafAuthority(file.handle(), strings.ToLower(fileName), false); err != nil {
		t.Fatalf("case-only public spelling: %v", err)
	}
	if err := windowsV3VerifyOpenedLeafAuthority(file.handle(), "different.bin", false); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("different public spelling = %v", err)
	}
	if err := windowsV3VerifyOpenedLeafAuthority(file.handle(), "bad\x00name", false); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("invalid expected leaf = %v", err)
	}
	if err := windowsV3VerifyOpenedLeafAuthority(windows.InvalidHandle, fileName, false); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("invalid opened handle = %v", err)
	}

	injected := errors.New("injected inspector failure")
	brokenFile := *file
	brokenFile.inspector = windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) {
		return windowsV3HandleFacts{}, injected
	})
	if _, err := sameWindowsV3OpenedObject(&brokenFile, file); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("file comparison inspector failure = %v", err)
	}
	if _, err := sameWindowsV3OpenedObject(nil, file); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("missing file comparison authority = %v", err)
	}
	brokenDirectory := *root
	brokenDirectory.inspector = brokenFile.inspector
	if _, err := sameWindowsV3OpenedDirectory(&brokenDirectory, root); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("directory comparison inspector failure = %v", err)
	}
	if _, err := sameWindowsV3OpenedDirectory(nil, root); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("missing directory comparison authority = %v", err)
	}

	for _, locator := range []string{"a/../b", "CON", "bad\x00name"} {
		if _, err := windowsV3OutputLocatorKey(locator); !errors.Is(err, errWindowsV3OutputUnsafe) {
			t.Fatalf("invalid locator %q = %v", locator, err)
		}
	}
	if _, err := windowsV3LinkRenameBuffer(0, root.handle(), "bad\x00name"); err == nil {
		t.Fatal("link buffer accepted an embedded NUL")
	}
	if _, _, _, err := windowsV3ReadPinnedEntryIdentity(windows.InvalidHandle, root.volume); err == nil {
		t.Fatal("invalid handle produced a pinned identity")
	}
	foreignVolume := root.volume
	foreignVolume.serial++
	if _, _, _, err := windowsV3ReadPinnedEntryIdentity(file.handle(), foreignVolume); err == nil {
		t.Fatal("file handle satisfied a different volume identity")
	}
	if _, err := windowsV3ReadHandleMetadata(windows.InvalidHandle); err == nil {
		t.Fatal("invalid handle produced file metadata")
	}
	modified, err := catalog.NewModifiedTime(1_700_000_000, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	if err := windowsV3SetHandleModifiedTime(windows.InvalidHandle, "invalid", modified); err == nil {
		t.Fatal("invalid handle accepted a modified time")
	}
}

func TestWindowsV3PersistentObjectIDProviderFailuresDoNotMutateAuthority(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	guard, err := platform.acquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	root := guard.Root()
	root.ancestryAuthority = windowsV3AncestryAuthorityVerifierFunc(func(windows.Handle) error { return nil })
	first := windowsV3PersistentObjectID{1}
	second := windowsV3PersistentObjectID{2}
	injected := errors.New("injected object-ID failure")

	clone := func(provider windowsV3PersistentObjectIDProvider, fixed windowsV3PersistentObjectID) *windowsV3Directory {
		copy := *root
		copy.objectIDs = provider
		copy.objectIDState = newWindowsV3PersistentObjectIDState()
		copy.objectIDState.identity = fixed
		return &copy
	}
	prepare := func(directory *windowsV3Directory) (windowsV3PersistentObjectID, error) {
		return directory.createOrGetPersistentObjectID(
			"test persistent identity preparation",
			directory.verifyPublicIdentityAuthority,
		)
	}
	assertUnsafe := func(label string, err error) {
		t.Helper()
		if !errors.Is(err, errWindowsV3OutputUnsafe) {
			t.Fatalf("%s = %v", label, err)
		}
	}

	if _, err := prepare(clone(nil, windowsV3PersistentObjectID{})); err == nil {
		t.Fatal("missing create provider was accepted")
	}
	if _, err := prepare(clone(
		windowsV3ObjectIDProviderStub{createErr: injected}, windowsV3PersistentObjectID{},
	)); !errors.Is(err, errWindowsV3OutputUnsupported) {
		t.Fatalf("create provider failure = %v", err)
	}
	if _, err := prepare(clone(windowsV3ObjectIDProviderStub{}, windowsV3PersistentObjectID{})); err == nil {
		t.Fatal("zero created identity was accepted")
	}
	if _, err := prepare(clone(windowsV3ObjectIDProviderStub{createID: second}, first)); err == nil {
		t.Fatal("changed created identity was accepted")
	}
	createdAuthority := clone(windowsV3ObjectIDProviderStub{createID: first}, windowsV3PersistentObjectID{})
	if identity, err := prepare(createdAuthority); err != nil || identity != first {
		t.Fatalf("created identity = %x error=%v", identity, err)
	}
	if fixed, prepared, fixedErr := createdAuthority.cachedPersistentObjectID(); fixedErr != nil || !prepared || fixed != first {
		t.Fatalf("fixed identity = %x prepared=%t error=%v", fixed, prepared, fixedErr)
	}

	brokenInspection := clone(windowsV3ObjectIDProviderStub{createID: first}, first)
	brokenInspection.inspector = windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) {
		return windowsV3HandleFacts{}, injected
	})
	_, err = brokenInspection.identityClaim()
	assertUnsafe("identity claim inspection", err)

	invalidFacts := clone(windowsV3ObjectIDProviderStub{createID: first}, first)
	invalidFacts.inspector = windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) {
		return windowsV3HandleFacts{}, nil
	})
	_, err = invalidFacts.identityClaim()
	assertUnsafe("identity claim object validation", err)

	readCalls := 0
	readOnly := clone(windowsV3ObjectIDProviderStub{createErr: injected, calls: &readCalls}, first)
	if _, err = readOnly.identityClaim(); err != nil || readCalls != 0 {
		t.Fatalf("read-only identity claim touched provider: calls=%d error=%v", readCalls, err)
	}

	duplicateFailure := *root
	duplicateFailure.inspector = brokenInspection.inspector
	if duplicate, err := duplicateFailure.Duplicate(); duplicate != nil || !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("duplicate verification failure = duplicate %v error %v", duplicate, err)
	}
	rootFacts, err := root.inspector.Inspect(root.handle())
	if err != nil {
		t.Fatal(err)
	}
	inspectionCalls := 0
	duplicateComparisonFailure := *root
	duplicateComparisonFailure.inspector = windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) {
		inspectionCalls++
		if inspectionCalls == 2 {
			return windowsV3HandleFacts{}, injected
		}
		return rootFacts, nil
	})
	if duplicate, err := duplicateComparisonFailure.Duplicate(); duplicate != nil || !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("duplicate comparison failure = duplicate %v error %v", duplicate, err)
	}
}

func TestWindowsV3NativeMutationsRejectCollisionsAndForeignAuthorities(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	root := platform.Root()

	if _, err := root.namesMatching(-1, func(string) bool { return true }); err == nil {
		t.Fatal("negative enumeration bound was accepted")
	}
	if _, err := root.namesMatching(1, nil); err == nil {
		t.Fatal("nil enumeration filter was accepted")
	}
	if _, _, err := root.openDirectoryStatus("forbidden", true, windows.FILE_CREATE); err == nil {
		t.Fatal("private directory bypassed the crash-safe creation protocol")
	}
	if _, err := root.openPinnedEntryForAccess("bad\x00name", windows.FILE_READ_ATTRIBUTES); err == nil {
		t.Fatal("pinned entry accepted an embedded NUL")
	}
	if err := (&windowsV3PinnedEntry{}).validate(); err == nil {
		t.Fatal("incomplete pin validated")
	}
	if err := (&windowsV3PinnedEntry{}).close(); err != nil {
		t.Fatalf("close incomplete pin: %v", err)
	}
	if _, err := root.pinnedEntryMatches("missing", nil); err == nil {
		t.Fatal("missing comparison pin was accepted")
	}
	if _, err := root.openPinnedDirectory(nil, false); err == nil {
		t.Fatal("missing directory pin was accepted")
	}
	if err := root.removePinnedEntry("missing", nil); err == nil {
		t.Fatal("missing removal pin was accepted")
	}

	closedFile := &windowsV3File{}
	if _, err := closedFile.ReadAt(make([]byte, 1), 0); err == nil {
		t.Fatal("closed native file was readable")
	}
	if _, err := closedFile.WriteAt([]byte{1}, 0); err == nil {
		t.Fatal("closed native file was writable")
	}
	if err := closedFile.Truncate(0); err == nil {
		t.Fatal("closed native file was truncatable")
	}
	if err := closedFile.Sync(); err == nil {
		t.Fatal("closed native file was syncable")
	}
	if err := (*windowsV3StableLock)(nil).Close(); err != nil {
		t.Fatalf("close nil lock: %v", err)
	}
	emptyLock := &windowsV3StableLock{}
	if err := emptyLock.Close(); err != nil {
		t.Fatalf("close empty lock: %v", err)
	}
	if err := emptyLock.Close(); err != nil {
		t.Fatalf("repeat empty lock close: %v", err)
	}
	if err := windowsV3RemoveHandle(windows.InvalidHandle); err == nil {
		t.Fatal("invalid handle was removed")
	}

	sourceFile, err := root.CreatePrivateFile("source.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer sourceFile.Close()
	if collided, err := root.CreatePrivateFile("source.bin"); collided != nil || !errors.Is(err, errWindowsV3OutputCollision) {
		t.Fatalf("file collision = file %v error %v", collided, err)
	}
	sourceDirectory, err := root.CreatePrivateDirectory("source-directory")
	if err != nil {
		t.Fatal(err)
	}
	defer sourceDirectory.Close()
	if collided, err := root.CreatePrivateDirectory("source-directory"); collided != nil || !errors.Is(err, errWindowsV3OutputCollision) {
		t.Fatalf("directory collision = directory %v error %v", collided, err)
	}

	foreignFile := *sourceFile
	foreignFile.volume.serial++
	foreignDirectory := *sourceDirectory
	foreignDirectory.volume.serial++
	for _, test := range []struct {
		name string
		run  func() error
	}{
		{name: "missing link source", run: func() error { _, err := root.LinkRegularFileNoReplace(nil, "link"); return err }},
		{name: "foreign link source", run: func() error { _, err := root.LinkRegularFileNoReplace(&foreignFile, "link"); return err }},
		{name: "invalid link target", run: func() error { _, err := root.LinkRegularFileNoReplace(sourceFile, "bad\x00link"); return err }},
		{name: "missing replacement source", run: func() error { return root.AtomicReplacePrivateFile(nil, "state") }},
		{name: "foreign replacement source", run: func() error { return root.AtomicReplacePrivateFile(&foreignFile, "state") }},
		{name: "invalid replacement target", run: func() error { return root.AtomicReplacePrivateFile(sourceFile, "bad\x00state") }},
		{name: "missing install source", run: func() error { _, err := root.InstallPrivateDirectoryNoReplace(nil, "installed"); return err }},
		{name: "foreign install source", run: func() error {
			_, err := root.InstallPrivateDirectoryNoReplace(&foreignDirectory, "installed")
			return err
		}},
		{name: "invalid install target", run: func() error {
			_, err := root.InstallPrivateDirectoryNoReplace(sourceDirectory, "bad\x00directory")
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); !errors.Is(err, errWindowsV3OutputUnsafe) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if err := root.RemoveRegularLink("absent-file", nil); err != nil {
		t.Fatalf("remove absent file: %v", err)
	}
	if err := root.RemoveDirectory("absent-directory", nil); err != nil {
		t.Fatalf("remove absent directory: %v", err)
	}

	pinnedFile, err := root.CreatePrivateFile("pinned-then-unlinked.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer pinnedFile.Close()
	pinned, err := root.openPinnedEntry("pinned-then-unlinked.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.close()
	if err := root.RemoveRegularLink("pinned-then-unlinked.bin", pinnedFile); err != nil {
		t.Fatal(err)
	}
	if matches, err := root.pinnedEntryMatches("pinned-then-unlinked.bin", pinned); err != nil || matches {
		t.Fatalf("unlinked pinned entry match = %t, %v", matches, err)
	}
	if err := root.removePinnedEntry("pinned-then-unlinked.bin", pinned); err != nil {
		t.Fatalf("remove already-unlinked pin: %v", err)
	}
	changedPin := *pinned
	changedPin.kind = outputcap.EntryDirectory
	if err := changedPin.validate(); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("changed pinned type = %v", err)
	}

	firstFile, err := root.CreatePrivateFile("first.bin")
	if err != nil {
		t.Fatal(err)
	}
	secondFile, err := root.CreatePrivateFile("second.bin")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.RemoveRegularLink("first.bin", secondFile); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("remove file through wrong identity = %v", err)
	}
	if err := root.RemoveRegularLink("first.bin", firstFile); err != nil {
		t.Fatal(err)
	}
	if err := root.RemoveRegularLink("second.bin", secondFile); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(firstFile.Close(), secondFile.Close()); err != nil {
		t.Fatal(err)
	}

	firstDirectory, err := root.CreatePrivateDirectory("first-directory")
	if err != nil {
		t.Fatal(err)
	}
	secondDirectory, err := root.CreatePrivateDirectory("second-directory")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.RemoveDirectory("first-directory", secondDirectory); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("remove directory through wrong identity = %v", err)
	}
	if err := root.RemoveDirectory("first-directory", firstDirectory); err != nil {
		t.Fatal(err)
	}
	if err := root.RemoveDirectory("second-directory", secondDirectory); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(firstDirectory.Close(), secondDirectory.Close()); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsV3ProbeCleanupRejectsMismatchedNativeIdentities(t *testing.T) {
	if err := (*windowsV3OutputProbe)(nil).cleanup(); err != nil {
		t.Fatalf("cleanup nil probe: %v", err)
	}
	if err := (&windowsV3OutputProbe{}).cleanup(); err != nil {
		t.Fatalf("cleanup incomplete probe: %v", err)
	}
	if err := (*windowsV3OutputProbeLeftover)(nil).close(); err != nil {
		t.Fatalf("close nil leftover: %v", err)
	}

	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	root := platform.Root()
	const probeName = "cleanup-probe"
	probeDirectory, err := root.CreatePrivateDirectory(probeName)
	if err != nil {
		t.Fatal(err)
	}
	defer probeDirectory.Close()
	stage, err := probeDirectory.CreatePrivateFile("stage")
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	otherFile, err := probeDirectory.CreatePrivateFile("other")
	if err != nil {
		t.Fatal(err)
	}
	defer otherFile.Close()
	expectedDirectory, err := probeDirectory.CreatePrivateDirectory("candidate")
	if err != nil {
		t.Fatal(err)
	}
	defer expectedDirectory.Close()
	otherDirectory, err := probeDirectory.CreatePrivateDirectory("other-directory")
	if err != nil {
		t.Fatal(err)
	}
	defer otherDirectory.Close()

	probe := &windowsV3OutputProbe{directory: probeDirectory}
	if err := probe.cleanupEntries(); err != nil {
		t.Fatalf("cleanup absent entries: %v", err)
	}
	if err := probe.removeKnownRegular("absent", otherFile); err != nil {
		t.Fatalf("remove absent regular entry: %v", err)
	}
	if err := probe.removeKnownRegular("stage", nil); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("remove regular entry without identity = %v", err)
	}
	if err := probe.removeKnownRegular("stage", otherFile); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("remove regular entry through different identity = %v", err)
	}
	if err := probe.removeKnownDirectory("absent-directory", otherDirectory); err != nil {
		t.Fatalf("remove absent directory: %v", err)
	}
	if err := probe.removeKnownDirectory("candidate", nil); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("remove directory without identity = %v", err)
	}
	if err := probe.removeKnownDirectory("candidate", otherDirectory); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("remove directory through different identity = %v", err)
	}

	if err := probeDirectory.RemoveRegularLink("stage", stage); err != nil {
		t.Fatal(err)
	}
	if err := probeDirectory.RemoveRegularLink("other", otherFile); err != nil {
		t.Fatal(err)
	}
	if err := probeDirectory.RemoveDirectory("candidate", expectedDirectory); err != nil {
		t.Fatal(err)
	}
	if err := probeDirectory.RemoveDirectory("other-directory", otherDirectory); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(stage.Close(), otherFile.Close(), expectedDirectory.Close(), otherDirectory.Close()); err != nil {
		t.Fatal(err)
	}
	if err := root.RemoveDirectory(probeName, probeDirectory); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsV3PlatformAdmissionAndClosedAuthoritiesFailExact(t *testing.T) {
	nativeInspector := nativeWindowsV3HandleInspector{}
	if platform, err := openWindowsV3OutputPlatformWithInspector("", nativeInspector); platform != nil || err == nil {
		t.Fatalf("empty root admission = platform %v error %v", platform, err)
	}
	if platform, err := openWindowsV3OutputPlatformWithInspector(t.TempDir(), nil); platform != nil || err == nil {
		t.Fatalf("missing inspector admission = platform %v error %v", platform, err)
	}

	injected := errors.New("injected root inspection failure")
	if platform, err := openWindowsV3OutputPlatformWithInspector(
		t.TempDir(),
		windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) {
			return windowsV3HandleFacts{}, injected
		}),
	); platform != nil || err == nil {
		t.Fatalf("failed root inspection = platform %v error %v", platform, err)
	}
	if platform, err := openWindowsV3OutputPlatformWithInspector(
		t.TempDir(),
		windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) {
			return windowsV3HandleFacts{}, nil
		}),
	); platform != nil || err == nil {
		t.Fatalf("invalid root facts = platform %v error %v", platform, err)
	}

	var nilPlatform *windowsV3OutputPlatform
	if durability := nilPlatform.Durability(); durability != 0 {
		t.Fatalf("nil platform durability = %v", durability)
	}
	if root := nilPlatform.Root(); root != nil {
		t.Fatalf("nil platform root = %v", root)
	}
	if err := nilPlatform.Close(); err != nil {
		t.Fatalf("close nil platform: %v", err)
	}
	if err := (&windowsV3OutputPlatform{}).Close(); err != nil {
		t.Fatalf("close empty platform: %v", err)
	}
	if err := (*windowsV3Directory)(nil).Close(); err != nil {
		t.Fatalf("close nil directory: %v", err)
	}
	if err := (*windowsV3File)(nil).Close(); err != nil {
		t.Fatalf("close nil file: %v", err)
	}
}

func TestWindowsV3ProbeLeftoverRejectsAmbiguousNativeCuts(t *testing.T) {
	for index, test := range []struct {
		name  string
		build func(*testing.T, *windowsV3Directory)
	}{
		{name: "unexpected entry", build: func(t *testing.T, probe *windowsV3Directory) {
			createWindowsV3ProbeTestFile(t, probe, "unexpected", nil)
		}},
		{name: "file where directory is required", build: func(t *testing.T, probe *windowsV3Directory) {
			createWindowsV3ProbeTestFile(t, probe, "candidate", nil)
		}},
		{name: "directory where file is required", build: func(t *testing.T, probe *windowsV3Directory) {
			directory, err := probe.CreatePrivateDirectory("stage")
			if err != nil {
				t.Fatal(err)
			}
			if err := directory.Close(); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "nonempty candidate", build: func(t *testing.T, probe *windowsV3Directory) {
			candidate, err := probe.CreatePrivateDirectory("candidate")
			if err != nil {
				t.Fatal(err)
			}
			createWindowsV3ProbeTestFile(t, candidate, "child", nil)
			if err := candidate.Close(); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "recognized directory before unexpected entry", build: func(t *testing.T, probe *windowsV3Directory) {
			candidate, err := probe.CreatePrivateDirectory("candidate")
			if err != nil {
				t.Fatal(err)
			}
			if err := candidate.Close(); err != nil {
				t.Fatal(err)
			}
			createWindowsV3ProbeTestFile(t, probe, "unexpected", nil)
		}},
		{name: "mismatched data identities", build: func(t *testing.T, probe *windowsV3Directory) {
			createWindowsV3ProbeTestFile(t, probe, "stage", []byte{1})
			createWindowsV3ProbeTestFile(t, probe, "anchor", []byte{1})
		}},
		{name: "unreachable publication", build: func(t *testing.T, probe *windowsV3Directory) {
			createWindowsV3ProbeTestFile(t, probe, "publication", []byte{1})
		}},
		{name: "oversized stage", build: func(t *testing.T, probe *windowsV3Directory) {
			createWindowsV3ProbeTestFile(t, probe, "stage", []byte{1, 2})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			platform := openWindowsV3TestPlatform(t)
			defer platform.Close()
			root := platform.Root()
			probeName := windowsV3OutputProbePrefix + fmt.Sprintf(
				"%0*x", windowsV3OutputProbeRandomBytes*2, index+1,
			)
			probe, err := root.CreatePrivateDirectory(probeName)
			if err != nil {
				t.Fatal(err)
			}
			test.build(t, probe)
			if err := probe.Close(); err != nil {
				t.Fatal(err)
			}
			leftover, err := root.inspectOutputProbeLeftover(probeName)
			if err == nil || leftover != nil {
				if leftover != nil {
					_ = leftover.close()
				}
				t.Fatalf("ambiguous probe cut = leftover %v error %v", leftover, err)
			}
		})
	}
}

func createWindowsV3ProbeTestFile(
	t *testing.T,
	directory *windowsV3Directory,
	name string,
	payload []byte,
) {
	t.Helper()
	file, err := directory.CreatePrivateFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) != 0 {
		written, writeErr := file.WriteAt(payload, 0)
		if writeErr != nil || written != len(payload) {
			_ = file.Close()
			t.Fatalf("write probe file %q = %d, %v", name, written, writeErr)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
