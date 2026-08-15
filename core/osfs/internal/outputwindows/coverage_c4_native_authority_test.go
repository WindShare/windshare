//go:build windows

package outputwindows

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"golang.org/x/sys/windows"
)

const (
	coverageC4PublicFileName      = "Long Public Artifact.txt"
	coverageC4PublicDirectoryName = "Selected Folder"
	coverageC4StageName           = "artifact.stage"
	coverageC4FinalName           = "artifact.bin"
	coverageC4LockDirectoryName   = "checkpoint-control"
	coverageC4LockName            = "intent.lock"
	coverageC4EmptyDirectoryName  = "empty-private-directory"
)

func TestCoverageC4WindowsBatchNameAuthorityUsesOnePinnedEnumeration(t *testing.T) {
	rootPath := windowsV3NativeTestTempDir(t)
	platform, err := Open(rootPath, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := platform.Close(); err != nil {
			t.Errorf("close platform: %v", err)
		}
	})
	root := platform.Root()

	file, err := root.CreateFile(coverageC4PublicFileName, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	directory, err := root.CreateDirectory(coverageC4PublicDirectoryName, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}

	validator, ok := root.(outputcap.PublicEntryNamesValidator)
	if !ok {
		t.Fatalf("Windows root does not expose batch name authority: %T", root)
	}
	// A case-equivalent duplicate must share the same authority record. Counting
	// it twice would manufacture ambiguity that is not present in the directory.
	if err := validator.ValidatePublicEntryNames([]string{
		coverageC4PublicFileName,
		strings.ToUpper(coverageC4PublicFileName),
		coverageC4PublicDirectoryName,
		"Missing Artifact.bin",
	}); err != nil {
		t.Fatalf("validate public names: %v", err)
	}
	if err := validator.ValidatePublicEntryNames([]string{"../escape"}); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("unsafe public name error = %v", err)
	}

	names, err := root.Names(2)
	if err != nil {
		t.Fatal(err)
	}
	wantNames := []string{coverageC4PublicFileName, coverageC4PublicDirectoryName}
	if !slices.Equal(names, wantNames) {
		t.Fatalf("enumerated names = %q, want %q", names, wantNames)
	}
	if _, err := root.Names(1); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("bounded enumeration error = %v", err)
	}
	if kind, err := root.ObserveEntry(coverageC4PublicFileName); err != nil || kind != outputcap.EntryRegularFile {
		t.Fatalf("observed file = %v, %v", kind, err)
	}
	if _, err := root.ObserveEntry("../escape"); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("unsafe observation error = %v", err)
	}
	if kind, exact, err := root.ClassifyExactEntry(coverageC4PublicDirectoryName); err != nil ||
		kind != outputcap.EntryDirectory || !exact {
		t.Fatalf("classified directory = %v, exact=%t, %v", kind, exact, err)
	}
	pinnedDirectory, err := root.OpenEntry(coverageC4PublicDirectoryName)
	if err != nil {
		t.Fatal(err)
	}
	openedDirectory, err := root.OpenPinnedDirectory(pinnedDirectory, false)
	if err != nil {
		_ = pinnedDirectory.Close()
		t.Fatal(err)
	}
	if childNames, err := openedDirectory.Names(0); err != nil || len(childNames) != 0 {
		_ = openedDirectory.Close()
		_ = pinnedDirectory.Close()
		t.Fatalf("pinned directory entries=%v error=%v", childNames, err)
	}
	if err := errors.Join(openedDirectory.Close(), pinnedDirectory.Close(), root.Sync()); err != nil {
		t.Fatal(err)
	}
	pinnedFile, err := root.OpenEntry(coverageC4PublicFileName)
	if err != nil {
		t.Fatal(err)
	}
	if opened, err := root.OpenPinnedDirectory(pinnedFile, false); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		if opened != nil {
			_ = opened.Close()
		}
		_ = pinnedFile.Close()
		t.Fatalf("file accepted as pinned directory: %v", err)
	}
	if err := pinnedFile.Close(); err != nil {
		t.Fatal(err)
	}
	if opened, err := root.OpenDirectory("missing-directory", false); !errors.Is(err, fs.ErrNotExist) {
		if opened != nil {
			_ = opened.Close()
		}
		t.Fatalf("missing directory error = %v", err)
	}
	if created, err := root.CreateDirectory(coverageC4PublicDirectoryName, false); !errors.Is(err, outputcap.ErrNamespaceCollision) {
		if created != nil {
			_ = created.Close()
		}
		t.Fatalf("duplicate directory error = %v", err)
	}
}

func TestCoverageC4WindowsRootBindingSurvivesRestartWithoutCreationInference(t *testing.T) {
	rootPath := windowsV3NativeTestTempDir(t)
	first, err := Open(rootPath, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := first.RootOpenDisposition(); got != outputcap.CallerProvidedContainer {
		_ = first.Close()
		t.Fatalf("existing root disposition = %q", got)
	}
	firstBinding, err := first.RootBinding()
	if err != nil {
		_ = first.Close()
		t.Fatal(err)
	}
	if firstBinding.IsZero() || firstBinding.Certification() != outputcap.CertificationWindowsNTFSProcessRestart {
		_ = first.Close()
		t.Fatalf("first root binding = %#v", firstBinding)
	}

	root := first.Root()
	duplicate, err := root.Duplicate()
	if err != nil {
		_ = first.Close()
		t.Fatal(err)
	}
	if same, err := root.SameDirectory(duplicate); err != nil || !same {
		_ = duplicate.Close()
		_ = first.Close()
		t.Fatalf("duplicate root identity = %t, %v", same, err)
	}
	guard, err := first.AcquirePublicOperationGuard()
	if err != nil {
		_ = duplicate.Close()
		_ = first.Close()
		t.Fatal(err)
	}
	if same, err := guard.Root().SameDirectory(root); err != nil || !same {
		_ = guard.Close()
		_ = duplicate.Close()
		_ = first.Close()
		t.Fatalf("guarded root identity = %t, %v", same, err)
	}
	if err := errors.Join(guard.Close(), duplicate.Close(), first.Close()); err != nil {
		t.Fatal(err)
	}

	restarted, err := Open(rootPath, true)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if got := restarted.RootOpenDisposition(); got != outputcap.CallerProvidedContainer {
		t.Fatalf("restart inferred root creation from existence: %q", got)
	}
	restartedBinding, err := restarted.RootBinding()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restartedBinding.Bytes(), firstBinding.Bytes()) {
		t.Fatalf("restart root binding changed: first=%s restarted=%s", firstBinding, restartedBinding)
	}
}

func TestCoverageC4WindowsRootOpenFailureDoesNotCreateAuthority(t *testing.T) {
	base := windowsV3NativeTestTempDir(t)
	missing := filepath.Join(base, "missing-root")
	if platform, err := Open(missing, false); platform != nil || !errors.Is(err, fs.ErrNotExist) {
		if platform != nil {
			_ = platform.Close()
		}
		t.Fatalf("missing output root = platform %T, error %v", platform, err)
	}
	if platform, err := OpenPrivatePublicationRoot(missing, false); platform != nil || !errors.Is(err, fs.ErrNotExist) {
		if platform != nil {
			_ = platform.Close()
		}
		t.Fatalf("missing private root = platform %T, error %v", platform, err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("non-creating opens changed the target: %v", err)
	}
	missingParentTarget := filepath.Join(base, "missing-parent", "private-root")
	if platform, err := OpenPrivatePublicationRoot(missingParentTarget, true); platform != nil || err == nil {
		if platform != nil {
			_ = platform.Close()
		}
		t.Fatalf("private root below missing parent = platform %T, error %v", platform, err)
	}
	if _, err := os.Stat(filepath.Dir(missingParentTarget)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("failed private-root create changed its missing parent: %v", err)
	}
	if platform, err := Open("relative-root", true); platform != nil ||
		!errors.Is(err, outputcap.ErrUnsafeNamespace) {
		if platform != nil {
			_ = platform.Close()
		}
		t.Fatalf("relative output root = platform %T, error %v", platform, err)
	}
	if platform, err := OpenPrivatePublicationRoot("relative-root", true); platform != nil ||
		!errors.Is(err, outputcap.ErrUnsafeNamespace) {
		if platform != nil {
			_ = platform.Close()
		}
		t.Fatalf("relative private root = platform %T, error %v", platform, err)
	}
}

func TestCoverageC4WindowsPrivatePublicationIsNoReplaceAcrossGuardedRestart(t *testing.T) {
	const payload = "restart-stable private publication"
	rootPath := filepath.Join(windowsV3NativeTestTempDir(t), "private-publication")
	platform, err := OpenPrivatePublicationRoot(rootPath, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := platform.RootOpenDisposition(); got != outputcap.AuthorityCreatedRoot {
		_ = platform.Close()
		t.Fatalf("created private root disposition = %q", got)
	}
	firstBinding, err := platform.RootBinding()
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	root := platform.Root()
	modified, err := catalog.NewModifiedTime(1_700_000_000, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	if err := root.SetModifiedTime(modified); err != nil {
		_ = platform.Close()
		t.Fatalf("set root modified time: %v", err)
	}

	stage, err := root.CreateFile(coverageC4StageName, true, int64(len(payload)))
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	if written, err := stage.WriteAt([]byte(payload), 0); err != nil || written != len(payload) {
		_ = stage.Close()
		_ = platform.Close()
		t.Fatalf("write stage = %d, %v", written, err)
	}
	if err := errors.Join(stage.SetModifiedTime(modified), stage.Sync()); err != nil {
		_ = stage.Close()
		_ = platform.Close()
		t.Fatal(err)
	}
	published, err := root.LinkFileNoReplace(stage, coverageC4FinalName)
	if err != nil {
		_ = stage.Close()
		_ = platform.Close()
		t.Fatal(err)
	}
	if same, err := stage.SameFile(published); err != nil || !same {
		_ = published.Close()
		_ = stage.Close()
		_ = platform.Close()
		t.Fatalf("published identity = %t, %v", same, err)
	}
	if unexpected, err := root.LinkFileNoReplace(stage, coverageC4FinalName); !errors.Is(err, outputcap.ErrNamespaceCollision) {
		if unexpected != nil {
			_ = unexpected.Close()
		}
		_ = published.Close()
		_ = stage.Close()
		_ = platform.Close()
		t.Fatalf("second publication error = %v", err)
	}

	guard, err := platform.AcquirePublicOperationGuard()
	if err != nil {
		_ = published.Close()
		_ = stage.Close()
		_ = platform.Close()
		t.Fatal(err)
	}
	pinned, err := guard.Root().OpenEntry(coverageC4FinalName)
	if err != nil {
		_ = guard.Close()
		_ = published.Close()
		_ = stage.Close()
		_ = platform.Close()
		t.Fatal(err)
	}
	defer pinned.Close()
	if pinned.Kind() != outputcap.EntryRegularFile {
		t.Fatalf("published entry kind = %v", pinned.Kind())
	}
	if matches, err := guard.Root().EntryMatches(coverageC4FinalName, pinned); err != nil || !matches {
		t.Fatalf("guarded publication match = %t, %v", matches, err)
	}
	if matches, err := published.MetadataMatches(uint64(len(payload)), modified); err != nil || !matches {
		t.Fatalf("published metadata = %t, %v", matches, err)
	}
	if err := errors.Join(guard.Close(), published.Close(), stage.Close(), platform.Close()); err != nil {
		t.Fatal(err)
	}

	restarted, err := OpenPrivatePublicationRoot(rootPath, false)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if got := restarted.RootOpenDisposition(); got != outputcap.CallerProvidedContainer {
		t.Fatalf("restarted private root disposition = %q", got)
	}
	restartedBinding, err := restarted.RootBinding()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restartedBinding.Bytes(), firstBinding.Bytes()) {
		t.Fatalf("private root binding changed: first=%s restarted=%s", firstBinding, restartedBinding)
	}
	restartedRoot := restarted.Root()
	restartedStage, err := restartedRoot.OpenFile(coverageC4StageName, true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer restartedStage.Close()
	restartedFinal, err := restartedRoot.OpenFile(coverageC4FinalName, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if same, err := restartedStage.SameFile(restartedFinal); err != nil || !same {
		_ = restartedFinal.Close()
		t.Fatalf("restart publication identity = %t, %v", same, err)
	}
	if matches, err := restartedRoot.EntryMatches(coverageC4FinalName, pinned); err != nil || !matches {
		_ = restartedFinal.Close()
		t.Fatalf("restart pinned match = %t, %v", matches, err)
	}

	if err := restartedRoot.RemoveFile(coverageC4FinalName, restartedFinal); err != nil {
		_ = restartedFinal.Close()
		t.Fatal(err)
	}
	if err := restartedFinal.Close(); err != nil {
		t.Fatal(err)
	}
	foreign, err := restartedRoot.CreateFile(coverageC4FinalName, false, int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	if written, err := foreign.WriteAt([]byte(payload), 0); err != nil || written != len(payload) {
		t.Fatalf("write foreign replacement = %d, %v", written, err)
	}
	if err := errors.Join(foreign.SetModifiedTime(modified), foreign.Sync()); err != nil {
		t.Fatal(err)
	}
	if matches, err := restartedRoot.EntryMatches(coverageC4FinalName, pinned); err != nil || matches {
		t.Fatalf("foreign replacement match = %t, %v", matches, err)
	}
	// Reconciliation must not turn a retained witness into delete authority for
	// a same-size, same-timestamp replacement installed after restart.
	if err := restartedRoot.RemoveEntry(coverageC4FinalName, pinned); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("remove foreign replacement error = %v", err)
	}
	preserved, err := restartedRoot.OpenFile(coverageC4FinalName, false, false)
	if err != nil {
		t.Fatalf("foreign replacement was removed: %v", err)
	}
	if same, err := foreign.SameFile(preserved); err != nil || !same {
		_ = preserved.Close()
		t.Fatalf("preserved replacement identity = %t, %v", same, err)
	}
	if err := preserved.Close(); err != nil {
		t.Fatal(err)
	}

	control, err := restartedRoot.CreateDirectory(coverageC4LockDirectoryName, true)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if missingLock, _, err := control.AcquireLock("missing.lock", true); !errors.Is(err, fs.ErrNotExist) {
		if missingLock != nil {
			_ = missingLock.Close()
		}
		t.Fatalf("missing existing-only lock error = %v", err)
	}
	lock, created, err := control.AcquireLock(coverageC4LockName, false)
	if err != nil || !created || lock == nil {
		t.Fatalf("create lock = created=%t lock=%T error=%v", created, lock, err)
	}
	if lock.File() == nil {
		t.Fatal("created lock exposed no file authority")
	}
	if contender, _, err := control.AcquireLock(coverageC4LockName, false); !errors.Is(err, outputcap.ErrNamespaceLockBusy) {
		if contender != nil {
			_ = contender.Close()
		}
		_ = lock.Close()
		t.Fatalf("contending lock error = %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	reopenedLock, created, err := control.AcquireLock(coverageC4LockName, true)
	if err != nil || created || reopenedLock == nil {
		t.Fatalf("reopen lock = created=%t lock=%T error=%v", created, reopenedLock, err)
	}
	if reopenedLock.File() == nil {
		t.Fatal("reopened lock exposed no file authority")
	}
	if err := reopenedLock.Close(); err != nil {
		t.Fatal(err)
	}

	empty, err := restartedRoot.CreateDirectory(coverageC4EmptyDirectoryName, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := empty.Close(); err != nil {
		t.Fatal(err)
	}
	reopenedEmpty, err := restartedRoot.OpenDirectory(coverageC4EmptyDirectoryName, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := restartedRoot.RemoveDirectory(coverageC4EmptyDirectoryName, reopenedEmpty); err != nil {
		_ = reopenedEmpty.Close()
		t.Fatal(err)
	}
	if err := reopenedEmpty.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCoverageC4WindowsMetadataAuthorityMapsNativeFailures(t *testing.T) {
	if _, err := windowsV3OutputLocatorKey("folder/CON"); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("reserved locator error = %v", err)
	}
	fractionalTick, err := catalog.NewModifiedTime(0, 1, catalog.TimePrecisionNanoseconds)
	if err != nil {
		t.Fatal(err)
	}
	beforeFiletimeEpoch, err := catalog.NewModifiedTime(
		-windowsV3UnixEpochFiletimeSeconds-1, 0, catalog.TimePrecisionSeconds,
	)
	if err != nil {
		t.Fatal(err)
	}
	beyondFiletimeRange, err := catalog.NewModifiedTime(
		catalog.MaxSafeInteger, 0, catalog.TimePrecisionSeconds,
	)
	if err != nil {
		t.Fatal(err)
	}
	for name, modified := range map[string]catalog.ModifiedTime{
		"fractional NTFS tick":  fractionalTick,
		"before FILETIME epoch": beforeFiletimeEpoch,
		"beyond FILETIME range": beyondFiletimeRange,
	} {
		t.Run(name, func(t *testing.T) {
			if err := (&windowsOutputV3Platform{}).ValidateModifiedTime(modified); !errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) {
				t.Fatalf("modified-time error = %v", err)
			}
			if windowsV3ModifiedTimeMatches(0, modified) {
				t.Fatal("unrepresentable modified time matched native metadata")
			}
		})
	}

	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	file, err := platform.Root().CreatePrivateFile("metadata-authority.bin")
	if err != nil {
		t.Fatal(err)
	}
	valid, err := catalog.NewModifiedTime(1_700_000_000, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	if err := windowsV3VerifyOpenedPlacementLeafAuthority(file.handle(), "different.bin", false); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("placement mismatch error = %v", err)
	}
	if err := windowsV3VerifyOpenedPlacementLeafAuthority(file.handle(), "../escape", false); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("unsafe placement name error = %v", err)
	}
	if err := windowsV3VerifyOpenedPlacementLeafAuthority(windows.InvalidHandle, "metadata-authority.bin", false); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("closed placement handle error = %v", err)
	}
	for _, exact := range []bool{false, true} {
		if err := windowsV3VerifyOpenedLeafAuthority(file.handle(), "different.bin", exact); !errors.Is(err, errWindowsV3OutputUnsafe) {
			t.Fatalf("leaf mismatch exact=%t error=%v", exact, err)
		}
	}
	if err := windowsV3VerifyOpenedLeafAuthority(file.handle(), "../escape", true); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("unsafe leaf name error = %v", err)
	}
	if err := windowsV3VerifyOpenedLeafAuthority(windows.InvalidHandle, "metadata-authority.bin", true); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("closed leaf handle error = %v", err)
	}
	if err := windowsV3SetHandleModifiedTime(windows.InvalidHandle, "metadata-authority.bin", valid); err == nil {
		t.Fatal("closed metadata handle accepted a timestamp")
	}
	if err := windowsV3SetHandleModifiedTime(windows.InvalidHandle, "metadata-authority.bin", catalog.ModifiedTime{}); err != nil {
		t.Fatalf("absent metadata should not touch a closed handle: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.setModifiedTime(valid); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("closed file modified-time error = %v", err)
	}
	if matches, err := file.metadataMatches(0, valid); matches || !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("closed file metadata = %t, %v", matches, err)
	}

	directory, err := platform.Root().Duplicate()
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
	if err := directory.setModifiedTime(valid); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("closed directory modified-time error = %v", err)
	}
}

type coverageC4ObjectIDProvider struct{ err error }

func (provider coverageC4ObjectIDProvider) CreateOrGet(windows.Handle) (windowsV3PersistentObjectID, error) {
	return windowsV3PersistentObjectID{}, provider.err
}

func TestCoverageC4WindowsRootBindingMapsIdentityPreparationFailure(t *testing.T) {
	native := openWindowsV3TestPlatform(t)
	injected := errors.New("injected persistent identity failure")
	native.root.objectIDs = coverageC4ObjectIDProvider{err: injected}
	platform := &windowsOutputV3Platform{
		native: native,
		root:   &windowsOutputV3Directory{native: native.root},
	}
	defer platform.Close()
	if binding, err := platform.RootBinding(); !binding.IsZero() ||
		!errors.Is(err, injected) || !errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) {
		t.Fatalf("failed root binding = %v, %v", binding, err)
	}
}

func TestCoverageC4WindowsPrivateRootCleanupPreservesUnprovenTargets(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	root := platform.Root()

	if err := cleanupWindowsV3PrivatePublicationRoot(nil, nil, "target", root.path, nil); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("missing cleanup parent error = %v", err)
	}
	absentPath := filepath.Join(root.path, "already-absent")
	if err := cleanupWindowsV3PrivatePublicationRoot(root, nil, "already-absent", absentPath, []byte{1}); err != nil {
		t.Fatalf("already-absent cleanup = %v", err)
	}

	exactName := "exact-created-root"
	exact, err := root.CreatePrivateDirectory(exactName)
	if err != nil {
		t.Fatal(err)
	}
	exactClaim, err := exact.preparePrivateIdentityClaim()
	if err != nil {
		_ = exact.Close()
		t.Fatal(err)
	}
	if err := exact.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cleanupWindowsV3PrivatePublicationRoot(
		root, nil, exactName, filepath.Join(root.path, exactName), exactClaim,
	); err != nil {
		t.Fatalf("exact reopened cleanup = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root.path, exactName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("exact cleanup left target: %v", err)
	}

	foreignName := "foreign-replacement-root"
	owned, err := root.CreatePrivateDirectory(foreignName)
	if err != nil {
		t.Fatal(err)
	}
	ownedClaim, err := owned.preparePrivateIdentityClaim()
	if err != nil {
		_ = owned.Close()
		t.Fatal(err)
	}
	if err := root.RemoveDirectory(foreignName, owned); err != nil {
		_ = owned.Close()
		t.Fatal(err)
	}
	if err := owned.Close(); err != nil {
		t.Fatal(err)
	}
	foreign, err := root.CreatePrivateDirectory(foreignName)
	if err != nil {
		t.Fatal(err)
	}
	if err := foreign.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cleanupWindowsV3PrivatePublicationRoot(
		root, nil, foreignName, filepath.Join(root.path, foreignName), ownedClaim,
	); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("foreign cleanup error = %v", err)
	}
	if info, err := os.Stat(filepath.Join(root.path, foreignName)); err != nil || !info.IsDir() {
		t.Fatalf("foreign cleanup changed replacement: info=%v error=%v", info, err)
	}

	wrongKindName := "root-name-now-a-file"
	wrongKind, err := root.CreatePrivateFile(wrongKindName)
	if err != nil {
		t.Fatal(err)
	}
	if err := wrongKind.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cleanupWindowsV3PrivatePublicationRoot(
		root, nil, wrongKindName, filepath.Join(root.path, wrongKindName), []byte{1},
	); err == nil {
		t.Fatal("cleanup accepted a non-directory replacement")
	}
	if info, err := os.Stat(filepath.Join(root.path, wrongKindName)); err != nil || info.IsDir() {
		t.Fatalf("wrong-kind cleanup changed replacement: info=%v error=%v", info, err)
	}
}

func TestCoverageC4WindowsRootCreationFailuresReleasePinnedAuthority(t *testing.T) {
	if retained, err := retainWindowsV3PrivatePublicationRoot(nil); retained != nil ||
		!errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("nil private-root retention = %v, %v", retained, err)
	}
	base := windowsV3NativeTestTempDir(t)
	volumeRoot := filepath.VolumeName(base) + string(filepath.Separator)
	if created, err := createWindowsV3PrivatePublicationRootWithObserver(volumeRoot, nil); created != nil ||
		!errors.Is(err, errWindowsV3OutputUnsafe) {
		if created != nil {
			_ = created.Close()
		}
		t.Fatalf("volume-root private creation = %v, %v", created, err)
	}

	injectedPlacement := errors.New("injected placement failure")
	privateTarget := filepath.Join(base, "private-placement-cut")
	privateObserver := windowsV3OutputRootCreateObserverFunc(func(_ string, cut windowsV3OutputRootCreateCut) error {
		if cut == windowsV3OutputRootCreatePlacementPinned {
			return injectedPlacement
		}
		return nil
	})
	if created, err := createWindowsV3PrivatePublicationRootWithObserver(privateTarget, privateObserver); created != nil || !errors.Is(err, injectedPlacement) {
		if created != nil {
			_ = created.Close()
		}
		t.Fatalf("private placement failure = %v, %v", created, err)
	}
	if _, err := os.Stat(privateTarget); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("private placement failure created a target: %v", err)
	}

	collisionTarget := filepath.Join(base, "preexisting-private-root")
	preexisting, err := createWindowsV3PrivatePublicationRootWithObserver(collisionTarget, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := preexisting.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(collisionTarget)
	if err != nil {
		t.Fatal(err)
	}
	if created, err := createWindowsV3PrivatePublicationRootWithObserver(collisionTarget, nil); created != nil ||
		!errors.Is(err, errWindowsV3OutputCollision) {
		if created != nil {
			_ = created.Close()
		}
		t.Fatalf("private creation collision = %v, %v", created, err)
	}
	after, err := os.Stat(collisionTarget)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("private collision changed target: same=%t error=%v", err == nil && os.SameFile(before, after), err)
	}

	publicPlacementTarget := filepath.Join(base, "public-placement", "root")
	if created, err := windowsCreateCertifiedOutputRootWithObserver(publicPlacementTarget, privateObserver); created != nil || !errors.Is(err, injectedPlacement) {
		if created != nil {
			_ = created.Close()
		}
		t.Fatalf("public placement failure = %v, %v", created, err)
	}
	if _, err := os.Stat(filepath.Dir(publicPlacementTarget)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("public placement failure created a component: %v", err)
	}

	injectedComponent := errors.New("injected component failure")
	componentTarget := filepath.Join(base, "completed-component", "unfinished-root")
	componentObserver := windowsV3OutputRootCreateObserverFunc(func(_ string, cut windowsV3OutputRootCreateCut) error {
		if cut == windowsV3OutputRootCreateComponentPinned {
			return injectedComponent
		}
		return nil
	})
	if created, err := windowsCreateCertifiedOutputRootWithObserver(componentTarget, componentObserver); created != nil || !errors.Is(err, injectedComponent) {
		if created != nil {
			_ = created.Close()
		}
		t.Fatalf("public component failure = %v, %v", created, err)
	}
	if info, err := os.Stat(filepath.Dir(componentTarget)); err != nil || !info.IsDir() {
		t.Fatalf("completed component was not retained: info=%v error=%v", info, err)
	}
	if _, err := os.Stat(componentTarget); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unfinished root was installed: %v", err)
	}

	blocker := filepath.Join(base, "regular-file-ancestor")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if ancestor, _, err := windowsV3FindCertifiedOutputAncestor(filepath.Join(blocker, "child")); ancestor != nil || err == nil {
		if ancestor != nil {
			_ = ancestor.Close()
		}
		t.Fatalf("regular-file ancestor = %v, %v", ancestor, err)
	}

	native := openWindowsV3TestPlatform(t)
	existing, err := native.Root().openDirectory("component-collision", false, windows.FILE_CREATE)
	if err != nil {
		_ = native.Close()
		t.Fatal(err)
	}
	if err := existing.Close(); err != nil {
		_ = native.Close()
		t.Fatal(err)
	}
	if current, created, err := windowsV3CreateMissingOutputComponents(
		native.Root(), []string{"component-collision"}, nil,
	); current != nil || len(created) != 0 || !errors.Is(err, errWindowsV3OutputCollision) {
		_ = native.Close()
		t.Fatalf("component collision = current=%v created=%d error=%v", current, len(created), err)
	}
	if err := native.Close(); err != nil {
		t.Fatal(err)
	}

	var nilPlatform *windowsOutputV3Platform
	if nilPlatform.RootOpenDisposition() != "" {
		t.Fatal("nil platform reported a root disposition")
	}
	incomplete := &windowsOutputV3Platform{
		native: &windowsV3OutputPlatform{root: &windowsV3Directory{}},
		root:   &windowsOutputV3Directory{native: &windowsV3Directory{}},
	}
	if guard, err := incomplete.AcquirePublicOperationGuard(); guard != nil ||
		!errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("incomplete guard = %v, %v", guard, err)
	}
	if binding, err := incomplete.RootBinding(); !binding.IsZero() ||
		!errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("incomplete binding = %v, %v", binding, err)
	}
	if err := incomplete.ProbeRecoverableFeatures(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("incomplete probe error = %v", err)
	}
	if err := incomplete.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCoverageC4WindowsEnumerationRejectsAuthorityLoss(t *testing.T) {
	for _, validate := range []bool{false, true} {
		t.Run(map[bool]string{false: "names", true: "public names"}[validate], func(t *testing.T) {
			for _, failure := range []struct {
				name string
				call int
			}{{name: "before scan", call: 1}, {name: "after scan", call: 2}} {
				t.Run(failure.name, func(t *testing.T) {
					platform := openWindowsV3TestPlatform(t)
					defer platform.Close()
					root := platform.Root()
					file, err := root.CreatePrivateFile("enumerated.bin")
					if err != nil {
						t.Fatal(err)
					}
					if err := file.Close(); err != nil {
						t.Fatal(err)
					}
					delegate := nativeWindowsV3HandleInspector{}
					injected := errors.New("injected enumeration inspection failure")
					calls := 0
					root.inspector = windowsV3HandleInspectorFunc(func(handle windows.Handle) (windowsV3HandleFacts, error) {
						calls++
						if calls == failure.call {
							return windowsV3HandleFacts{}, injected
						}
						return delegate.Inspect(handle)
					})
					if validate {
						err = root.validatePublicEntryNames([]string{"enumerated.bin"})
					} else {
						_, err = root.names(1)
					}
					if !errors.Is(err, injected) {
						t.Fatalf("enumeration authority error = %v", err)
					}
				})
			}
		})
	}

	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	withoutEnumeration := *platform.Root()
	withoutEnumeration.enumerate = nil
	if _, err := withoutEnumeration.names(1); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("missing name-enumeration authority error = %v", err)
	}
	if err := withoutEnumeration.validatePublicEntryNames([]string{"entry"}); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("missing alias-enumeration authority error = %v", err)
	}
}
