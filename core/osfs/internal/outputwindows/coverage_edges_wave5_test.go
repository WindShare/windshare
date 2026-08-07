//go:build windows

package outputwindows

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"golang.org/x/sys/windows"
)

func TestWindowsV3Wave5ProbeAdmissionAndRecoveryFailures(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	guard, err := platform.acquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	root := guard.Root()

	malformedName := windowsV3OutputProbeReservedPrefix + "-malformed"
	malformed, err := root.CreatePrivateDirectory(malformedName)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.recoverOutputProbeLeftovers(); err == nil {
		t.Fatal("malformed reserved probe name was recovered")
	}
	if err := root.RemoveDirectory(malformedName, malformed); err != nil {
		_ = malformed.Close()
		t.Fatal(err)
	}
	if err := malformed.Close(); err != nil {
		t.Fatal(err)
	}

	fileName := windowsV3OutputProbePrefix + strings.Repeat("b", windowsV3OutputProbeRandomBytes*2)
	wrongKind, err := root.CreatePrivateFile(fileName)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.recoverOutputProbeLeftovers(); err == nil {
		t.Fatal("regular file in the reserved probe namespace was recovered")
	}
	if err := root.RemoveRegularLink(fileName, wrongKind); err != nil {
		_ = wrongKind.Close()
		t.Fatal(err)
	}
	if err := wrongKind.Close(); err != nil {
		t.Fatal(err)
	}

	var nilRoot *windowsV3Directory
	if err := nilRoot.probeRecoverableFeaturesWithRandom(bytes.NewReader(make([]byte, windowsV3OutputProbeRandomBytes))); err == nil {
		t.Fatal("closed root feature probe succeeded")
	}
	if err := root.probeRecoverableFeaturesWithRandom(nil); err == nil {
		t.Fatal("feature probe without randomness succeeded")
	}
	if err := root.probeRecoverableFeaturesWithRandom(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Fatalf("feature-probe random failure = %v", err)
	}

	injected := errors.New("injected candidate preparation failure")
	root.createObserver = windowsV3PrivateDirectoryCreateObserverFunc(func(
		_ string,
		target string,
		cut windowsV3PrivateDirectoryCreateCut,
	) error {
		if target == "candidate" && cut == windowsV3PrivateDirectoryCutCreated {
			return injected
		}
		return nil
	})
	probeErr := root.probeRecoverableFeaturesWithRandom(bytes.NewReader(make([]byte, windowsV3OutputProbeRandomBytes)))
	root.createObserver = nil
	if !errors.Is(probeErr, injected) {
		t.Fatalf("injected feature-probe failure = %v", probeErr)
	}
	if err := root.recoverOutputProbeLeftovers(); err != nil {
		t.Fatalf("recover failed feature probe: %v", err)
	}
}

func TestWindowsV3Wave5ProbeLockFailureContracts(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	guard, err := platform.acquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	root := guard.Root()
	if _, err := root.prepareIdentityClaim(); err != nil {
		t.Fatal(err)
	}

	unverifiedRoot := *root
	unverifiedRoot.inspector = windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) {
		return windowsV3HandleFacts{}, errors.New("injected initial probe-lock inspection failure")
	})
	if lock, err := unverifiedRoot.acquireOutputProbeLock(); lock != nil || err == nil {
		t.Fatalf("unverified root probe lock = %v/%v", lock, err)
	}
	missingIdentity := *root
	missingIdentity.objectIDState = nil
	if lock, err := missingIdentity.acquireOutputProbeLock(); lock != nil || err == nil {
		t.Fatalf("probe lock without persistent identity = %v/%v", lock, err)
	}
	missingPolicy := *root
	missingPolicy.policy = &windowsV3PrivatePolicy{}
	if lock, err := missingPolicy.acquireOutputProbeLock(); lock != nil || err == nil {
		t.Fatalf("probe lock without private policy = %v/%v", lock, err)
	}

	rootFacts, err := root.inspector.Inspect(root.handle())
	if err != nil {
		t.Fatal(err)
	}
	inspectionCalls := 0
	changedDuringLock := *root
	changedDuringLock.inspector = windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) {
		inspectionCalls++
		if inspectionCalls >= 4 {
			return windowsV3HandleFacts{}, errors.New("injected post-lock inspection failure")
		}
		return rootFacts, nil
	})
	if lock, err := changedDuringLock.acquireOutputProbeLock(); lock != nil || err == nil {
		if lock != nil {
			_ = changedDuringLock.releaseOutputProbeLock(lock)
		}
		t.Fatalf("probe lock survived root revalidation failure = %v/%v", lock, err)
	}

	runtime.LockOSThread()
	handle, err := windows.CreateMutex(nil, false, nil)
	if err != nil {
		runtime.UnlockOSThread()
		t.Fatal(err)
	}
	notOwned := &windowsV3OutputProbeLock{
		handle: handle, held: true, threadPinned: true, threadID: windows.GetCurrentThreadId(),
	}
	if err := root.releaseOutputProbeLock(notOwned); err == nil {
		t.Fatal("unowned mutex release succeeded")
	}
}

func TestWindowsV3Wave5NativeDirectoryAndMetadataFailures(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	root := platform.Root()

	if opened, err := openWindowsV3OutputPlatformWithAuthority(
		root.path, nativeWindowsV3HandleInspector{}, 0, windowsV3DirectoryShareMode(false),
	); opened != nil || err == nil {
		t.Fatalf("zero root access authority = %v/%v", opened, err)
	}

	existing, err := root.CreatePrivateDirectory("wave5-existing")
	if err != nil {
		t.Fatal(err)
	}
	reopened, created, err := root.OpenOrCreatePrivateDirectory("wave5-existing")
	if err != nil || created {
		t.Fatalf("open existing private directory = %v/%t/%v", reopened, created, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	createdDirectory, created, err := root.OpenOrCreatePrivateDirectory("wave5-created")
	if err != nil || !created {
		t.Fatalf("create missing private directory = %v/%t/%v", createdDirectory, created, err)
	}

	missingPolicy := *root
	missingPolicy.policy = &windowsV3PrivatePolicy{}
	if opened, _, err := missingPolicy.openDirectoryStatus("wave5-existing", true, windows.FILE_OPEN); opened != nil || err == nil {
		t.Fatalf("private open without ACL policy = %v/%v", opened, err)
	}
	publicParent := *root
	publicParent.placementGuard = false
	publicParent.selfPlacementGuard = false
	publicDirectory, err := publicParent.openDirectory("wave5-public", false, windows.FILE_CREATE)
	if err != nil {
		t.Fatal(err)
	}
	if collided, err := publicParent.openDirectory("wave5-public", false, windows.FILE_CREATE); collided != nil || err == nil {
		t.Fatalf("exclusive public directory collision = %v/%v", collided, err)
	}

	rootFacts, err := root.inspector.Inspect(root.handle())
	if err != nil {
		t.Fatal(err)
	}
	brokenSync := *root
	brokenSync.inspector = windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) {
		return windowsV3HandleFacts{}, errors.New("injected sync inspection failure")
	})
	if err := brokenSync.Sync(); err == nil {
		t.Fatal("directory sync ignored revalidation failure")
	}

	file, err := root.CreatePrivateFile("wave5-metadata")
	if err != nil {
		t.Fatal(err)
	}
	fileFacts, err := file.inspector.Inspect(file.handle())
	if err != nil {
		t.Fatal(err)
	}
	brokenFile := *file
	brokenFile.inspector = windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) {
		return windowsV3HandleFacts{}, errors.New("injected file inspection failure")
	})
	if err := brokenFile.setModifiedTime(catalog.ModifiedTime{}); err == nil {
		t.Fatal("modified-time mutation ignored file revalidation failure")
	}
	if _, err := brokenFile.metadataMatches(0, catalog.ModifiedTime{}); err == nil {
		t.Fatal("metadata comparison ignored file revalidation failure")
	}

	closedOSFile, err := os.CreateTemp(t.TempDir(), "wave5-closed-file-")
	if err != nil {
		t.Fatal(err)
	}
	if err := closedOSFile.Close(); err != nil {
		t.Fatal(err)
	}
	closedNative := &windowsV3File{
		file: closedOSFile, path: closedOSFile.Name(), volume: fileFacts.object.volume,
		inspector: windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) {
			return fileFacts, nil
		}),
		policy: root.policy,
	}
	if _, err := closedNative.Size(); err == nil {
		t.Fatal("closed file exposed a size")
	}
	if _, err := closedNative.allocatedSize(); err == nil {
		t.Fatal("closed file exposed an allocation")
	}
	if _, err := closedNative.metadataMatches(0, catalog.ModifiedTime{}); err == nil {
		t.Fatal("closed file exposed metadata")
	}

	nonRepresentable, err := catalog.NewModifiedTime(1_700_000_000, 1, catalog.TimePrecisionNanoseconds)
	if err != nil {
		t.Fatal(err)
	}
	if windowsV3ModifiedTimeMatches(0, nonRepresentable) {
		t.Fatal("non-representable NTFS modified time matched")
	}
	if _, err := windowsV3OutputLocatorKey("trailing."); err == nil {
		t.Fatal("Windows-ambiguous catalog locator was accepted")
	}
	if err := windowsV3VerifyOpenedPlacementLeafAuthority(file.handle(), "different-name", false); err == nil {
		t.Fatal("different placement leaf was accepted")
	}

	if err := root.RemoveRegularLink("wave5-metadata", file); err != nil {
		t.Fatal(err)
	}
	if err := root.RemoveDirectory("wave5-existing", existing); err != nil {
		t.Fatal(err)
	}
	if err := root.RemoveDirectory("wave5-created", createdDirectory); err != nil {
		t.Fatal(err)
	}
	if err := publicParent.RemoveDirectory("wave5-public", publicDirectory); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(file.Close(), existing.Close(), createdDirectory.Close(), publicDirectory.Close()); err != nil {
		t.Fatal(err)
	}
	_ = rootFacts
}

func TestWindowsV3Wave5PinnedEntryFailureContracts(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	root := platform.Root()
	file, err := root.CreatePrivateFile("wave5-pinned-file")
	if err != nil {
		t.Fatal(err)
	}
	pinnedFile, err := root.openPinnedEntry("wave5-pinned-file")
	if err != nil {
		t.Fatal(err)
	}

	brokenDirectory := *root
	brokenDirectory.inspector = windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) {
		return windowsV3HandleFacts{}, errors.New("injected pinned-entry inspection failure")
	})
	if _, err := brokenDirectory.pinnedEntryMatches("wave5-pinned-file", pinnedFile); err == nil {
		t.Fatal("pinned comparison ignored parent revalidation failure")
	}

	closedPin, err := root.openPinnedEntry("wave5-pinned-file")
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.CloseHandle(closedPin.handle); err != nil {
		t.Fatal(err)
	}
	if err := closedPin.validate(); err == nil {
		t.Fatal("externally closed pin validated")
	}
	closedPin.handle = windows.InvalidHandle

	directory, err := root.CreatePrivateDirectory("wave5-pinned-directory")
	if err != nil {
		t.Fatal(err)
	}
	child, err := directory.CreatePrivateFile("child")
	if err != nil {
		t.Fatal(err)
	}
	pinnedDirectory, err := root.openPinnedEntry("wave5-pinned-directory")
	if err != nil {
		t.Fatal(err)
	}
	if opened, err := brokenDirectory.openPinnedDirectory(pinnedDirectory, true); opened != nil || err == nil {
		t.Fatalf("pinned directory ignored reopen verification = %v/%v", opened, err)
	}

	directoryFacts, err := directory.inspector.Inspect(directory.handle())
	if err != nil {
		t.Fatal(err)
	}
	inspectCalls := 0
	mismatchedDirectory := *root
	mismatchedDirectory.inspector = windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) {
		inspectCalls++
		if inspectCalls == 1 {
			return directoryFacts, nil
		}
		changed := directoryFacts
		changed.object.fileID[0]++
		return changed, nil
	})
	if opened, err := mismatchedDirectory.openPinnedDirectory(pinnedDirectory, true); opened != nil || err == nil {
		t.Fatalf("pinned directory incarnation mismatch = %v/%v", opened, err)
	}

	if err := errors.Join(pinnedFile.close(), pinnedDirectory.close()); err != nil {
		t.Fatal(err)
	}
	if err := directory.RemoveRegularLink("child", child); err != nil {
		t.Fatal(err)
	}
	if err := root.RemoveRegularLink("wave5-pinned-file", file); err != nil {
		t.Fatal(err)
	}
	if err := root.RemoveDirectory("wave5-pinned-directory", directory); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(child.Close(), file.Close(), directory.Close()); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsV3Wave5PostMutationVerificationFailures(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	root := platform.Root()
	injected := errors.New("injected post-mutation inspection failure")

	linkSource, err := root.CreatePrivateFile("wave5-link-source")
	if err != nil {
		t.Fatal(err)
	}
	brokenRoot := *root
	brokenRoot.inspector = windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) {
		return windowsV3HandleFacts{}, injected
	})
	if linked, err := brokenRoot.LinkRegularFileNoReplace(linkSource, "wave5-link-target"); linked != nil || err == nil {
		t.Fatalf("linked file ignored post-link inspection = %v/%v", linked, err)
	}
	if err := root.RemoveRegularLink("wave5-link-target", linkSource); err != nil {
		t.Fatal(err)
	}
	if err := root.RemoveRegularLink("wave5-link-source", linkSource); err != nil {
		t.Fatal(err)
	}
	if err := linkSource.Close(); err != nil {
		t.Fatal(err)
	}

	mismatchSource, err := root.CreatePrivateFile("wave5-link-mismatch-source")
	if err != nil {
		t.Fatal(err)
	}
	fileFacts, err := mismatchSource.inspector.Inspect(mismatchSource.handle())
	if err != nil {
		t.Fatal(err)
	}
	inspectionCalls := 0
	mismatchRoot := *root
	mismatchRoot.inspector = windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) {
		inspectionCalls++
		if inspectionCalls == 1 {
			return fileFacts, nil
		}
		changed := fileFacts
		changed.object.fileID[0]++
		return changed, nil
	})
	if linked, err := mismatchRoot.LinkRegularFileNoReplace(mismatchSource, "wave5-link-mismatch-target"); linked != nil || err == nil {
		t.Fatalf("linked file identity mismatch = %v/%v", linked, err)
	}
	if err := root.RemoveRegularLink("wave5-link-mismatch-target", mismatchSource); err != nil {
		t.Fatal(err)
	}
	if err := root.RemoveRegularLink("wave5-link-mismatch-source", mismatchSource); err != nil {
		t.Fatal(err)
	}
	if err := mismatchSource.Close(); err != nil {
		t.Fatal(err)
	}

	replacement, err := root.CreatePrivateFile("wave5-replace-source")
	if err != nil {
		t.Fatal(err)
	}
	if err := brokenRoot.AtomicReplacePrivateFile(replacement, "wave5-replace-target"); err == nil {
		t.Fatal("private replacement ignored post-rename inspection")
	}
	if err := root.RemoveRegularLink("wave5-replace-target", replacement); err != nil {
		t.Fatal(err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}

	installSource, err := root.CreatePrivateDirectory("wave5-install-source")
	if err != nil {
		t.Fatal(err)
	}
	if installed, err := brokenRoot.InstallPrivateDirectoryNoReplace(installSource, "wave5-install-target"); installed != nil || err == nil {
		t.Fatalf("private install ignored post-rename inspection = %v/%v", installed, err)
	}
	if err := root.RemoveDirectory("wave5-install-target", installSource); err != nil {
		t.Fatal(err)
	}
	if err := installSource.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsV3Wave5PlatformCreateAndWrapperContracts(t *testing.T) {
	base := windowsV3NativeTestTempDir(t)
	publicPath := filepath.Join(base, "wave5-public-root")
	publicPlatform, err := Open(publicPath, true)
	if err != nil {
		t.Fatal(err)
	}
	wrapper, ok := publicPlatform.(*windowsOutputV3Platform)
	if !ok || wrapper.Root() == nil {
		t.Fatalf("created Windows platform wrapper = %T", publicPlatform)
	}
	guard, err := wrapper.AcquirePublicOperationGuard()
	if err != nil || guard.Root() == nil {
		t.Fatalf("created wrapper guard = %v/%v", guard, err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if err := publicPlatform.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(publicPath); err != nil {
		t.Fatal(err)
	}

	privatePath := filepath.Join(base, ".wave5-private-root")
	privatePlatform, err := OpenPrivatePublicationRoot(privatePath, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := privatePlatform.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenPrivatePublicationRoot(privatePath, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(privatePath); err != nil {
		t.Fatal(err)
	}

	missingPath := filepath.Join(base, "wave5-absent-private-root")
	if opened, err := OpenPrivatePublicationRoot(missingPath, false); opened != nil || err == nil {
		t.Fatalf("absent private publication root = %v/%v", opened, err)
	}
	incomplete := &windowsOutputV3Platform{native: &windowsV3OutputPlatform{}}
	if guard, err := incomplete.AcquirePublicOperationGuard(); guard != nil || err == nil {
		t.Fatalf("incomplete wrapper guard = %v/%v", guard, err)
	}
	if err := incomplete.ProbeRecoverableFeatures(); err == nil {
		t.Fatal("incomplete wrapper feature probe succeeded")
	}
	if err := incomplete.ValidateSelectionMetadata(windowsSelectionMetadataSelection(t, nil)); err == nil {
		t.Fatal("incomplete wrapper metadata admission succeeded")
	}
}
