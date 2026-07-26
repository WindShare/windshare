//go:build windows

package osfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
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

func TestWindowsV3SelectionRejectsDOSAliasOfControlBeforeMutation(t *testing.T) {
	rootPath := t.TempDir()
	authority := v3RecoveryAuthority(t, rootPath, nil)
	platform, err := openOutputV3Platform(rootPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := platform.ProbeRecoverableFeatures(); err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	control, _, err := authority.openOrBootstrapControl(platform)
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	if err := errors.Join(control.Close(), platform.Close()); err != nil {
		t.Fatal(err)
	}

	controlPath := filepath.Join(rootPath, resumestate.ControlDirectoryName)
	shortLeaf := windowsV3TestShortLeaf(t, controlPath)
	if strings.EqualFold(shortLeaf, resumestate.ControlDirectoryName) {
		t.Skip("test NTFS volume does not assign a DOS alias to the installed control directory")
	}
	longInfo, err := os.Stat(controlPath)
	if err != nil {
		t.Fatal(err)
	}
	aliasInfo, err := os.Stat(filepath.Join(rootPath, shortLeaf))
	if err != nil || !os.SameFile(longInfo, aliasInfo) {
		t.Fatalf("short control name %q is not an alias: same=%t error=%v", shortLeaf, os.SameFile(longInfo, aliasInfo), err)
	}
	rootBefore := windowsV3TestEntryNames(t, rootPath)
	controlBefore := windowsV3TestEntryNames(t, controlPath)
	probeCalls := 0
	authority.platformFactory = func(path string, create bool) (outputV3Platform, error) {
		opened, err := openOutputV3Platform(path, create)
		if err != nil {
			return nil, err
		}
		return &windowsV3TestProbeCountingPlatform{outputV3Platform: opened, calls: &probeCalls}, nil
	}

	attacks := []struct {
		name      string
		selection transfer.OutputSelection
	}{
		{name: "directory-descent", selection: windowsV3TestDirectorySelection(t, shortLeaf+"/must-not-exist")},
		{name: "root-file-locator", selection: v3RecoverySelectionPaths(t, []string{shortLeaf}, 1)},
	}
	for _, attack := range attacks {
		t.Run(attack.name, func(t *testing.T) {
			session, err := authority.OpenSelection(context.Background(), attack.selection)
			if err == nil || !errors.Is(err, errOutputV3Unsafe) {
				if session != nil {
					_, _ = session.PauseJob(context.Background(), transfer.JobPauseOutputFailure)
				}
				t.Fatalf("DOS alias selection error = %v, want unsafe-name rejection", err)
			}
			requireWindowsV3FreshSelectionFault(t, err)
		})
	}
	if probeCalls != 0 {
		t.Fatalf("reserved DOS alias reached recoverability probe %d times", probeCalls)
	}
	if got := windowsV3TestEntryNames(t, rootPath); strings.Join(got, "\x00") != strings.Join(rootBefore, "\x00") {
		t.Fatalf("rejected DOS alias changed output root: before=%v after=%v", rootBefore, got)
	}
	if got := windowsV3TestEntryNames(t, controlPath); strings.Join(got, "\x00") != strings.Join(controlBefore, "\x00") {
		t.Fatalf("rejected DOS alias changed control namespace: before=%v after=%v", controlBefore, got)
	}
	if _, err := os.Lstat(filepath.Join(controlPath, "must-not-exist")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("DOS alias selection created a child in control: %v", err)
	}
}

type windowsV3TestProbeCountingPlatform struct {
	outputV3Platform
	calls *int
}

func (platform *windowsV3TestProbeCountingPlatform) ProbeRecoverableFeatures() error {
	(*platform.calls)++
	return platform.outputV3Platform.ProbeRecoverableFeatures()
}

func windowsV3TestDirectorySelection(t *testing.T, path string) transfer.OutputSelection {
	t.Helper()
	share := v3RecoveryIdentity16[catalog.ShareInstance](0x71)
	root := v3RecoveryIdentity16[catalog.DirectoryID](0x72)
	rootGeneration := v3RecoveryIdentity16[catalog.DirectoryGeneration](0x73)
	components := strings.Split(path, "/")
	directories := make([]transfer.OutputSelectionDirectory, 0, len(components))
	for index := range components {
		directories = append(directories, transfer.OutputSelectionDirectory{
			Path:         strings.Join(components[:index+1], "/"),
			DirectoryID:  v3RecoveryIdentity16[catalog.DirectoryID](byte(0x74 + index)),
			Generation:   v3RecoveryIdentity16[catalog.DirectoryGeneration](byte(0x84 + index)),
			ModifiedTime: v3RecoveryModifiedTime(t),
		})
	}
	plan, err := transfer.NewOutputSelection(
		share, root, rootGeneration, directories, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	request, err := transfer.NewCanonicalSelectionRequest(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := transfer.NewCanonicalSelectionV1(request, plan)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := canonical.BindPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	return selection
}

func windowsV3TestShortLeaf(t *testing.T, path string) string {
	t.Helper()
	longPath, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]uint16, 32<<10)
	count, err := windows.GetShortPathName(longPath, &buffer[0], uint32(len(buffer)))
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 || count >= uint32(len(buffer)) {
		t.Fatalf("invalid short-path length %d", count)
	}
	return filepath.Base(windows.UTF16ToString(buffer[:count]))
}

func windowsV3TestEntryNames(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for index := range entries {
		names[index] = entries[index].Name()
	}
	return names
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
		{path: "COM¹", leaf: true},
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
		strings.Repeat("😀", windowsV3MaximumComponentUTF16Units/2+1),
		strings.Repeat("a/", windowsV3MaximumNTNameUTF16Units/2) + "a",
	}
	for _, path := range invalid {
		if _, err := windowsV3RelativePath(path, false); err == nil {
			t.Fatalf("over-limit NTFS path was accepted (UTF-8 bytes=%d)", len(path))
		}
	}
}

func TestWindowsV3LocatorKeyUsesWindowsOrdinalUpcase(t *testing.T) {
	lower, err := windowsV3OutputLocatorKey("Folder/écho")
	if err != nil {
		t.Fatalf("canonicalize lowercase locator: %v", err)
	}
	upper, err := windowsV3OutputLocatorKey("FOLDER/ÉCHO")
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
		if observeErr != nil || !exact || kind != outputV3EntryRegularFile {
			t.Errorf("entry %q after failed first cut: kind=%v exact=%t error=%v", name, kind, exact, observeErr)
		}
	}
}

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
	if kind, exact, err := root.classifyExactEntry(strings.ToLower(regularName)); err != nil || kind != outputV3EntryRegularFile || exact {
		t.Fatalf("case-alias classification kind=%v exact=%t error=%v", kind, exact, err)
	}
	regular, err := root.openPinnedEntry(regularName)
	if err != nil {
		t.Fatal(err)
	}
	defer regular.close()
	if regular.kind != outputV3EntryRegularFile {
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
	if directoryEntry.kind != outputV3EntryDirectory {
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
	if opaque.kind != outputV3EntryOther {
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
		{name: "selection metadata", run: func() error { return platform.ValidateSelectionMetadata(transfer.OutputSelection{}) }},
		{name: "duplicate directory", run: func() error { _, err := directory.Duplicate(); return err }},
		{name: "sync directory", run: directory.Sync},
		{name: "list names", run: func() error { _, err := directory.Names(1); return err }},
		{name: "list prefix", run: func() error { _, err := directory.NamesWithPrefix("x", 1); return err }},
		{name: "observe entry", run: func() error { _, err := directory.ObserveEntry("x"); return err }},
		{name: "classify exact entry", run: func() error { _, _, err := directory.ClassifyExactEntry("x"); return err }},
		{name: "validate public name", run: func() error { return directory.ValidatePublicEntryName("x") }},
		{name: "validate public names", run: func() error { return directory.ValidatePublicEntryNames([]string{"x"}) }},
		{name: "open entry", run: func() error { _, err := directory.OpenEntry("x"); return err }},
		{name: "match entry", run: func() error { _, err := directory.EntryMatches("x", nil); return err }},
		{name: "open pinned directory", run: func() error { _, err := directory.OpenPinnedDirectory(nil, true); return err }},
		{name: "remove entry", run: func() error { return directory.RemoveEntry("x", nil) }},
		{name: "directory identity", run: func() error { _, err := directory.IdentityClaim(); return err }},
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
		{name: "entry allocation", run: func() error { _, err := entry.AllocatedSize(); return err }},
		{name: "read file", run: func() error { _, err := file.ReadAt(make([]byte, 1), 0); return err }},
		{name: "write file", run: func() error { _, err := file.WriteAt([]byte{1}, 0); return err }},
		{name: "sync file", run: file.Sync},
		{name: "truncate file", run: func() error { return file.Truncate(0) }},
		{name: "file size", run: func() error { _, err := file.Size(); return err }},
		{name: "file allocation", run: func() error { _, err := file.AllocatedSize(); return err }},
		{name: "set file time", run: func() error { return file.SetModifiedTime(catalog.ModifiedTime{}) }},
		{name: "match file metadata", run: func() error { _, err := file.MetadataMatches(0, catalog.ModifiedTime{}); return err }},
		{name: "compare file", run: func() error { _, err := file.SameFile(nil); return err }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.run(); !errors.Is(err, errOutputV3Unsafe) {
				t.Fatalf("error = %v, want fail-closed authority error", err)
			}
		})
	}

	if platform.Root() != nil || (*windowsOutputV3Platform)(nil).Root() != nil ||
		(&windowsOutputV3Platform{root: &windowsOutputV3Directory{}}).Root() != nil {
		t.Fatal("closed platform exposed a root authority")
	}
	if entry.Kind() != outputV3EntryAbsent || (*windowsOutputV3EntryRef)(nil).Kind() != outputV3EntryAbsent {
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
