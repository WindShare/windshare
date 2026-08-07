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
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
	"golang.org/x/sys/windows"
)

func TestWindowsV3Wave3FoundationClassificationContracts(t *testing.T) {
	base := windowsV3HandleFacts{
		filesystem: windowsV3OutputFilesystem,
		path:       `C:\windshare-output`,
		driveType:  windows.DRIVE_FIXED,
		flags:      windows.FILE_SUPPORTS_HARD_LINKS | windows.FILE_PERSISTENT_ACLS | windowsV3FileSupportsPOSIXSemantics,
		attributes: windows.FILE_ATTRIBUTE_DIRECTORY,
		object: windowsV3ObjectIdentity{
			volume: windowsV3VolumeIdentity{guid: "volume", serial: 1},
			fileID: [16]byte{1},
		},
	}
	for name, facts := range map[string]windowsV3HandleFacts{
		"non-ntfs": func() windowsV3HandleFacts { value := base; value.filesystem = "refs"; return value }(),
		"unc": func() windowsV3HandleFacts {
			value := base
			value.path = `\\?\UNC\server\share\output`
			return value
		}(),
		"removable": func() windowsV3HandleFacts { value := base; value.driveType = windows.DRIVE_REMOVABLE; return value }(),
		"no-hard-links": func() windowsV3HandleFacts {
			value := base
			value.flags &^= windows.FILE_SUPPORTS_HARD_LINKS
			return value
		}(),
		"no-acls": func() windowsV3HandleFacts { value := base; value.flags &^= windows.FILE_PERSISTENT_ACLS; return value }(),
		"no-posix": func() windowsV3HandleFacts {
			value := base
			value.flags &^= windowsV3FileSupportsPOSIXSemantics
			return value
		}(),
	} {
		t.Run("volume/"+name, func(t *testing.T) {
			if err := validateWindowsV3VolumeCertification(facts); !errors.Is(err, errWindowsV3OutputUnsupported) {
				t.Fatalf("volume certification error = %v", err)
			}
		})
	}
	for name, facts := range map[string]windowsV3HandleFacts{
		"not-directory": func() windowsV3HandleFacts {
			value := base
			value.attributes &^= windows.FILE_ATTRIBUTE_DIRECTORY
			return value
		}(),
		"cloud": func() windowsV3HandleFacts {
			value := base
			value.attributes |= windows.FILE_ATTRIBUTE_REPARSE_POINT
			return value
		}(),
		"missing-object": func() windowsV3HandleFacts {
			value := base
			value.object.fileID = [16]byte{}
			return value
		}(),
	} {
		t.Run("shape/"+name, func(t *testing.T) {
			if err := validateWindowsV3DirectoryShape(facts, "shape", "directory"); !errors.Is(err, errWindowsV3OutputUnsupported) {
				t.Fatalf("directory shape error = %v", err)
			}
		})
	}
	caseSensitive := base
	caseSensitive.caseSensitive = true
	if err := validateWindowsV3Certification(caseSensitive); !errors.Is(err, errWindowsV3OutputUnsupported) {
		t.Fatalf("case-sensitive certification error = %v", err)
	}
	otherVolume := base
	otherVolume.object.volume.guid = "other-volume"
	if err := validateWindowsV3ExternalPlacement(otherVolume, base.object.volume); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("cross-volume placement error = %v", err)
	}

	cause := errors.New("wave3-cause")
	for _, failure := range []*windowsV3OutputError{
		{Operation: "empty", Category: nil, Cause: nil},
		{Operation: "cause", Cause: cause},
		{Operation: "category", Category: errWindowsV3OutputUnsafe},
		{Operation: "both", Path: `C:\output`, Category: errWindowsV3OutputUnsafe, Cause: cause},
	} {
		if failure.Error() == "" || failure.Unwrap() != failure.Cause || failure.Is(errWindowsV3OutputUnsupported) != errors.Is(failure.Category, errWindowsV3OutputUnsupported) {
			t.Fatalf("error projection = %q", failure.Error())
		}
	}
	if !errors.Is(windowsV3NativeOperationFailure("op", "p", windows.ERROR_NOT_SUPPORTED), errWindowsV3OutputUnsupported) {
		t.Fatal("unsupported native error was not classified")
	}
	if !errors.Is(windowsV3NativeOperationFailure("op", "p", cause), cause) {
		t.Fatal("operational cause was not retained")
	}
	if !errors.Is(windowsV3NativeNoReplaceFailure("op", "p", windows.ERROR_FILE_EXISTS), errWindowsV3OutputCollision) {
		t.Fatal("collision native error was not classified")
	}
}

func TestWindowsV3Wave3AncestryPathAndParameterContracts(t *testing.T) {
	for _, scope := range []windowsV3AncestryGuardScope{
		windowsV3GuardPublicOutputRoot,
		windowsV3GuardExternalPlacement,
		windowsV3GuardPrivateRootCreation,
	} {
		if operation, err := windowsV3AncestryGuardOperation(scope); err != nil || operation == "" {
			t.Fatalf("scope %d operation = %q, %v", scope, operation, err)
		}
	}
	if _, err := windowsV3AncestryGuardOperation(windowsV3AncestryGuardScope(99)); err == nil {
		t.Fatal("invalid ancestry scope accepted")
	}
	if windowsV3AncestryRootAccess(windowsV3GuardPrivateRootCreation) != windowsV3PrivateRootParentAccess() ||
		windowsV3AncestryRootAccess(windowsV3GuardPublicOutputRoot) != windowsV3RootDirectoryAccess() {
		t.Fatal("ancestry root access contract changed")
	}
	root, path, access, attrs := windowsV3AncestryOpenParameters(`C:\output`, 0, true, 0, false, 7)
	if root != 0 || path != `\??\C:\output` || access != 7 || attrs != windows.OBJ_CASE_INSENSITIVE|windows.OBJ_DONT_REPARSE {
		t.Fatalf("root ancestry parameters = %#x/%q/%#x/%#x", root, path, access, attrs)
	}
	root, path, _, attrs = windowsV3AncestryOpenParameters(`C:\output\child`, 1, false, 42, true, 7)
	if root != 42 || path != "child" || attrs != windows.OBJ_DONT_REPARSE {
		t.Fatalf("case-sensitive descendant parameters = %#x/%q/%#x", root, path, attrs)
	}
	for _, path := range []string{`\\server\share\output`, `relative\output`, `C:relative\output`, `\\?\Volume{123}\output`} {
		if _, err := windowsV3AbsoluteDirectoryAncestry(path); err == nil {
			t.Fatalf("unsafe ancestry %q accepted", path)
		}
	}
	if paths, err := windowsV3AbsoluteDirectoryAncestry(`C:\`); err != nil || len(paths) != 1 {
		t.Fatalf("drive-root ancestry = %v, %v", paths, err)
	}
	if _, err := windowsV3AbsoluteDirectoryAncestry(`C:\output\..\other`); err != nil {
		t.Fatalf("cleaned ancestry rejected: %v", err)
	}
	guard := &windowsV3PublicOperationGuard{pins: []*os.File{os.NewFile(1, "wave3-pin")}}
	_ = guard.Close()
	if guard.Root() != nil || guard.pins != nil {
		t.Fatal("closed ancestry guard retained authority")
	}
}

func TestWindowsV3Wave3MetadataPlanningAndExecutorGuards(t *testing.T) {
	if _, err := windowsV3MetadataPrecisionIndex(catalog.TimePrecision(99)); err == nil {
		t.Fatal("unknown metadata precision accepted")
	}
	if plan, err := windowsV3PlanSelectionMetadata(windowsSelectionMetadataSelection(t, nil)); err != nil || !plan.empty() {
		t.Fatalf("empty metadata plan = %+v, %v", plan, err)
	}
	if err := executeWindowsV3MetadataProbe(nil, windowsV3MetadataProbePlan{}, nil); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("nil metadata executor error = %v", err)
	}
	var root *windowsV3Directory
	if _, _, err := root.createMetadataProbe(nil); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("nil metadata probe random error = %v", err)
	}
	if err := (windowsV3NativeMetadataProbeExecutor{}).ProbeSize(nil, 1); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("nil size probe error = %v", err)
	}
	if err := (windowsV3NativeMetadataProbeExecutor{}).ProbeModifiedTime(nil, 1, windowsV3MetadataTimeWitness{}); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("nil time probe error = %v", err)
	}
	if _, err := duplicateWindowsV3MetadataProbe(nil); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("nil probe duplicate error = %v", err)
	}
	if err := root.closeMetadataProbeAndVerifyAbsent(windows.InvalidHandle, nil, "missing"); err == nil {
		t.Fatal("closed root metadata cleanup unexpectedly succeeded")
	}
	executor := &wave3MetadataExecutor{}
	plan := windowsV3MetadataProbePlan{
		hasSize: true, maximumSize: 8,
		times: []windowsV3MetadataTimeWitness{{modified: catalog.ModifiedTime{}, ticks: 0}},
	}
	if err := executeWindowsV3MetadataProbe(&windowsV3File{}, plan, executor); err != nil || executor.sizes != 1 || executor.times != 1 {
		t.Fatalf("metadata executor calls = %+v, %v", executor, err)
	}
}

type wave3MetadataExecutor struct {
	sizes int
	times int
}

func (executor *wave3MetadataExecutor) ProbeSize(*windowsV3File, uint64) error {
	executor.sizes++
	return nil
}

func (executor *wave3MetadataExecutor) ProbeModifiedTime(*windowsV3File, uint64, windowsV3MetadataTimeWitness) error {
	executor.times++
	return nil
}

var errWave3MetadataExecutor = errors.New("wave3 metadata executor")

func TestWindowsV3Wave3ProbeNameAndCleanupContracts(t *testing.T) {
	if _, err := newWindowsV3OutputProbeName(bytes.NewReader(nil)); err == nil {
		t.Fatal("short probe random source accepted")
	}
	if err := (&windowsV3OutputProbe{}).cleanup(); err != nil {
		t.Fatalf("empty probe cleanup = %v", err)
	}
	if err := (&windowsV3OutputProbe{root: nil}).cleanup(); err != nil {
		t.Fatalf("rootless probe cleanup = %v", err)
	}
	if err := (&windowsV3Directory{}).releaseOutputProbeLock(nil); err == nil {
		t.Fatal("nil probe lock release accepted")
	}
	if got := windowsV3CanonicalProbeName(windowsV3OutputProbePrefix + strings.Repeat("0", windowsV3OutputProbeRandomBytes*2)); !got {
		t.Fatal("canonical probe name rejected")
	}
}

func TestWindowsV3Wave3PlatformPathGuards(t *testing.T) {
	for _, run := range []func() (outputcap.Platform, error){
		func() (outputcap.Platform, error) { return Open("relative", false) },
		func() (outputcap.Platform, error) { return OpenPrivatePublicationRoot("relative", false) },
	} {
		if platform, err := run(); platform != nil || !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("relative platform path = %v, %v", platform, err)
		}
	}
	if platform, err := Open(filepath.Join(`C:\`, "windshare-wave3-missing", "root"), false); platform != nil || err == nil {
		t.Fatalf("missing platform root = %v, %v", platform, err)
	}
	if got := windowsV3ObserveOutputRootCreate(nil, "wave3", windowsV3OutputRootCreatePlacementPinned); got != nil {
		t.Fatalf("nil output-root observer = %v", got)
	}
	if got := windowsV3ObserveOutputRootCreate(windowsV3OutputRootCreateObserverFunc(func(string, windowsV3OutputRootCreateCut) error {
		return errWave3MetadataExecutor
	}), "wave3", windowsV3OutputRootCreateComponentPinned); !errors.Is(got, errWave3MetadataExecutor) {
		t.Fatalf("observer error = %v", got)
	}
	if keep, err := finishWindowsV3OutputRootCreate(nil, nil, nil); !keep || err != nil {
		t.Fatalf("successful output-root finish = %t/%v", keep, err)
	}
	if keep, err := finishWindowsV3OutputRootCreate(errWave3MetadataExecutor, nil, nil); keep || !errors.Is(err, errWave3MetadataExecutor) {
		t.Fatalf("failed output-root finish = %t/%v", keep, err)
	}
}

func TestWindowsV3Wave3NativeCapabilityGuards(t *testing.T) {
	// These nil/partial authorities are the states reached when a constructor
	// fails after allocating only part of a native handle graph.  Keeping every
	// boundary fail closed makes cleanup and retry paths deterministic instead of
	// turning an admission failure into a process panic.
	var nilDirectory *windowsV3Directory
	if _, err := nilDirectory.prepareIdentityClaim(); err == nil {
		t.Fatal("nil public identity preparation succeeded")
	}
	if _, err := nilDirectory.identityClaim(); err == nil {
		t.Fatal("nil public identity claim succeeded")
	}
	if _, err := nilDirectory.preparePrivateIdentityClaim(); err == nil {
		t.Fatal("nil private identity preparation succeeded")
	}
	if _, err := nilDirectory.privateIdentityClaim(); err == nil {
		t.Fatal("nil private identity claim succeeded")
	}
	if _, err := nilDirectory.openPinnedEntry("entry"); err == nil {
		t.Fatal("nil pinned entry open succeeded")
	}
	if _, err := nilDirectory.openPinnedEntryForAccess("entry", windows.FILE_READ_ATTRIBUTES); err == nil {
		t.Fatal("nil pinned entry access succeeded")
	}
	if _, err := nilDirectory.pinnedEntryMatches("entry", nil); err == nil {
		t.Fatal("nil pinned entry comparison succeeded")
	}
	if _, err := nilDirectory.openPinnedDirectory(nil, false); err == nil {
		t.Fatal("nil pinned directory open succeeded")
	}
	if err := nilDirectory.removePinnedEntry("entry", nil); err == nil {
		t.Fatal("nil pinned entry removal succeeded")
	}
	if _, err := nilDirectory.Duplicate(); err == nil {
		t.Fatal("nil directory duplication succeeded")
	}
	if err := nilDirectory.Sync(); err == nil {
		t.Fatal("nil directory sync succeeded")
	}
	if _, err := nilDirectory.names(1); err == nil {
		t.Fatal("nil directory enumeration succeeded")
	}
	if _, err := nilDirectory.namesWithPrefix("prefix", 1); err == nil {
		t.Fatal("nil prefix enumeration succeeded")
	}
	if _, _, err := nilDirectory.classifyExactEntry("entry"); err == nil {
		t.Fatal("nil exact-entry classification succeeded")
	}
	if _, _, err := nilDirectory.cachedPersistentObjectID(); err == nil {
		t.Fatal("nil cached identity lookup succeeded")
	}
	if err := nilDirectory.preparePersistentRootIdentity(); err == nil {
		t.Fatal("nil root identity preparation succeeded")
	}
	if err := (&windowsV3Directory{}).observePrivateDirectoryCreate("entry", windowsV3PrivateDirectoryCutCreated); err != nil {
		t.Fatalf("nil private-create observer should be a no-op: %v", err)
	}

	var nilEntry *windowsV3PinnedEntry
	if err := nilEntry.validate(); err == nil {
		t.Fatal("nil pinned entry validated")
	}
	if _, err := nilEntry.allocatedSize(); err == nil {
		t.Fatal("nil pinned entry allocation inspected")
	}
	if err := nilEntry.close(); err != nil {
		t.Fatalf("nil pinned entry close = %v", err)
	}
	if err := (&windowsV3PinnedEntry{}).validate(); err == nil {
		t.Fatal("partial pinned entry validated")
	}
	if _, err := (&windowsV3PinnedEntry{}).allocatedSize(); err == nil {
		t.Fatal("partial pinned entry allocation inspected")
	}

	var nilFile *windowsV3File
	if _, err := nilFile.ReadAt(make([]byte, 1), 0); err == nil {
		t.Fatal("nil file read succeeded")
	}
	if _, err := nilFile.WriteAt([]byte("x"), 0); err == nil {
		t.Fatal("nil file write succeeded")
	}
	if err := nilFile.Truncate(0); err == nil {
		t.Fatal("nil file truncate succeeded")
	}
	if err := nilFile.Sync(); err == nil {
		t.Fatal("nil file sync succeeded")
	}
	if err := nilFile.verify(false); err == nil {
		t.Fatal("nil file verification succeeded")
	}
	if err := (&windowsV3File{}).Close(); err != nil {
		t.Fatalf("partial file close = %v", err)
	}
	if _, err := (&windowsV3File{}).allocatedSize(); err == nil {
		t.Fatal("partial file allocation inspected")
	}

	if !stringsEqualFoldExact("Name", "Name") || !stringsEqualFoldExact("Name", "name") || stringsEqualFoldExact("Name", "other") {
		t.Fatal("Windows name equality contract changed")
	}
	if !equalFoldWindowsName("Straße", "STRASSE") && !equalFoldWindowsName("Name", "name") {
		t.Fatal("Windows case-fold helper rejected a basic equivalent")
	}

	valid := windowsV3HandleFacts{
		filesystem: windowsV3OutputFilesystem,
		object: windowsV3ObjectIdentity{
			volume: windowsV3VolumeIdentity{guid: "volume", serial: 1},
			fileID: [16]byte{1},
		},
	}
	if err := windowsV3ValidateOpenedObject(valid, valid.object.volume, false); err != nil {
		t.Fatalf("valid file facts rejected: %v", err)
	}
	if err := windowsV3ValidateOpenedObject(valid, valid.object.volume, true); err == nil {
		t.Fatal("file facts accepted as directory")
	}
	for name, facts := range map[string]windowsV3HandleFacts{
		"filesystem": func() windowsV3HandleFacts { value := valid; value.filesystem = "refs"; return value }(),
		"volume":     func() windowsV3HandleFacts { value := valid; value.object.volume.serial++; return value }(),
		"cloud": func() windowsV3HandleFacts {
			value := valid
			value.attributes = windowsV3CloudAttributeMask
			return value
		}(),
		"directory": func() windowsV3HandleFacts {
			value := valid
			value.attributes = windows.FILE_ATTRIBUTE_DIRECTORY
			return value
		}(),
		"identity": func() windowsV3HandleFacts { value := valid; value.object.fileID = [16]byte{}; return value }(),
	} {
		if err := windowsV3ValidateOpenedObject(facts, valid.object.volume, false); err == nil {
			t.Fatalf("invalid opened-object facts %q accepted", name)
		}
	}
	directoryFacts := valid
	directoryFacts.attributes = windows.FILE_ATTRIBUTE_DIRECTORY
	directoryFacts.caseSensitive = true
	if err := windowsV3ValidateOpenedObject(directoryFacts, valid.object.volume, true); err == nil {
		t.Fatal("case-sensitive directory facts accepted")
	}
	if same, err := sameWindowsV3OpenedObject(nil, nil); same || err == nil {
		t.Fatalf("nil object comparison = %t/%v", same, err)
	}
	if same, err := sameWindowsV3OpenedDirectory(nil, nil); same || err == nil {
		t.Fatalf("nil directory comparison = %t/%v", same, err)
	}

	state := newWindowsV3PersistentObjectIDState()
	if _, prepared := state.current(); prepared {
		t.Fatal("empty identity state reported prepared")
	}
	if _, prepared := (*windowsV3PersistentObjectIDState)(nil).current(); prepared {
		t.Fatal("nil identity state reported prepared")
	}
	state.identity[0] = 1
	if got, prepared := state.current(); !prepared || got != state.identity {
		t.Fatalf("prepared identity state = %v/%t", got, prepared)
	}
}

func TestWindowsV3Wave3CapabilityWrapperClosedGuards(t *testing.T) {
	// The transport-neutral wrappers are also used by deferred cleanup. Their
	// closed-state branches must remain total so a failed native constructor can
	// be safely surfaced through the public outputcap interfaces.
	var directory *windowsOutputV3Directory
	if err := directory.Close(); err != nil {
		t.Fatalf("closed directory close = %v", err)
	}
	directoryErrors := []struct {
		name string
		call func() error
	}{
		{"duplicate", func() error { _, err := directory.Duplicate(); return err }},
		{"sync", directory.Sync},
		{"names", func() error { _, err := directory.Names(1); return err }},
		{"prefix", func() error { _, err := directory.NamesWithPrefix("x", 1); return err }},
		{"observe", func() error { _, err := directory.ObserveEntry("x"); return err }},
		{"classify", func() error { _, _, err := directory.ClassifyExactEntry("x"); return err }},
		{"name", func() error { return directory.ValidatePublicEntryName("x") }},
		{"names-list", func() error { return directory.ValidatePublicEntryNames([]string{"x"}) }},
		{"open-entry", func() error { _, err := directory.OpenEntry("x"); return err }},
		{"entry-match", func() error { _, err := directory.EntryMatches("x", nil); return err }},
		{"pinned-directory", func() error { _, err := directory.OpenPinnedDirectory(nil, false); return err }},
		{"entry-remove", func() error { return directory.RemoveEntry("x", nil) }},
		{"identity", func() error { _, err := directory.IdentityClaim(); return err }},
		{"prepare-identity", func() error { _, err := directory.PrepareIdentityClaim(); return err }},
		{"prepare-private-identity", func() error { _, err := directory.PreparePrivateIdentityClaim(); return err }},
		{"private-identity", func() error { _, err := directory.PrivateIdentityClaim(); return err }},
		{"same", func() error { _, err := directory.SameDirectory(nil); return err }},
		{"modified-time", func() error { return directory.SetModifiedTime(catalog.ModifiedTime{}) }},
		{"open-directory", func() error { _, err := directory.OpenDirectory("x", false); return err }},
		{"create-directory", func() error { _, err := directory.CreateDirectory("x", false); return err }},
		{"install-directory", func() error { _, err := directory.InstallDirectoryNoReplace(nil, "x"); return err }},
		{"remove-directory", func() error { return directory.RemoveDirectory("x", nil) }},
		{"create-file", func() error { _, err := directory.CreateFile("x", false, 0); return err }},
		{"create-file-negative", func() error { _, err := directory.CreateFile("x", false, -1); return err }},
		{"open-file", func() error { _, err := directory.OpenFile("x", false, false); return err }},
		{"link-file", func() error { _, err := directory.LinkFileNoReplace(nil, "x"); return err }},
		{"replace-file", func() error { return directory.ReplacePrivateFile(nil, "x") }},
		{"remove-file", func() error { return directory.RemoveFile("x", nil) }},
		{"lock", func() error { _, _, err := directory.AcquireLock("x", false); return err }},
	}
	for _, test := range directoryErrors {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); err == nil {
				t.Fatalf("closed directory operation %s succeeded", test.name)
			}
		})
	}

	var file *windowsOutputV3File
	fileErrors := []struct {
		name string
		call func() error
	}{
		{"read", func() error { _, err := file.ReadAt(make([]byte, 1), 0); return err }},
		{"write", func() error { _, err := file.WriteAt([]byte("x"), 0); return err }},
		{"sync", file.Sync},
		{"truncate", func() error { return file.Truncate(0) }},
		{"truncate-negative", func() error { return file.Truncate(-1) }},
		{"size", func() error { _, err := file.Size(); return err }},
		{"allocation", func() error { _, err := file.AllocatedSize(); return err }},
		{"modified-time", func() error { return file.SetModifiedTime(catalog.ModifiedTime{}) }},
		{"metadata", func() error { _, err := file.MetadataMatches(0, catalog.ModifiedTime{}); return err }},
		{"same", func() error { _, err := file.SameFile(nil); return err }},
		{"revalidation", func() error { _, err := file.CloseRevalidationIdentity(); return err }},
	}
	for _, test := range fileErrors {
		t.Run("file/"+test.name, func(t *testing.T) {
			if err := test.call(); err == nil {
				t.Fatalf("closed file operation %s succeeded", test.name)
			}
		})
	}
	if err := file.Close(); err != nil {
		t.Fatalf("closed file close = %v", err)
	}

	var entry *windowsOutputV3EntryRef
	if entry.Kind() != outputcap.EntryAbsent {
		t.Fatal("closed entry reported a kind")
	}
	if _, err := entry.AllocatedSize(); err == nil {
		t.Fatal("closed entry allocation succeeded")
	}
	if err := entry.Close(); err != nil {
		t.Fatalf("closed entry close = %v", err)
	}

	var lock *windowsOutputV3Lock
	if lock.File() != nil {
		t.Fatal("closed lock exposed a file")
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("closed lock close = %v", err)
	}
	if wrapped := newWindowsOutputV3Lock(nil); wrapped == nil || wrapped.File() != nil {
		t.Fatal("nil native lock wrapper retained a file")
	}
}

type wave3FailingMetadataExecutor struct {
	failSize bool
}

func (executor wave3FailingMetadataExecutor) ProbeSize(*windowsV3File, uint64) error {
	if executor.failSize {
		return errWave3MetadataExecutor
	}
	return nil
}

func (executor wave3FailingMetadataExecutor) ProbeModifiedTime(*windowsV3File, uint64, windowsV3MetadataTimeWitness) error {
	if !executor.failSize {
		return errWave3MetadataExecutor
	}
	return nil
}

func TestWindowsV3Wave3MetadataAndAuthorityPureEdges(t *testing.T) {
	if _, err := windowsV3PlanSelectionMetadata(transfer.OutputSelection{}); err == nil {
		t.Fatal("invalid zero output selection accepted")
	}
	selection := windowsSelectionMetadataSelection(t, []windowsSelectionMetadataFile{{size: 1}})
	var nilRoot *windowsV3Directory
	if err := nilRoot.validateSelectionMetadataWithExecutor(selection, nil); err == nil {
		t.Fatal("metadata validation without an executor succeeded")
	}
	if err := executeWindowsV3MetadataProbe(&windowsV3File{}, windowsV3MetadataProbePlan{
		hasSize: true, maximumSize: 1,
	}, wave3FailingMetadataExecutor{failSize: true}); !errors.Is(err, errWave3MetadataExecutor) {
		t.Fatalf("size executor error = %v", err)
	}
	if err := executeWindowsV3MetadataProbe(&windowsV3File{}, windowsV3MetadataProbePlan{
		times: []windowsV3MetadataTimeWitness{{}},
	}, wave3FailingMetadataExecutor{}); !errors.Is(err, errWave3MetadataExecutor) {
		t.Fatalf("time executor error = %v", err)
	}

	if err := windowsV3SetHandleModifiedTime(windows.InvalidHandle, "absent", catalog.ModifiedTime{}); err != nil {
		t.Fatalf("absent modified time should be inert: %v", err)
	}
	for _, modified := range []catalog.ModifiedTime{
		windowsSelectionMetadataModified(t, 1_700_000_000, 0, catalog.TimePrecisionSeconds),
		windowsSelectionMetadataModified(t, 1_700_000_000, 123_000_000, catalog.TimePrecisionMilliseconds),
		windowsSelectionMetadataModified(t, 1_700_000_000, 123_456_700, catalog.TimePrecisionNanoseconds),
	} {
		ticks, present, err := windowsV3ModifiedTimeTicks(modified)
		if err != nil || !present || ticks == 0 || !windowsV3ModifiedTimeMatches(ticks, modified) {
			t.Fatalf("modified time conversion = %d/%t/%v", ticks, present, err)
		}
	}
	if !windowsV3ModifiedTimeMatches(0, catalog.ModifiedTime{}) {
		t.Fatal("absent modified time did not match")
	}
	if windowsV3ModifiedTimeMatches(0, windowsSelectionMetadataModified(t, 1_700_000_000, 0, catalog.TimePrecisionSeconds)) {
		t.Fatal("different modified time matched")
	}
	if _, err := windowsV3ReadHandleMetadata(windows.InvalidHandle); err == nil {
		t.Fatal("invalid metadata handle succeeded")
	}
	if _, err := windowsV3OpenedLeafNameWithFlags(windows.InvalidHandle, 0); err == nil {
		t.Fatal("invalid leaf-name handle succeeded")
	}
	for _, path := range []string{"", "./child", "child/../other", "child\\other"} {
		if _, err := windowsV3OutputLocatorKey(path); err == nil {
			t.Fatalf("non-canonical locator %q accepted", path)
		}
	}
	if match, err := windowsV3PlacementLeafNamesMatch("Name", "Name", "other", true); err != nil || !match {
		t.Fatalf("case-sensitive placement match = %t/%v", match, err)
	}
	if match, err := windowsV3PlacementLeafNamesMatch("Name", "Other", "third", true); err != nil || match {
		t.Fatalf("case-sensitive placement mismatch = %t/%v", match, err)
	}
	if match, err := windowsV3PlacementLeafNamesMatch("Name", "name", "other", false); err != nil || !match {
		t.Fatalf("case-insensitive placement match = %t/%v", match, err)
	}

	if windowsV3IsAdministratorAccount(nil) {
		t.Fatal("nil administrator SID accepted")
	}
	var nilPolicy *windowsV3PrivatePolicy
	if nilPolicy.ancestryExempts(nil) {
		t.Fatal("nil ancestry policy exempted a trustee")
	}
	if (&windowsV3PrivatePolicy{}).ancestryExempts(nil) {
		t.Fatal("empty ancestry policy exempted a trustee")
	}
	if _, err := windowsV3CertifiedAncestryDACL(nil, nil); err == nil {
		t.Fatal("missing ancestry descriptor accepted")
	}
	if err := windowsV3VerifyAncestryAuthorityDescriptor(nil, nil); err == nil {
		t.Fatal("missing ancestry authority accepted")
	}
	for _, aceType := range []uint8{
		windows.ACCESS_ALLOWED_ACE_TYPE,
		windows.ACCESS_DENIED_ACE_TYPE,
		windowsV3AccessAllowedObjectACEType,
		0xff,
	} {
		if _, err := windowsV3AncestryACEAllowsAccess(aceType); aceType != windows.ACCESS_ALLOWED_ACE_TYPE && aceType != windows.ACCESS_DENIED_ACE_TYPE && err == nil {
			t.Fatalf("unsupported ACE type %#x accepted", aceType)
		}
	}
	if err := nilRoot.verifyPublicIdentityAuthority(); err == nil {
		t.Fatal("nil public identity authority verified")
	}
	if got := windowsV3AuthorityFailureClass(errWindowsV3OutputUnsupported); got != errWindowsV3OutputUnsupported {
		t.Fatal("unsupported authority failure was remapped")
	}
	if got := windowsV3AuthorityFailureClass(errors.New("unsafe")); got != errWindowsV3OutputUnsafe {
		t.Fatal("ordinary authority failure was not unsafe")
	}

	for cut := windowsV3PrivateDirectoryCreateCut(0); cut <= windowsV3PrivateDirectoryCutClosed; cut++ {
		if cut.String() == "" {
			t.Fatalf("empty private-directory cut name for %d", cut)
		}
	}
	if got := windowsV3PrivateDirectoryCreateCut(255).String(); got != "unknown(255)" {
		t.Fatalf("unknown private-directory cut = %q", got)
	}
	installErr := errors.New("install")
	for name, args := range map[string]struct {
		observation error
		close       error
	}{
		"collision": {observation: nil},
		"unsafe":    {observation: windowsV3Failure("observe", "target", errWindowsV3OutputUnsafe, errors.New("unsafe"))},
		"native":    {observation: errors.New("observe")},
		"close":     {observation: nil, close: errors.New("close")},
	} {
		t.Run("install-denied/"+name, func(t *testing.T) {
			if err := windowsV3DirectoryInstallDeniedFailure("target", installErr, args.observation, args.close); err == nil {
				t.Fatal("denied install projection was nil")
			}
		})
	}

	leftover := &windowsV3OutputProbeLeftover{
		regular:     map[string]*windowsV3File{"stage": nil},
		directories: map[string]*windowsV3Directory{"candidate": nil},
	}
	if err := leftover.validateDataLinks(); err != nil {
		t.Fatalf("empty leftover links = %v", err)
	}
	if err := leftover.closeChildren(); err != nil || len(leftover.regular) != 0 || len(leftover.directories) != 0 {
		t.Fatalf("leftover child cleanup = %v/%d/%d", err, len(leftover.regular), len(leftover.directories))
	}
	if err := leftover.close(); err != nil {
		t.Fatalf("empty leftover close = %v", err)
	}
	if err := (&windowsV3OutputProbeLeftover{}).close(); err != nil {
		t.Fatalf("zero leftover close = %v", err)
	}
	if err := (&windowsV3Directory{}).recoverOutputProbeLeftovers(); err == nil {
		t.Fatal("closed-root probe recovery succeeded")
	}
	for _, lock := range []*windowsV3OutputProbeLock{
		{handle: 1, held: true, threadPinned: false},
		{handle: 1, held: true, threadPinned: true, threadID: 0},
	} {
		if err := (&windowsV3Directory{}).releaseOutputProbeLock(lock); err == nil {
			t.Fatal("unowned probe lock release succeeded")
		}
	}
	for _, name := range []string{"", "bad", windowsV3OutputProbePrefix + "g", windowsV3OutputProbePrefix + strings.Repeat("0", windowsV3OutputProbeRandomBytes*2-1)} {
		if windowsV3CanonicalProbeName(name) {
			t.Fatalf("malformed probe name %q accepted", name)
		}
	}
}

func TestWindowsV3Wave3PlatformWrapperClosedEdges(t *testing.T) {
	var platform *windowsOutputV3Platform
	if platform.Root() != nil {
		t.Fatal("closed platform exposed a root")
	}
	if _, err := platform.AcquirePublicOperationGuard(); err == nil {
		t.Fatal("closed platform acquired a guard")
	}
	if _, err := platform.RootBinding(); err == nil {
		t.Fatal("closed platform produced a root binding")
	}
	if err := platform.ProbeRecoverableFeatures(); err == nil {
		t.Fatal("closed platform probed recoverable features")
	}
	if err := platform.ValidateSelectionMetadata(transfer.OutputSelection{}); err == nil {
		t.Fatal("closed platform validated metadata")
	}
	if err := platform.ValidateModifiedTime(catalog.ModifiedTime{}); err != nil {
		t.Fatalf("absent platform modified time = %v", err)
	}
	if _, err := platform.CanonicalLocatorKey("../bad"); err == nil {
		t.Fatal("closed platform accepted non-canonical locator")
	}
	if _, err := platform.CanonicalComponentKey("../bad"); err == nil {
		t.Fatal("closed platform accepted unsafe component")
	}
	if got := platform.Certification(); got != resumestate.CertificationWindowsNTFSProcessRestart {
		t.Fatalf("platform certification = %v", got)
	}
	if got := platform.Durability(); got != transfer.DurabilityProcessRestart {
		t.Fatalf("platform durability = %v", got)
	}
	if err := platform.Close(); err != nil {
		t.Fatalf("closed platform close = %v", err)
	}
	if native, err := retainWindowsV3PrivatePublicationRoot(nil); native != nil || err == nil {
		t.Fatalf("nil private publication retention = %v/%v", native, err)
	}
	if err := cleanupWindowsV3PrivatePublicationRoot(nil, nil, "child", `C:\output\child`, nil); err == nil {
		t.Fatal("nil publication cleanup parent accepted")
	}
	if current, created, err := windowsV3CreateMissingOutputComponents(nil, nil, nil); current != nil || len(created) != 0 || err != nil {
		t.Fatalf("empty component creation = %v/%v/%v", current, created, err)
	}
	for _, path := range []string{"relative", `\\server\share\output`, `C:\windshare-wave3-no-root\child`} {
		if _, _, err := windowsV3FindCertifiedOutputAncestor(path); err == nil {
			t.Fatalf("uncertified ancestor %q accepted", path)
		}
	}
}

type wave3AncestryOpener struct {
	err    error
	handle windows.Handle
}

func (opener wave3AncestryOpener) Open(windows.Handle, string, uint32, uint32) (windows.Handle, uintptr, error) {
	if opener.err != nil {
		return windows.InvalidHandle, 0, opener.err
	}
	return opener.handle, 0, nil
}

func TestWindowsV3Wave3AncestryTraversalGuards(t *testing.T) {
	var nilGuard *windowsV3PublicOperationGuard
	if nilGuard.Root() != nil || nilGuard.Close() != nil {
		t.Fatal("nil ancestry guard retained state")
	}
	var nilPlatform *windowsV3OutputPlatform
	if _, err := nilPlatform.acquireDirectoryAncestryGuardWithOpener(windowsV3GuardPublicOutputRoot, wave3AncestryOpener{err: errors.New("open")}); err == nil {
		t.Fatal("nil ancestry platform accepted")
	}
	closedPlatform := &windowsV3OutputPlatform{}
	if _, err := closedPlatform.acquireDirectoryAncestryGuardWithOpener(windowsV3GuardPublicOutputRoot, wave3AncestryOpener{}); err == nil {
		t.Fatal("closed ancestry platform accepted")
	}
	if _, err := closedPlatform.acquireDirectoryAncestryGuardWithOpener(windowsV3AncestryGuardScope(99), wave3AncestryOpener{}); err == nil {
		t.Fatal("invalid ancestry scope accepted by platform")
	}
	platformWithRoot := &windowsV3OutputPlatform{root: &windowsV3Directory{}}
	if _, err := platformWithRoot.acquireDirectoryAncestryGuardWithOpener(windowsV3GuardPublicOutputRoot, nil); err == nil {
		t.Fatal("missing ancestry opener accepted")
	}

	rootVolume := windowsV3VolumeIdentity{guid: "volume", serial: 1}
	facts := windowsV3HandleFacts{
		filesystem: windowsV3OutputFilesystem,
		path:       `C:\output`,
		driveType:  windows.DRIVE_FIXED,
		flags:      windows.FILE_SUPPORTS_HARD_LINKS | windows.FILE_PERSISTENT_ACLS | windowsV3FileSupportsPOSIXSemantics,
		attributes: windows.FILE_ATTRIBUTE_DIRECTORY,
		object:     windowsV3ObjectIdentity{volume: rootVolume, fileID: [16]byte{1}},
	}
	root := &windowsV3Directory{
		path: `C:\output`, volume: rootVolume,
		inspector: windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) { return facts, nil }),
	}
	temporary, err := os.CreateTemp(t.TempDir(), "wave3-ancestry-")
	if err != nil {
		t.Fatal(err)
	}
	var syntheticHandle windows.Handle
	if err := windows.DuplicateHandle(
		windows.CurrentProcess(), windows.Handle(temporary.Fd()),
		windows.CurrentProcess(), &syntheticHandle, 0, false, windows.DUPLICATE_SAME_ACCESS,
	); err != nil {
		_ = temporary.Close()
		t.Fatal(err)
	}
	_ = temporary.Close()
	traversal := windowsV3AncestryGuardTraversal{
		operation: "wave3 ancestry", scope: windowsV3GuardExternalPlacement,
		root: root, opener: wave3AncestryOpener{handle: syntheticHandle},
		paths: []string{`C:\output`}, rootAccess: windowsV3RootDirectoryAccess(),
	}
	if file, gotFacts, err := traversal.openEntry(0, 0, false); err != nil || file == nil || gotFacts.object != facts.object {
		t.Fatalf("successful ancestry entry = %v/%v/%v", file, gotFacts, err)
	} else {
		_ = file.Close()
	}
	if _, _, err := (windowsV3AncestryGuardTraversal{
		operation: "wave3 ancestry", scope: windowsV3GuardExternalPlacement,
		root: root, opener: wave3AncestryOpener{err: errors.New("injected opener")}, paths: []string{`C:\output`},
	}).openEntry(0, 0, false); err == nil {
		t.Fatal("ancestry opener failure was swallowed")
	}
	if err := traversal.validateEntry(0, `C:\output`, 0, true, false, facts, errors.New("inspection")); err != nil {
		t.Fatalf("inspection failure should defer validation: %v", err)
	}
	if err := traversal.validateEntry(0, `C:\output`, 0, true, false, windowsV3HandleFacts{}, nil); err == nil {
		t.Fatal("invalid ancestry facts accepted")
	}

	for _, caseValue := range []struct {
		name        string
		rootEntry   bool
		scope       windowsV3AncestryGuardScope
		inspectErr  error
		validateErr error
	}{
		{name: "non-root", rootEntry: false, scope: windowsV3GuardPublicOutputRoot},
		{name: "inspection", rootEntry: true, scope: windowsV3GuardPublicOutputRoot, inspectErr: errors.New("inspect")},
		{name: "validation", rootEntry: true, scope: windowsV3GuardPublicOutputRoot, validateErr: errors.New("validate")},
	} {
		if err := traversal.verifyRootAuthority(1, caseValue.rootEntry, caseValue.inspectErr, caseValue.validateErr); err != nil {
			t.Fatalf("verify root authority %s = %v", caseValue.name, err)
		}
	}
	publicTraversal := traversal
	publicTraversal.scope = windowsV3GuardPublicOutputRoot
	if err := publicTraversal.verifyRootAuthority(1, true, nil, nil); err == nil {
		t.Fatal("missing ancestry authority verifier accepted")
	}
	publicTraversal.root.ancestryAuthority = windowsV3AncestryAuthorityVerifierFunc(func(windows.Handle) error {
		return errors.New("authority")
	})
	if err := publicTraversal.verifyRootAuthority(1, true, nil, nil); err == nil {
		t.Fatal("ancestry authority denial was swallowed")
	}
	publicTraversal.root.ancestryAuthority = windowsV3AncestryAuthorityVerifierFunc(func(windows.Handle) error { return nil })
	if err := publicTraversal.verifyRootAuthority(1, true, nil, nil); err != nil {
		t.Fatalf("successful ancestry authority verification = %v", err)
	}
}

func TestWindowsV3Wave3PartialDirectoryWrapperEdges(t *testing.T) {
	partialNative := &windowsV3Directory{policy: &windowsV3PrivatePolicy{}}
	directory := &windowsOutputV3Directory{native: partialNative}
	entry := &windowsOutputV3EntryRef{native: &windowsV3PinnedEntry{
		handle: windows.InvalidHandle, kind: outputcap.EntryDirectory,
	}}
	partialFile := &windowsOutputV3File{native: &windowsV3File{}, private: true}
	otherDirectory := &windowsOutputV3Directory{native: &windowsV3Directory{policy: &windowsV3PrivatePolicy{}}}
	operations := []struct {
		name string
		call func() error
	}{
		{"entry-match", func() error { _, err := directory.EntryMatches("entry", entry); return err }},
		{"pinned-directory", func() error { _, err := directory.OpenPinnedDirectory(entry, false); return err }},
		{"entry-remove", func() error { return directory.RemoveEntry("entry", entry) }},
		{"identity", func() error { _, err := directory.IdentityClaim(); return err }},
		{"prepare-identity", func() error { _, err := directory.PrepareIdentityClaim(); return err }},
		{"prepare-private-identity", func() error { _, err := directory.PreparePrivateIdentityClaim(); return err }},
		{"private-identity", func() error { _, err := directory.PrivateIdentityClaim(); return err }},
		{"same", func() error { _, err := directory.SameDirectory(otherDirectory); return err }},
		{"open-directory", func() error { _, err := directory.OpenDirectory("child", false); return err }},
		{"create-directory", func() error { _, err := directory.CreateDirectory("child", true); return err }},
		{"install-directory", func() error { _, err := directory.InstallDirectoryNoReplace(otherDirectory, "child"); return err }},
		{"remove-directory", func() error { return directory.RemoveDirectory("child", otherDirectory) }},
		{"open-file", func() error { _, err := directory.OpenFile("file", false, false); return err }},
		{"create-file", func() error { _, err := directory.CreateFile("file", false, 0); return err }},
		{"link-file", func() error { _, err := directory.LinkFileNoReplace(partialFile, "file"); return err }},
		{"replace-file", func() error { return directory.ReplacePrivateFile(partialFile, "file") }},
		{"remove-file", func() error { return directory.RemoveFile("file", partialFile) }},
		{"lock-new", func() error { _, _, err := directory.AcquireLock("lock", false); return err }},
		{"lock-existing", func() error { _, _, err := directory.AcquireLock("lock", true); return err }},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			if err := operation.call(); err == nil {
				t.Fatalf("partial directory operation %s succeeded", operation.name)
			}
		})
	}
}
