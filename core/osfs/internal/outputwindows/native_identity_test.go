//go:build windows

package outputwindows

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"golang.org/x/sys/windows"
)

func TestWindowsV3DirectoryClaimsAndPinnedRemovalAreHandleBound(t *testing.T) {
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

	claim, err := root.identityClaim()
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := root.Duplicate()
	if err != nil {
		t.Fatal(err)
	}
	defer duplicate.Close()
	repeated, err := duplicate.identityClaim()
	if err != nil || !bytes.Equal(claim, repeated) {
		t.Fatalf("duplicate directory claim differs: equal=%t error=%v", bytes.Equal(claim, repeated), err)
	}

	regularName := "Mixed-Regular"
	regularPath := filepath.Join(root.path, regularName)
	if err := os.WriteFile(regularPath, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if kind, exact, err := root.classifyExactEntry(strings.ToLower(regularName)); err != nil || kind != outputcap.EntryRegularFile || exact {
		t.Fatalf("case-alias classification kind=%v exact=%t error=%v", kind, exact, err)
	}
	regular, err := root.openPinnedEntry(regularName)
	if err != nil {
		t.Fatal(err)
	}
	defer regular.close()
	if regular.kind != outputcap.EntryRegularFile {
		t.Fatalf("pinned regular kind=%v", regular.kind)
	}
	if err := root.removePinnedEntry(strings.ToLower(regularName), regular); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("pinned removal accepted a case alias: %v", err)
	}
	if got, err := os.ReadFile(regularPath); err != nil || string(got) != "keep" {
		t.Fatalf("case-alias rejection mutated the regular file: content=%q error=%v", got, err)
	}
	if err := root.removePinnedEntry(regularName, regular); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(regularPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pinned regular file still exists: %v", err)
	}
	raceName := "Replacement-Race"
	racePath := filepath.Join(root.path, raceName)
	displacedPath := filepath.Join(root.path, "displaced-original")
	if err := os.WriteFile(racePath, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	race, err := root.openPinnedEntry(raceName)
	if err != nil {
		t.Fatal(err)
	}
	defer race.close()
	if err := os.Rename(racePath, displacedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(racePath, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := root.removePinnedEntry(raceName, race); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("pinned removal accepted a replacement object: %v", err)
	}
	if got, err := os.ReadFile(racePath); err != nil || string(got) != "replacement" {
		t.Fatalf("identity rejection mutated replacement: content=%q error=%v", got, err)
	}

	directoryPath := filepath.Join(root.path, "empty-directory")
	if err := os.Mkdir(directoryPath, 0o700); err != nil {
		t.Fatal(err)
	}
	directoryEntry, err := root.openPinnedEntry("empty-directory")
	if err != nil {
		t.Fatal(err)
	}
	defer directoryEntry.close()
	if directoryEntry.kind != outputcap.EntryDirectory {
		t.Fatalf("pinned directory kind=%v", directoryEntry.kind)
	}
	openedDirectory, err := root.openPinnedDirectory(directoryEntry, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := openedDirectory.Close(); err != nil {
		t.Fatal(err)
	}
	if err := root.removePinnedEntry("empty-directory", directoryEntry); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(directoryPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pinned directory still exists: %v", err)
	}

	targetPath := filepath.Join(root.path, "target")
	if err := os.WriteFile(targetPath, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkName := "Opaque-Link"
	linkPath := filepath.Join(root.path, linkName)
	if err := os.Symlink("target", linkPath); err != nil {
		t.Skipf("Windows symbolic links are unavailable: %v", err)
	}
	opaque, err := root.openPinnedEntry(linkName)
	if err != nil {
		t.Fatal(err)
	}
	defer opaque.close()
	if opaque.kind != outputcap.EntryOther {
		t.Fatalf("pinned reparse-point kind=%v", opaque.kind)
	}
	if err := root.removePinnedEntry(strings.ToLower(linkName), opaque); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("pinned removal accepted a case alias: %v", err)
	}
	if _, err := os.Lstat(linkPath); err != nil {
		t.Fatalf("case-alias rejection removed the reparse point: %v", err)
	}
	if err := root.removePinnedEntry(linkName, opaque); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(linkPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("opaque reparse point still exists: %v", err)
	}
	if got, err := os.ReadFile(targetPath); err != nil || string(got) != "target" {
		t.Fatalf("opaque removal followed or mutated its target: content=%q error=%v", got, err)
	}
}

type mutableWindowsV3ObjectIDProvider struct {
	identity windowsV3PersistentObjectID
}

func (provider *mutableWindowsV3ObjectIDProvider) CreateOrGet(
	windows.Handle,
) (windowsV3PersistentObjectID, error) {
	return provider.identity, nil
}

func TestWindowsV3PersistentObjectIDDetectsIncarnationChangeOnSameHandle(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	first := windowsV3PersistentObjectID{1}
	second := windowsV3PersistentObjectID{2}
	provider := &mutableWindowsV3ObjectIDProvider{identity: first}
	platform.root.objectIDs = provider
	platform.root.objectIDState = newWindowsV3PersistentObjectIDState()
	guard, err := platform.acquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	root := guard.Root()
	if claim, err := root.prepareIdentityClaim(); err != nil || len(claim) == 0 {
		t.Fatalf("fix first persistent identity: claim=%x error=%v", claim, err)
	}
	provider.identity = second
	if _, err := root.prepareIdentityClaim(); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("same raw handle accepted changed persistent incarnation: %v", err)
	}
	provider.identity = windowsV3PersistentObjectID{}
	root.objectIDState = newWindowsV3PersistentObjectIDState()
	if _, err := root.prepareIdentityClaim(); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("zero persistent incarnation was accepted: %v", err)
	}
}

func TestWindowsV3UnknownDirectoryNeverReceivesPersistentObjectID(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	root := platform.Root()
	trap := &windowsV3ObjectIDMutationTrap{}
	root.objectIDs = trap
	if err := os.Mkdir(filepath.Join(root.path, "foreign"), 0o700); err != nil {
		t.Fatal(err)
	}
	entry, err := root.openPinnedEntry("foreign")
	if err != nil {
		t.Fatal(err)
	}
	defer entry.close()
	foreign, err := root.openPinnedDirectory(entry, false)
	if err != nil {
		t.Fatal(err)
	}
	if calls := trap.calls.Load(); calls != 0 {
		_ = foreign.Close()
		t.Fatalf("opening an unknown directory invoked CreateOrGet %d times", calls)
	}
	if err := foreign.Close(); err != nil {
		t.Fatal(err)
	}

	root.objectIDs = nativeWindowsV3PersistentObjectIDProvider{}
	private, err := root.CreatePrivateDirectory("private-authority")
	if err != nil {
		t.Fatal(err)
	}
	defer private.Close()
	if identity, prepared, identityErr := private.cachedPersistentObjectID(); identityErr != nil || !prepared || !identity.valid() {
		t.Fatalf("WindShare-created private directory lacks persistent identity: id=%x prepared=%t error=%v",
			identity, prepared, identityErr)
	}
}

func TestWindowsV3PinnedDescendantPreventsAncestorRename(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	guard, err := platform.acquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	root := guard.Root()
	source, err := root.CreatePrivateDirectory("pin-rename-source")
	if err != nil {
		t.Fatal(err)
	}
	file, err := source.CreatePrivateFile("child")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	pin, err := source.openPinnedEntry("child")
	if err != nil {
		t.Fatal(err)
	}
	if moved, err := root.InstallPrivateDirectoryNoReplace(source, "pin-rename-target"); err == nil {
		_ = moved.Close()
		_ = pin.close()
		t.Skip("this Windows version permits ancestor rename while a descendant is pinned")
	}
	if err := pin.close(); err != nil {
		t.Fatal(err)
	}
	moved, err := root.InstallPrivateDirectoryNoReplace(source, "pin-rename-target")
	if err != nil {
		t.Fatalf("ancestor rename remained blocked after closing descendant pin: %v", err)
	}
	defer moved.Close()
}

func TestWindowsV3AdapterAuthoritiesFailClosedAfterClose(t *testing.T) {
	platform := &windowsOutputV3Platform{}
	directory := &windowsOutputV3Directory{}
	entry := &windowsOutputV3EntryRef{}
	file := &windowsOutputV3File{}
	lock := &windowsOutputV3Lock{}

	checks := []struct {
		name string
		run  func() error
	}{
		{name: "root binding", run: func() error { _, err := platform.RootBinding(); return err }},
		{name: "feature probe", run: platform.ProbeRecoverableFeatures},
		{name: "duplicate directory", run: func() error { _, err := directory.Duplicate(); return err }},
		{name: "sync directory", run: directory.Sync},
		{name: "list names", run: func() error { _, err := directory.Names(1); return err }},
		{name: "observe entry", run: func() error { _, err := directory.ObserveEntry("x"); return err }},
		{name: "classify exact entry", run: func() error { _, _, err := directory.ClassifyExactEntry("x"); return err }},
		{name: "validate public names", run: func() error { return directory.ValidatePublicEntryNames([]string{"x"}) }},
		{name: "open entry", run: func() error { _, err := directory.OpenEntry("x"); return err }},
		{name: "match entry", run: func() error { _, err := directory.EntryMatches("x", nil); return err }},
		{name: "open pinned directory", run: func() error { _, err := directory.OpenPinnedDirectory(nil, true); return err }},
		{name: "remove entry", run: func() error { return directory.RemoveEntry("x", nil) }},
		{name: "compare directory", run: func() error { _, err := directory.SameDirectory(nil); return err }},
		{name: "set directory time", run: func() error { return directory.SetModifiedTime(catalog.ModifiedTime{}) }},
		{name: "open directory", run: func() error { _, err := directory.OpenDirectory("x", false); return err }},
		{name: "create directory", run: func() error { _, err := directory.CreateDirectory("x", true); return err }},
		{name: "install directory", run: func() error { _, err := directory.InstallDirectoryNoReplace(nil, "x"); return err }},
		{name: "remove directory", run: func() error { return directory.RemoveDirectory("x", nil) }},
		{name: "create file", run: func() error { _, err := directory.CreateFile("x", true, 0); return err }},
		{name: "open file", run: func() error { _, err := directory.OpenFile("x", true, true); return err }},
		{name: "link file", run: func() error { _, err := directory.LinkFileNoReplace(nil, "x"); return err }},
		{name: "replace file", run: func() error { return directory.ReplacePrivateFile(nil, "x") }},
		{name: "remove file", run: func() error { return directory.RemoveFile("x", nil) }},
		{name: "acquire lock", run: func() error { _, _, err := directory.AcquireLock("x", false); return err }},
		{name: "read file", run: func() error { _, err := file.ReadAt(make([]byte, 1), 0); return err }},
		{name: "write file", run: func() error { _, err := file.WriteAt([]byte{1}, 0); return err }},
		{name: "sync file", run: file.Sync},
		{name: "file size", run: func() error { _, err := file.Size(); return err }},
		{name: "set file time", run: func() error { return file.SetModifiedTime(catalog.ModifiedTime{}) }},
		{name: "match file metadata", run: func() error { _, err := file.MetadataMatches(0, catalog.ModifiedTime{}); return err }},
		{name: "compare file", run: func() error { _, err := file.SameFile(nil); return err }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.run(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
				t.Fatalf("error = %v, want fail-closed authority error", err)
			}
		})
	}

	if platform.Root() != nil || (*windowsOutputV3Platform)(nil).Root() != nil ||
		(&windowsOutputV3Platform{root: &windowsOutputV3Directory{}}).Root() != nil {
		t.Fatal("closed platform exposed a root authority")
	}
	if entry.Kind() != outputcap.EntryAbsent || (*windowsOutputV3EntryRef)(nil).Kind() != outputcap.EntryAbsent {
		t.Fatal("closed pinned entry did not collapse to absent")
	}
	if lock.File() != nil || (*windowsOutputV3Lock)(nil).File() != nil {
		t.Fatal("closed lock exposed a file authority")
	}
	if err := errors.Join(platform.Close(), directory.Close(), entry.Close(), file.Close(), lock.Close()); err != nil {
		t.Fatalf("idempotent close = %v", err)
	}
	closed := newWindowsOutputV3Lock(nil)
	if closed.File() != nil || closed.file.native != nil || !closed.file.borrowed {
		t.Fatal("empty native lock exposed a non-live file authority")
	}
	live := newWindowsOutputV3Lock(&windowsV3StableLock{file: &windowsV3File{}})
	if live.File() == nil {
		t.Fatal("live native lock did not expose its borrowed file authority")
	}
}

func openWindowsV3TestPlatform(t *testing.T) *windowsV3OutputPlatform {
	t.Helper()
	platform, err := openWindowsV3OutputPlatform(windowsV3NativeTestTempDir(t))
	if errors.Is(err, errWindowsV3OutputUnsupported) {
		t.Skipf("test volume is outside the local NTFS matrix: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	return platform
}
