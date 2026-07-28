//go:build windows

package outputwindows

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"golang.org/x/sys/windows"
)

func validWindowsV3CertificationFacts() windowsV3HandleFacts {
	var fileID [16]byte
	fileID[0] = 1
	return windowsV3HandleFacts{
		filesystem: windowsV3OutputFilesystem,
		path:       `\\?\C:\output`,
		driveType:  windows.DRIVE_FIXED,
		flags: windows.FILE_SUPPORTS_HARD_LINKS | windows.FILE_PERSISTENT_ACLS |
			windowsV3FileSupportsPOSIXSemantics,
		attributes: windows.FILE_ATTRIBUTE_DIRECTORY,
		object: windowsV3ObjectIdentity{
			volume: windowsV3VolumeIdentity{guid: `\\?\volume{test}`, serial: 1},
			fileID: fileID,
		},
	}
}

func TestWindowsV3CertificationIsNTFSLocalAndFailClosed(t *testing.T) {
	if err := validateWindowsV3Certification(validWindowsV3CertificationFacts()); err != nil {
		t.Fatalf("valid NTFS facts: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*windowsV3HandleFacts)
	}{
		{name: "ReFS", mutate: func(facts *windowsV3HandleFacts) { facts.filesystem = "ReFS" }},
		{name: "FAT", mutate: func(facts *windowsV3HandleFacts) { facts.filesystem = "FAT32" }},
		{name: "remote", mutate: func(facts *windowsV3HandleFacts) { facts.path = `\\?\UNC\host\share\output` }},
		{name: "removable", mutate: func(facts *windowsV3HandleFacts) { facts.driveType = windows.DRIVE_REMOVABLE }},
		{name: "no hard links", mutate: func(facts *windowsV3HandleFacts) { facts.flags &^= windows.FILE_SUPPORTS_HARD_LINKS }},
		{name: "no persistent ACL", mutate: func(facts *windowsV3HandleFacts) { facts.flags &^= windows.FILE_PERSISTENT_ACLS }},
		{name: "no POSIX namespace", mutate: func(facts *windowsV3HandleFacts) { facts.flags &^= windowsV3FileSupportsPOSIXSemantics }},
		{name: "reparse", mutate: func(facts *windowsV3HandleFacts) { facts.attributes |= windows.FILE_ATTRIBUTE_REPARSE_POINT }},
		{name: "offline", mutate: func(facts *windowsV3HandleFacts) { facts.attributes |= windows.FILE_ATTRIBUTE_OFFLINE }},
		{name: "recall", mutate: func(facts *windowsV3HandleFacts) { facts.attributes |= 0x00400000 }},
		{name: "case sensitive", mutate: func(facts *windowsV3HandleFacts) { facts.caseSensitive = true }},
		{name: "missing volume", mutate: func(facts *windowsV3HandleFacts) { facts.object.volume = windowsV3VolumeIdentity{} }},
		{name: "missing File ID", mutate: func(facts *windowsV3HandleFacts) { facts.object.fileID = [16]byte{} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			facts := validWindowsV3CertificationFacts()
			test.mutate(&facts)
			err := validateWindowsV3Certification(facts)
			if !errors.Is(err, errWindowsV3OutputUnsupported) {
				t.Fatalf("error=%v", err)
			}
			var typed *windowsV3OutputError
			if !errors.As(err, &typed) || typed.Operation == "" {
				t.Fatalf("typed error=%#v", typed)
			}
		})
	}
}

func TestWindowsV3CurrentObjectIdentityCannotBeEncodedAsOwnership(t *testing.T) {
	left := validWindowsV3CertificationFacts().object
	right := left
	if !left.same(right) {
		t.Fatal("same current object was not equal")
	}
	right.fileID[0]++
	if left.same(right) {
		t.Fatal("different current File IDs were equal")
	}
	right = left
	right.volume.serial++
	if left.same(right) {
		t.Fatal("objects on different current volumes were equal")
	}
	if _, ok := any(left).(interface{ Bytes() []byte }); ok {
		t.Fatal("current object identity unexpectedly exposes a persistent encoding")
	}
}

func TestWindowsV3CertificationPrecedesAnyResumeMutation(t *testing.T) {
	root := t.TempDir()
	inspector := windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) {
		facts := validWindowsV3CertificationFacts()
		facts.filesystem = "ReFS"
		return facts, nil
	})
	platform, err := openWindowsV3OutputPlatformWithInspector(root, inspector)
	if platform != nil || !errors.Is(err, errWindowsV3OutputUnsupported) {
		if platform != nil {
			_ = platform.Close()
		}
		t.Fatalf("platform=%v error=%v", platform, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("unsupported admission created entries=%v error=%v", entries, err)
	}
}

func TestWindowsV3RelativeNamesRejectAliasesAndEscapes(t *testing.T) {
	valid := []struct {
		path string
		leaf bool
	}{
		{path: "sessions/00/session", leaf: false},
		{path: "header.state", leaf: true},
	}
	for _, test := range valid {
		if _, err := windowsV3RelativePath(test.path, test.leaf); err != nil {
			t.Fatalf("valid path %q: %v", test.path, err)
		}
	}
	invalid := []struct {
		path string
		leaf bool
	}{
		{path: "", leaf: false},
		{path: ".", leaf: false},
		{path: "../escape", leaf: false},
		{path: `C:\absolute`, leaf: false},
		{path: "record:stream", leaf: true},
		{path: "NUL.txt", leaf: true},
		{path: "COM\u00b9", leaf: true},
		{path: "wild*.state", leaf: true},
		{path: "session./header", leaf: false},
		{path: "session /header", leaf: false},
		{path: "nested/name", leaf: true},
		{path: "a//b", leaf: false},
	}
	for _, test := range invalid {
		if _, err := windowsV3RelativePath(test.path, test.leaf); err == nil {
			t.Fatalf("unsafe path %q was accepted", test.path)
		}
	}
}

func TestWindowsV3RelativeNamesPrevalidateNTFSLimits(t *testing.T) {
	if _, err := windowsV3RelativePath(strings.Repeat("a", windowsV3MaximumComponentUTF16Units), true); err != nil {
		t.Fatalf("maximum NTFS component was rejected: %v", err)
	}
	invalid := []string{
		strings.Repeat("a", windowsV3MaximumComponentUTF16Units+1),
		strings.Repeat("\U0001F600", windowsV3MaximumComponentUTF16Units/2+1),
		strings.Repeat("a/", windowsV3MaximumNTNameUTF16Units/2) + "a",
	}
	for _, path := range invalid {
		if _, err := windowsV3RelativePath(path, false); err == nil {
			t.Fatalf("over-limit NTFS path was accepted (UTF-8 bytes=%d)", len(path))
		}
	}
}

func TestWindowsV3LocatorKeyUsesWindowsOrdinalUpcase(t *testing.T) {
	lower, err := windowsV3OutputLocatorKey("Folder/\u00e9cho")
	if err != nil {
		t.Fatalf("canonicalize lowercase locator: %v", err)
	}
	upper, err := windowsV3OutputLocatorKey("FOLDER/\u00c9CHO")
	if err != nil {
		t.Fatalf("canonicalize uppercase locator: %v", err)
	}
	if lower != upper {
		t.Fatalf("Windows-equivalent locators have different keys %q and %q", lower, upper)
	}
}

func TestWindowsV3PinnedDirectoryEnumerationRestarts(t *testing.T) {
	platform, err := openWindowsV3OutputPlatform(t.TempDir())
	if err != nil {
		t.Fatalf("open NTFS root: %v", err)
	}
	defer platform.Close()
	directory, err := platform.Root().CreatePrivateDirectory("enumeration")
	if err != nil {
		t.Fatalf("create enumeration directory: %v", err)
	}
	defer directory.Close()
	for _, name := range []string{"alpha", "beta", "gamma"} {
		file, err := directory.CreatePrivateFile(name)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("close %s: %v", name, err)
		}
	}
	first, err := directory.names(3)
	if err != nil {
		t.Fatalf("first pinned enumeration: %v", err)
	}
	second, err := directory.names(3)
	if err != nil {
		t.Fatalf("restarted pinned enumeration: %v", err)
	}
	if strings.Join(first, "\x00") != strings.Join(second, "\x00") {
		t.Fatalf("pinned enumeration did not restart: first=%v second=%v", first, second)
	}
}

func TestWindowsV3PrivateNamespaceRejectsCaseAliasesAtEveryLevel(t *testing.T) {
	platform, err := openWindowsV3OutputPlatform(t.TempDir())
	if err != nil {
		t.Fatalf("open NTFS root: %v", err)
	}
	defer platform.Close()

	control, err := platform.Root().CreatePrivateDirectory(".WINDSHARE-OUTPUT")
	if err != nil {
		t.Fatalf("create uppercase control alias: %v", err)
	}
	defer control.Close()
	if opened, err := platform.Root().OpenPrivateDirectory(".windshare-output"); !errors.Is(err, errWindowsV3OutputUnsafe) {
		if opened != nil {
			_ = opened.Close()
		}
		t.Fatalf("top-level private alias was trusted: %v", err)
	}

	shard, err := control.CreatePrivateDirectory("AB")
	if err != nil {
		t.Fatalf("create uppercase shard alias: %v", err)
	}
	defer shard.Close()
	if opened, err := control.OpenPrivateDirectory("ab"); !errors.Is(err, errWindowsV3OutputUnsafe) {
		if opened != nil {
			_ = opened.Close()
		}
		t.Fatalf("private shard alias was trusted: %v", err)
	}

	record, err := control.CreatePrivateFile("RECORD")
	if err != nil {
		t.Fatalf("create uppercase record alias: %v", err)
	}
	defer record.Close()
	if opened, err := control.OpenPrivateFile("record"); !errors.Is(err, errWindowsV3OutputUnsafe) {
		if opened != nil {
			_ = opened.Close()
		}
		t.Fatalf("private record alias was trusted: %v", err)
	}
}

func TestWindowsV3LinkRenameBufferUsesPinnedParent(t *testing.T) {
	const root windows.Handle = 123
	buffer, err := windowsV3LinkRenameBuffer(windows.FILE_RENAME_REPLACE_IF_EXISTS, root, "header.state")
	if err != nil {
		t.Fatal(err)
	}
	information := (*windowsV3LinkRenameInformation)(unsafe.Pointer(&buffer[0]))
	if information.Flags != windows.FILE_RENAME_REPLACE_IF_EXISTS || information.RootDirectory != root {
		t.Fatalf("information=%+v", information)
	}
	if information.FileNameLength != uint32(len([]rune("header.state"))*2) {
		t.Fatalf("filename bytes=%d", information.FileNameLength)
	}
	name := windows.UTF16ToString(unsafe.Slice(&information.FileName[0], int(information.FileNameLength/2)))
	if name != "header.state" {
		t.Fatalf("name=%q", name)
	}
}

func TestWindowsV3PrivateNamespaceAndNativePrimitives(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	if platform.Durability() != windowsV3OutputProcessRestart {
		t.Fatalf("durability=%d", platform.Durability())
	}

	candidate, err := platform.Root().CreatePrivateDirectory(".windshare-output.bootstrap-test")
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	control, err := platform.Root().InstallPrivateDirectoryNoReplace(candidate, ".windshare-output")
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if err := control.Sync(); err != nil {
		t.Fatal(err)
	}

	stage, err := control.CreatePrivateFile("stage.tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	payload := []byte("checkpoint payload")
	if err := stage.Truncate(int64(len(payload))); err != nil {
		t.Fatal(err)
	}
	if count, err := stage.WriteAt(payload, 0); err != nil || count != len(payload) {
		t.Fatalf("write count=%d error=%v", count, err)
	}
	if err := stage.Sync(); err != nil {
		t.Fatal(err)
	}

	anchor, err := control.LinkRegularFileNoReplace(stage, "anchor.link")
	if err != nil {
		t.Fatal(err)
	}
	defer anchor.Close()
	same, err := sameWindowsV3OpenedObject(stage, anchor)
	if err != nil || !same {
		t.Fatalf("stage/anchor same=%v error=%v", same, err)
	}
	if _, err := control.LinkRegularFileNoReplace(stage, "anchor.link"); !errors.Is(err, errWindowsV3OutputCollision) {
		t.Fatalf("second link error=%v", err)
	}

	final, err := platform.Root().LinkRegularFileNoReplace(anchor, "published.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer final.Close()
	read := make([]byte, len(payload))
	if count, err := final.ReadAt(read, 0); err != nil || count != len(read) || !bytes.Equal(read, payload) {
		t.Fatalf("published read=%q count=%d error=%v", read, count, err)
	}
	finalInfo, err := os.Lstat(filepath.Join(platform.Root().path, "published.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if native, ok := finalInfo.Sys().(*syscall.Win32FileAttributeData); !ok || native.FileAttributes&windows.FILE_ATTRIBUTE_HIDDEN != 0 {
		t.Fatalf("published file inherited internal Hidden semantics: %#v", finalInfo.Sys())
	}
	if err := platform.Root().Sync(); err != nil {
		t.Fatal(err)
	}
	if err := control.RemoveRegularLink("stage.tmp", stage); err != nil {
		t.Fatal(err)
	}
	if err := control.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := control.RemoveRegularLink("anchor.link", anchor); err != nil {
		t.Fatal(err)
	}
	if err := control.Sync(); err != nil {
		t.Fatal(err)
	}
	if count, err := final.ReadAt(read, 0); err != nil || count != len(read) || !bytes.Equal(read, payload) {
		t.Fatalf("retired internal links changed final=%q count=%d error=%v", read, count, err)
	}
}

func TestWindowsV3AtomicPrivateStateReplacement(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	control, _, err := platform.Root().OpenOrCreatePrivateDirectory(".windshare-output")
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()

	install := func(temp string, payload []byte) {
		t.Helper()
		file, err := control.CreatePrivateFile(temp)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if _, err := file.WriteAt(payload, 0); err != nil {
			t.Fatal(err)
		}
		if err := file.Sync(); err != nil {
			t.Fatal(err)
		}
		if err := control.AtomicReplacePrivateFile(file, "header.state"); err != nil {
			t.Fatal(err)
		}
		if err := control.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	install("header.1.tmp", []byte("generation one"))
	install("header.2.tmp", []byte("generation two"))

	state, err := control.OpenPrivateFile("header.state")
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	buffer := make([]byte, len("generation two"))
	if count, err := state.ReadAt(buffer, 0); err != nil || count != len(buffer) || string(buffer) != "generation two" {
		t.Fatalf("state=%q count=%d error=%v", buffer, count, err)
	}
}

func TestWindowsV3StableLockIsNeverReallocatedForContention(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	control, _, err := platform.Root().OpenOrCreatePrivateDirectory(".windshare-output")
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()

	first, created, err := control.AcquireStableLock("coordinator.lock")
	if err != nil || !created {
		t.Fatalf("first lock created=%v error=%v", created, err)
	}
	if second, _, err := control.AcquireStableLock("coordinator.lock"); !errors.Is(err, errWindowsV3OutputLockBusy) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("contending lock error=%v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, created, err := control.AcquireStableLock("coordinator.lock")
	if err != nil || created {
		t.Fatalf("reopened lock created=%v error=%v", created, err)
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsV3RemovalRequiresCurrentOpenIdentity(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	control, _, err := platform.Root().OpenOrCreatePrivateDirectory(".windshare-output")
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	first, err := control.CreatePrivateFile("first.stage")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := control.CreatePrivateFile("second.stage")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	if err := control.RemoveRegularLink("first.stage", second); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("mismatched removal error=%v", err)
	}
	reopened, err := control.OpenPrivateFile("first.stage")
	if err != nil {
		t.Fatalf("mismatched removal deleted the current entry: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsV3DirectoryInstallationIsAtomicNoReplace(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	installed, _, err := platform.Root().OpenOrCreatePrivateDirectory(".windshare-output")
	if err != nil {
		t.Fatal(err)
	}
	defer installed.Close()
	candidate, err := platform.Root().CreatePrivateDirectory(".windshare-output.bootstrap-collision")
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()

	if opened, err := platform.Root().InstallPrivateDirectoryNoReplace(candidate, ".windshare-output"); !errors.Is(err, errWindowsV3OutputCollision) {
		if opened != nil {
			_ = opened.Close()
		}
		t.Fatalf("directory collision error=%v", err)
	}
	current, err := platform.Root().OpenPrivateDirectory(".windshare-output")
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	same, err := sameWindowsV3OpenedDirectory(current, installed)
	if err != nil || !same {
		t.Fatalf("installed directory changed same=%v error=%v", same, err)
	}
}

func TestWindowsV3ExistingPrivateNamespaceFailsWithoutMutation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "broad-control")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	platform, err := openWindowsV3OutputPlatform(root)
	if errors.Is(err, errWindowsV3OutputUnsupported) {
		t.Skipf("test volume is outside the local NTFS matrix: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	if opened, err := platform.Root().OpenPrivateDirectory("broad-control"); !errors.Is(err, errWindowsV3OutputUnsafe) {
		if opened != nil {
			_ = opened.Close()
		}
		t.Fatalf("broad control error=%v", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	native, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		t.Fatalf("native info=%T", info.Sys())
	}
	if native.FileAttributes&windows.FILE_ATTRIBUTE_HIDDEN != 0 {
		t.Fatal("failed validation mutated the existing directory to Hidden")
	}
}

func TestWindowsV3NoFollowDirectoryOpenRejectsReparse(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	target := filepath.Join(platform.Root().path, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(platform.Root().path, "alias")
	if err := os.Symlink("target", alias); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	if opened, err := platform.Root().OpenDirectory("alias"); !errors.Is(err, errWindowsV3OutputUnsafe) {
		if opened != nil {
			_ = opened.Close()
		}
		t.Fatalf("reparse open error=%v", err)
	}
}

func TestWindowsV3RecoverableFeatureProbeExercisesAndCleansNativeNamespace(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	guard, err := platform.acquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	root := guard.Root()
	nonce := bytes.Repeat([]byte{0x5a}, windowsV3OutputProbeRandomBytes)
	if err := root.probeRecoverableFeaturesWithRandom(bytes.NewReader(nonce)); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(platform.Root().path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("feature probe left namespace entries: %v", entries)
	}
}

func TestWindowsV3FeatureProbeRejectsMissingEntropyBeforeMutation(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	guard, err := platform.acquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	if err := guard.Root().probeRecoverableFeaturesWithRandom(bytes.NewReader(nil)); err == nil {
		t.Fatal("feature probe accepted an exhausted random source")
	}
	entries, err := os.ReadDir(platform.Root().path)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed probe created entries=%v error=%v", entries, err)
	}
}

func TestWindowsV3FeatureProbeNeverAdoptsPreexistingReservedLockFile(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	guard, err := platform.acquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	root := guard.Root()
	path := filepath.Join(platform.Root().path, windowsV3OutputProbeLockName)
	want := []byte("not WindShare control metadata")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.probeRecoverableFeaturesWithRandom(bytes.NewReader(nil)); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("preexisting reserved lock error=%v", err)
	}
	after, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("preexisting reserved lock was removed: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || !bytes.Equal(got, want) {
		t.Fatalf("preexisting reserved lock was adopted or mutated: identity=%v content=%q", os.SameFile(before, after), got)
	}
	entries, err := os.ReadDir(platform.Root().path)
	if err != nil || len(entries) != 1 || entries[0].Name() != windowsV3OutputProbeLockName {
		t.Fatalf("failed probe mutated unrelated namespace entries=%v error=%v", entries, err)
	}
}

func TestWindowsV3ProbeCleanupStopsAtFirstUnverifiedCut(t *testing.T) {
	rootPath := t.TempDir()
	platform, err := openWindowsV3OutputPlatform(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	const probeName = ".windshare-output.probe-00112233445566778899aabbccddeeff"
	directory, err := platform.root.CreatePrivateDirectory(probeName)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := directory.CreatePrivateFile("stage")
	if err != nil {
		t.Fatal(err)
	}
	if err := stage.Truncate(1); err != nil {
		t.Fatal(err)
	}
	anchor, err := directory.LinkRegularFileNoReplace(stage, "anchor")
	if err != nil {
		t.Fatal(err)
	}
	publication, err := directory.LinkRegularFileNoReplace(anchor, "publication")
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.RemoveRegularLink("stage", stage); err != nil {
		t.Fatal(err)
	}
	replacement, err := directory.CreatePrivateFile("stage")
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(replacement.Truncate(1), replacement.Close()); err != nil {
		t.Fatal(err)
	}

	probe := windowsV3OutputProbe{
		root: platform.root, rootName: probeName, directory: directory, rootPresent: true,
		stage: stage, anchor: anchor, publication: publication,
		stageMayExist: true, anchorMayExist: true, publicationMayExist: true,
	}
	if err := probe.cleanup(); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("cleanup mismatch error = %v", err)
	}

	leftover, err := platform.root.OpenPrivateDirectory(probeName)
	if err != nil {
		t.Fatalf("reopen fail-fast probe namespace: %v", err)
	}
	defer leftover.Close()
	for _, name := range []string{"stage", "anchor", "publication"} {
		kind, exact, observeErr := leftover.classifyExactEntry(name)
		if observeErr != nil || !exact || kind != outputcap.EntryRegularFile {
			t.Errorf("entry %q after failed first cut: kind=%v exact=%t error=%v", name, kind, exact, observeErr)
		}
	}
}
