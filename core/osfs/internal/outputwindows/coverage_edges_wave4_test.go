//go:build windows

package outputwindows

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"golang.org/x/sys/windows"
)

// A committed probe leftover is the one recovery shape that the normal probe
// leaves behind only when a process dies between its final namespace writes.
// Keeping this test on the real NTFS adapter exercises the same handle-bound
// identity checks and ordered removals that restart recovery uses.
func TestWindowsV3Wave4RecoveryRemovesCommittedProbeLeftover(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	root := platform.Root()
	name := windowsV3OutputProbePrefix + strings.Repeat("a", windowsV3OutputProbeRandomBytes*2)
	probe, err := root.CreatePrivateDirectory(name)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := probe.CreatePrivateFile("stage")
	if err != nil {
		_ = probe.Close()
		t.Fatal(err)
	}
	if err := stage.Truncate(1); err != nil {
		_ = stage.Close()
		_ = probe.Close()
		t.Fatal(err)
	}
	anchor, err := probe.LinkRegularFileNoReplace(stage, "anchor")
	if err != nil {
		_ = stage.Close()
		_ = probe.Close()
		t.Fatal(err)
	}
	publication, err := probe.LinkRegularFileNoReplace(anchor, "publication")
	if err != nil {
		_ = errors.Join(stage.Close(), anchor.Close(), probe.Close())
		t.Fatal(err)
	}
	record, err := probe.CreatePrivateFile("record")
	if err != nil {
		_ = errors.Join(stage.Close(), anchor.Close(), publication.Close(), probe.Close())
		t.Fatal(err)
	}
	if err := record.Truncate(1); err != nil {
		_ = errors.Join(stage.Close(), anchor.Close(), publication.Close(), record.Close(), probe.Close())
		t.Fatal(err)
	}
	installed, err := probe.CreatePrivateDirectory("installed")
	if err != nil {
		_ = errors.Join(stage.Close(), anchor.Close(), publication.Close(), record.Close(), probe.Close())
		t.Fatal(err)
	}
	candidate, err := probe.CreatePrivateDirectory("candidate")
	if err != nil {
		_ = errors.Join(stage.Close(), anchor.Close(), publication.Close(), record.Close(), installed.Close(), probe.Close())
		t.Fatal(err)
	}
	if err := errors.Join(
		stage.Close(), anchor.Close(), publication.Close(), record.Close(),
		installed.Close(), candidate.Close(), probe.Close(), root.Sync(),
	); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.recoverOutputProbeLeftovers() })
	if err := root.recoverOutputProbeLeftovers(); err != nil {
		t.Fatal(err)
	}
	kind, exact, err := root.classifyExactEntry(name)
	if err != nil {
		t.Fatal(err)
	}
	if kind != outputcap.EntryAbsent || !exact {
		t.Fatalf("recovered probe entry kind=%d exact=%t", kind, exact)
	}
}

func TestWindowsV3Wave4CapabilityWrapperMutationAndIdentityEdges(t *testing.T) {
	temp, err := os.CreateTemp(t.TempDir(), "wave4-truncate-")
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &windowsOutputV3File{native: &windowsV3File{file: temp}}
	if err := temp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := wrapped.Truncate(0); err == nil {
		t.Fatal("closed native file truncate succeeded")
	}
	_ = wrapped.Close()

	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	root := platform.Root()
	source, err := root.CreatePrivateFile("wave4-wrapper-source")
	if err != nil {
		t.Fatal(err)
	}
	facts, err := source.inspector.Inspect(source.handle())
	if err != nil {
		_ = source.Close()
		t.Fatal(err)
	}
	invalidVerification := *source
	invalidVerification.inspector = windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) {
		return windowsV3HandleFacts{}, nil
	})
	if _, err := (&windowsOutputV3File{native: &invalidVerification}).CloseRevalidationIdentity(); err == nil {
		t.Fatal("invalid native identity verification succeeded")
	}
	inspectionCalls := 0
	secondInspectionFails := *source
	secondInspectionFails.inspector = windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) {
		inspectionCalls++
		if inspectionCalls == 1 {
			return facts, nil
		}
		return windowsV3HandleFacts{}, errors.New("injected identity inspection failure")
	})
	if _, err := (&windowsOutputV3File{native: &secondInspectionFails}).CloseRevalidationIdentity(); err == nil {
		t.Fatal("identity revalidation inspection failure was swallowed")
	}

	wrappedRoot := &windowsOutputV3Directory{native: root}
	linked, err := wrappedRoot.LinkFileNoReplace(&windowsOutputV3File{native: source}, "wave4-wrapper-link")
	if err != nil {
		_ = source.Close()
		t.Fatal(err)
	}
	if err := wrappedRoot.RemoveFile("wave4-wrapper-link", linked); err != nil {
		_ = source.Close()
		t.Fatal(err)
	}
	if err := wrappedRoot.RemoveFile("wave4-wrapper-source", &windowsOutputV3File{native: source}); err != nil {
		_ = source.Close()
		t.Fatal(err)
	}
	if err := errors.Join(linked.Close(), source.Close()); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsV3Wave4FoundationAndObjectIDErrorContracts(t *testing.T) {
	if _, err := (nativeWindowsV3HandleInspector{}).Inspect(windows.InvalidHandle); err == nil {
		t.Fatal("invalid handle inspection succeeded")
	}
	if _, err := windowsV3VolumePath("::::invalid-volume-path::::"); err == nil {
		t.Fatal("invalid volume path succeeded")
	}
	if _, err := windowsV3CreateOrGetPersistentObjectID(windows.InvalidHandle); err == nil {
		t.Fatal("invalid object-ID handle succeeded")
	}

	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	root := platform.Root()
	if err := root.policy.verifyObjectPolicy(
		windows.InvalidHandle, windows.SE_FILE_OBJECT, windowsV3FileAllAccess, 0,
	); err == nil {
		t.Fatal("invalid security handle succeeded")
	}
	ordinary, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := root.policy.verifyObjectPolicy(
		windows.Handle(ordinary.Fd()), windows.SE_FILE_OBJECT, windowsV3FileAllAccess, 0,
	); err == nil {
		t.Fatal("ordinary inherited directory security envelope was accepted as private")
	}
	_ = ordinary.Close()

	rootFacts, err := root.inspector.Inspect(root.handle())
	if err != nil {
		t.Fatal(err)
	}
	noCache := *root
	noCache.objectIDState = nil
	if _, _, err := noCache.cachedPersistentObjectID(); err == nil {
		t.Fatal("missing object-ID cache succeeded")
	}
	inspectErr := errors.New("injected object-ID cache inspection failure")
	brokenCache := *root
	brokenCache.objectIDState = newWindowsV3PersistentObjectIDState()
	brokenCache.inspector = windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) {
		return windowsV3HandleFacts{}, inspectErr
	})
	if _, _, err := brokenCache.cachedPersistentObjectID(); !errors.Is(err, inspectErr) {
		t.Fatalf("cache inspection error = %v", err)
	}
	invalidCache := *root
	invalidCache.objectIDState = newWindowsV3PersistentObjectIDState()
	invalidCache.inspector = windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) {
		return windowsV3HandleFacts{}, nil
	})
	if _, _, err := invalidCache.cachedPersistentObjectID(); err == nil {
		t.Fatal("invalid cached object facts succeeded")
	}

	first := windowsV3PersistentObjectID{1}
	provider := func(inspector windowsV3HandleInspector) *windowsV3Directory {
		copy := *root
		copy.objectIDs = windowsV3ObjectIDProviderStub{createID: first}
		copy.objectIDState = newWindowsV3PersistentObjectIDState()
		copy.inspector = inspector
		return &copy
	}
	if _, err := provider(windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) {
		return rootFacts, nil
	})).createOrGetPersistentObjectID("wave4 missing authority", nil); err == nil {
		t.Fatal("missing object-ID authority callback succeeded")
	}
	if _, err := provider(windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) {
		return windowsV3HandleFacts{}, inspectErr
	})).createOrGetPersistentObjectID("wave4 first inspection", func() error { return nil }); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("first object-ID inspection error = %v", err)
	}
	if _, err := provider(windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) {
		return windowsV3HandleFacts{}, nil
	})).createOrGetPersistentObjectID("wave4 first validation", func() error { return nil }); err == nil {
		t.Fatal("invalid first object-ID facts succeeded")
	}
	sequenceCalls := 0
	if _, err := provider(windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) {
		sequenceCalls++
		if sequenceCalls == 1 {
			return rootFacts, nil
		}
		changed := rootFacts
		changed.caseSensitive = true
		return changed, nil
	})).createOrGetPersistentObjectID("wave4 after validation", func() error { return nil }); err == nil {
		t.Fatal("invalid post-creation object facts succeeded")
	}
	authorizeCalls := 0
	if _, err := provider(windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) {
		return rootFacts, nil
	})).createOrGetPersistentObjectID("wave4 second authority", func() error {
		authorizeCalls++
		if authorizeCalls == 2 {
			return errors.New("injected second authority failure")
		}
		return nil
	}); err == nil {
		t.Fatal("second object-ID authority failure was swallowed")
	}
}
