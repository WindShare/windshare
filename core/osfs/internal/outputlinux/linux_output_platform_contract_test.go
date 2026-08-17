//go:build linux

package outputlinux

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	"golang.org/x/sys/unix"
)

// The adapter contract tests use real directory/file descriptors, while the
// certificate fields are supplied by a deterministic statx shim. This keeps the
// namespace and handle lifecycle testable on CI volumes such as overlayfs, which
// are intentionally not admitted as production ext4 certification roots.
const (
	linuxAdapterTestMountID     = uint64(0x51a7)
	linuxAdapterTestDeviceMajor = uint32(8)
	linuxAdapterTestDeviceMinor = uint32(2)
)

func newLinuxAdapterTestPlatform(t *testing.T) (*linuxV3Platform, string) {
	t.Helper()
	rootPath := t.TempDir()
	fd, err := unix.Open(rootPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open adapter contract root: %v", err)
	}

	system := linuxHostOutputSystem
	originalStatx := system.statx
	system.statx = func(statFD int, path string, flags int, mask int, stat *unix.Statx_t) error {
		if err := originalStatx(statFD, path, flags, mask, stat); err != nil {
			return err
		}
		// The shim changes only the mount identity domain. Object type, inode,
		// ownership, size, blocks, and timestamps remain observations from the
		// real descriptor, so mutations still exercise the native kernel ABI.
		stat.Mnt_id = linuxAdapterTestMountID
		stat.Dev_major = linuxAdapterTestDeviceMajor
		stat.Dev_minor = linuxAdapterTestDeviceMinor
		stat.Mask |= uint32(mask)
		if mask&unix.STATX_BTIME != 0 {
			stat.Btime.Sec = 1_700_000_000
			stat.Btime.Nsec = 123_000_000
		}
		return nil
	}
	// Authority checks are deliberately deterministic here. The production
	// implementation obtains these values from the kernel; returning the safe
	// result lets this test focus on the adapter's sequencing and cleanup rules.
	system.faccessat2 = func(int, string, uint32, int) error { return nil }
	system.getFlags = func(int) (uint32, error) { return 0, nil }
	system.getVersion = func(int) (uint32, error) { return 0, unix.ENOTTY }
	system.geteuid = unix.Geteuid
	system.fsync = func(int) error { return nil }
	system.fstatfs = func(_ int, stat *unix.Statfs_t) error {
		reflect.ValueOf(stat).Elem().FieldByName("Type").SetInt(linuxExt4SuperMagic)
		stat.Fsid.Val = [2]int32{17, 29}
		return nil
	}
	system.getFilesystemUUID = func(int) ([linuxFilesystemUUIDBytes]byte, error) {
		return linuxTestFilesystemUUID, nil
	}

	facts, err := linuxReadOpenHandleFacts(&system, fd, unix.STATX_MNT_ID_UNIQUE)
	if err != nil {
		_ = unix.Close(fd)
		t.Fatalf("read adapter contract root identity: %v", err)
	}
	mount := linuxMountIdentity{
		uniqueMountID:       linuxAdapterTestMountID,
		deviceMajor:         linuxAdapterTestDeviceMajor,
		deviceMinor:         linuxAdapterTestDeviceMinor,
		runtimeFilesystemID: [2]int32{17, 29},
		filesystemUUID:      linuxTestFilesystemUUID,
	}
	restartIdentity, err := linuxReadStatxBirthTimeRestartIdentity(&system, fd, mount)
	if err != nil {
		_ = unix.Close(fd)
		t.Fatalf("read adapter contract restart identity: %v", err)
	}
	certificate := linuxOutputCertificate{
		mount:               mount,
		rootObject:          facts.identity,
		rootRestartIdentity: restartIdentity,
		durability:          linuxOutputProcessRestartDurability,
	}
	root := &linuxV3Directory{
		native: &linuxOutputDirectory{
			system: &system, fd: fd, certificate: certificate,
			object: facts.identity,
		},
	}
	platform := &linuxV3Platform{root: root, rootOpenDisposition: outputcap.CallerProvidedContainer}
	t.Cleanup(func() {
		if err := platform.Close(); err != nil {
			t.Errorf("close adapter contract root: %v", err)
		}
	})
	return platform, rootPath
}

func TestLinuxPlatformAdapterLifecycle(t *testing.T) {
	platform, rootPath := newLinuxAdapterTestPlatform(t)
	root := platform.Root()
	if root == nil {
		t.Fatal("platform returned a nil root")
	}
	if platform.Certification() != outputcap.CertificationLinuxExt4ProcessRestart {
		t.Fatalf("certification = %q", platform.Certification())
	}
	if platform.Durability() != transfer.DurabilityProcessRestart {
		t.Fatalf("durability = %v", platform.Durability())
	}

	guard, err := platform.AcquirePublicOperationGuard()
	if err != nil {
		t.Fatalf("acquire public operation guard: %v", err)
	}
	guardRoot := guard.Root()
	if guardRoot == nil || guardRoot == root {
		t.Fatal("public guard did not return an independently owned root capability")
	}
	if same, sameErr := guardRoot.SameDirectory(root); sameErr != nil || !same {
		t.Fatalf("guard root differs from the platform root: same=%t error=%v", same, sameErr)
	}
	if err := guard.Close(); err != nil {
		t.Fatalf("close public guard: %v", err)
	}
	if guard.Root() != nil {
		t.Fatal("closed public guard retained its root capability")
	}
	if err := guard.Close(); err != nil {
		t.Fatalf("close public guard twice: %v", err)
	}

	binding, err := platform.RootBinding()
	if err != nil || binding.IsZero() || binding.Certification() != outputcap.CertificationLinuxExt4ProcessRestart {
		t.Fatalf("root binding zero=%t certification=%q error=%v", binding.IsZero(), binding.Certification(), err)
	}
	repeated, err := platform.RootBinding()
	if err != nil || repeated != binding {
		t.Fatalf("root binding is not stable: first=%s repeated=%s error=%v", binding.String(), repeated.String(), err)
	}
	if err := platform.ProbeRecoverableFeatures(); err != nil {
		t.Fatalf("probe recoverable features: %v", err)
	}

	modified, err := catalog.NewModifiedTime(1_700_000_000, 123_000_000, catalog.TimePrecisionNanoseconds)
	if err != nil {
		t.Fatal(err)
	}
	if err := platform.ValidateModifiedTime(modified); err != nil {
		t.Fatalf("validate modified time: %v", err)
	}
	if _, err := platform.CanonicalLocatorKey("nested/file"); err != nil {
		t.Fatalf("canonical locator key: %v", err)
	}
	if _, err := platform.CanonicalComponentKey("safe-name"); err != nil {
		t.Fatalf("canonical component key: %v", err)
	}
	if _, err := platform.CanonicalLocatorKey("../escape"); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("unsafe locator was accepted: %v", err)
	}
	if _, err := platform.CanonicalComponentKey("bad/name"); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("unsafe component was accepted: %v", err)
	}

	for _, operation := range []struct {
		name string
		call func() error
	}{
		{"sync root", root.Sync},
		{"validate create authority", func() error {
			validator, ok := root.(outputcap.CreateAuthorityValidator)
			if !ok {
				return errors.New("root does not expose create-authority validation")
			}
			return validator.ValidateCreateAuthority()
		}},
		{"validate metadata authority", func() error {
			validator, ok := root.(outputcap.MetadataAuthorityValidator)
			if !ok {
				return errors.New("root does not expose metadata-authority validation")
			}
			return validator.ValidateMetadataAuthority()
		}},
	} {
		if err := operation.call(); err != nil {
			t.Fatalf("%s: %v", operation.name, err)
		}
	}

	privateDirectory, err := root.CreateDirectory("private", true)
	if err != nil {
		t.Fatalf("create private directory: %v", err)
	}
	publicDirectory, err := root.CreateDirectory("public", false)
	if err != nil {
		t.Fatalf("create public directory: %v", err)
	}
	t.Cleanup(func() { _ = privateDirectory.Close() })
	t.Cleanup(func() { _ = publicDirectory.Close() })

	duplicate, err := privateDirectory.Duplicate()
	if err != nil {
		t.Fatalf("duplicate private directory: %v", err)
	}
	t.Cleanup(func() { _ = duplicate.Close() })
	same, err := privateDirectory.SameDirectory(duplicate)
	if err != nil || !same {
		t.Fatalf("duplicate directory comparison same=%t error=%v", same, err)
	}
	stage, err := privateDirectory.CreateFile("stage", true, 0)
	if err != nil {
		t.Fatalf("create stage file: %v", err)
	}
	t.Cleanup(func() { _ = stage.Close() })
	payload := []byte("linux adapter contract payload")
	if count, err := stage.WriteAt(payload, 0); err != nil || count != len(payload) {
		t.Fatalf("write stage count=%d error=%v", count, err)
	}
	readBack := make([]byte, len(payload))
	if count, err := stage.ReadAt(readBack, 0); err != nil || count != len(payload) || !bytes.Equal(readBack, payload) {
		t.Fatalf("read stage count=%d payload=%q error=%v", count, readBack, err)
	}
	if _, err := stage.ReadAt(make([]byte, len(payload)+1), 0); !errors.Is(err, io.EOF) {
		t.Fatalf("short read did not report EOF: %v", err)
	}
	if size, err := stage.Size(); err != nil || size != uint64(len(payload)) {
		t.Fatalf("stage size=%d error=%v", size, err)
	}
	if err := stage.Sync(); err != nil {
		t.Fatalf("sync stage: %v", err)
	}
	if err := stage.SetModifiedTime(modified); err != nil {
		t.Fatalf("set stage modified time: %v", err)
	}
	if matches, err := stage.MetadataMatches(uint64(len(payload)), modified); err != nil || !matches {
		t.Fatalf("stage metadata matches=%t error=%v", matches, err)
	}

	openedStage, err := privateDirectory.OpenMutableFile("stage", true)
	if err != nil {
		t.Fatalf("reopen stage writable: %v", err)
	}
	t.Cleanup(func() { _ = openedStage.Close() })
	if same, err := stage.SameFile(openedStage); err != nil || !same {
		t.Fatalf("stage same-file comparison same=%t error=%v", same, err)
	}

	anchor, err := root.LinkFileNoReplace(stage, "anchor")
	if err != nil {
		t.Fatalf("link stage into root: %v", err)
	}
	t.Cleanup(func() { _ = anchor.Close() })
	if _, err := root.LinkFileNoReplace(stage, "anchor"); !errors.Is(err, outputcap.ErrNamespaceCollision) {
		t.Fatalf("duplicate hard link did not report collision: %v", err)
	}

	state, err := privateDirectory.CreateFile("state", true, 1)
	if err != nil {
		t.Fatalf("create state file: %v", err)
	}
	t.Cleanup(func() { _ = state.Close() })
	stateReplacement, err := privateDirectory.CreateFile("state.tmp", true, 2)
	if err != nil {
		t.Fatalf("create replacement state file: %v", err)
	}
	t.Cleanup(func() { _ = stateReplacement.Close() })
	if err := privateDirectory.ReplacePrivateFile(stateReplacement, "state"); err != nil {
		t.Fatalf("replace private state file: %v", err)
	}

	entry, err := root.OpenEntry("anchor")
	if err != nil {
		t.Fatalf("open pinned anchor entry: %v", err)
	}
	t.Cleanup(func() { _ = entry.Close() })
	if entry.Kind() != outputcap.EntryRegularFile {
		t.Fatalf("pinned anchor kind=%v", entry.Kind())
	}
	if matches, err := root.EntryMatches("anchor", entry); err != nil || !matches {
		t.Fatalf("pinned anchor match=%t error=%v", matches, err)
	}
	if err := root.RemoveEntry("anchor", entry); err != nil {
		t.Fatalf("remove pinned anchor: %v", err)
	}

	childEntry, err := root.OpenEntry("private")
	if err != nil {
		t.Fatalf("open pinned private directory: %v", err)
	}
	t.Cleanup(func() { _ = childEntry.Close() })
	openedPinned, err := root.OpenPinnedDirectory(childEntry, true)
	if err != nil {
		t.Fatalf("open pinned private directory capability: %v", err)
	}
	t.Cleanup(func() { _ = openedPinned.Close() })
	if same, err := privateDirectory.SameDirectory(openedPinned); err != nil || !same {
		t.Fatalf("pinned directory comparison same=%t error=%v", same, err)
	}

	lock, created, err := root.AcquireLock("lock", false)
	if err != nil || !created {
		t.Fatalf("create output lock created=%t error=%v", created, err)
	}
	if lock.File() == nil {
		t.Fatal("new output lock did not expose its file")
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("close new output lock: %v", err)
	}
	existingLock, created, err := root.AcquireLock("lock", true)
	if err != nil || created {
		t.Fatalf("open existing output lock created=%t error=%v", created, err)
	}
	if err := existingLock.Close(); err != nil {
		t.Fatalf("close existing output lock: %v", err)
	}

	candidate, err := privateDirectory.CreateDirectory("candidate", true)
	if err != nil {
		t.Fatalf("create install candidate: %v", err)
	}
	installed, err := root.InstallDirectoryNoReplace(candidate, "installed")
	if err != nil {
		t.Fatalf("install directory without replacement: %v", err)
	}
	t.Cleanup(func() { _ = installed.Close() })
	if err := root.RemoveDirectory("installed", installed); err != nil {
		t.Fatalf("remove installed directory: %v", err)
	}

	if err := privateDirectory.RemoveFile("state", stateReplacement); err != nil {
		t.Fatalf("remove replaced state file: %v", err)
	}
	if err := privateDirectory.RemoveFile("stage", stage); err != nil {
		t.Fatalf("remove staged source file: %v", err)
	}
	if err := root.RemoveDirectory("public", publicDirectory); err != nil {
		t.Fatalf("remove public directory: %v", err)
	}
	if err := root.RemoveDirectory("private", privateDirectory); err != nil {
		t.Fatalf("remove private directory: %v", err)
	}
	if names, err := root.Names(8); err != nil || len(names) != 1 || names[0] != "lock" {
		t.Fatalf("remaining root names=%v error=%v", names, err)
	}
	if kind, exact, err := root.ClassifyExactEntry("lock"); err != nil || kind != outputcap.EntryRegularFile || !exact {
		t.Fatalf("lock classification kind=%v exact=%t error=%v", kind, exact, err)
	}
	validator, ok := root.(outputcap.PublicEntryNamesValidator)
	if !ok {
		t.Fatal("root does not expose batch public-entry validation")
	}
	if err := validator.ValidatePublicEntryNames([]string{"lock"}); err != nil {
		t.Fatalf("validate public entry names: %v", err)
	}
	if err := root.RemoveFile("lock", nil); err == nil {
		t.Fatal("remove file accepted a nil expected authority")
	}
	if err := os.Remove(filepath.Join(rootPath, "lock")); err != nil {
		t.Fatalf("remove test lock residue: %v", err)
	}
	if err := root.SetModifiedTime(modified); err != nil {
		t.Fatalf("set root modified time: %v", err)
	}
	if err := root.Close(); err != nil {
		t.Fatalf("close root explicitly: %v", err)
	}
}

func TestLinuxPlatformAdapterClosedCapabilityContracts(t *testing.T) {
	var nilPlatform *linuxV3Platform
	if nilPlatform.Root() != nil {
		t.Fatal("nil platform returned a root")
	}
	if _, err := nilPlatform.AcquirePublicOperationGuard(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("nil platform guard error=%v", err)
	}
	if _, err := nilPlatform.RootBinding(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("nil platform binding error=%v", err)
	}
	if err := nilPlatform.ProbeRecoverableFeatures(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("nil platform probe error=%v", err)
	}
	if err := nilPlatform.Close(); err != nil {
		t.Fatalf("nil platform close: %v", err)
	}

	var nilDirectory *linuxV3Directory
	if err := nilDirectory.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := nilDirectory.Duplicate(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("nil directory duplicate error=%v", err)
	}
	if _, err := nilDirectory.Names(1); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("nil directory names error=%v", err)
	}
	if _, err := nilDirectory.ObserveEntry("x"); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("nil directory observe error=%v", err)
	}
	if _, _, err := nilDirectory.ClassifyExactEntry("x"); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("nil directory classify error=%v", err)
	}
	for name, call := range map[string]func() error{
		"sync":               func() error { return nilDirectory.Sync() },
		"validate names":     func() error { return nilDirectory.ValidatePublicEntryNames([]string{"x"}) },
		"create authority":   func() error { return nilDirectory.ValidateCreateAuthority() },
		"metadata authority": func() error { return nilDirectory.ValidateMetadataAuthority() },
		"set modified":       func() error { return nilDirectory.SetModifiedTime(catalog.ModifiedTime{}) },
		"remove entry":       func() error { return nilDirectory.RemoveEntry("x", nil) },
		"remove directory":   func() error { return nilDirectory.RemoveDirectory("x", nil) },
		"remove file":        func() error { return nilDirectory.RemoveFile("x", nil) },
	} {
		if err := call(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Errorf("nil directory %s error=%v", name, err)
		}
	}
	if _, err := nilDirectory.OpenEntry("x"); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Errorf("nil directory open entry error=%v", err)
	}
	if _, err := nilDirectory.OpenPinnedDirectory(nil, false); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Errorf("nil directory open pinned error=%v", err)
	}
	if _, err := nilDirectory.OpenDirectory("x", false); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Errorf("nil directory open directory error=%v", err)
	}
	if _, err := nilDirectory.CreateDirectory("x", false); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Errorf("nil directory create directory error=%v", err)
	}
	if _, err := nilDirectory.CreateFile("x", false, 0); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Errorf("nil directory create file error=%v", err)
	}
	if _, err := nilDirectory.OpenObservedFile("x", false); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Errorf("nil directory open file error=%v", err)
	}
	if _, err := nilDirectory.LinkFileNoReplace(nil, "x"); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Errorf("nil directory link file error=%v", err)
	}
	if err := nilDirectory.ReplacePrivateFile(nil, "x"); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Errorf("nil directory replace file error=%v", err)
	}
	if _, err := nilDirectory.InstallDirectoryNoReplace(nil, "x"); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Errorf("nil directory install directory error=%v", err)
	}
	if _, _, err := nilDirectory.AcquireLock("x", false); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Errorf("nil directory acquire lock error=%v", err)
	}

	var nilFile *linuxV3MutableFile
	if err := nilFile.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := nilFile.ReadAt(make([]byte, 1), 0); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Errorf("nil file read error=%v", err)
	}
	if _, err := nilFile.WriteAt(make([]byte, 1), 0); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Errorf("nil file write error=%v", err)
	}
	for name, call := range map[string]func() error{
		"sync":         func() error { return nilFile.Sync() },
		"set modified": func() error { return nilFile.SetModifiedTime(catalog.ModifiedTime{}) },
	} {
		if err := call(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Errorf("nil file %s error=%v", name, err)
		}
	}
	if _, err := nilFile.Size(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Errorf("nil file size error=%v", err)
	}
	if _, err := nilFile.MetadataMatches(0, catalog.ModifiedTime{}); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Errorf("nil file metadata error=%v", err)
	}
	if _, err := nilFile.SameFile(nil); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Errorf("nil file same-file error=%v", err)
	}

	var nilEntry *linuxV3EntryRef
	if nilEntry.Kind() != outputcap.EntryAbsent {
		t.Fatal("nil entry kind was not absent")
	}
	if err := nilEntry.Close(); err != nil {
		t.Fatal(err)
	}

	var nilLock *linuxV3Lock
	if nilLock.File() != nil {
		t.Fatal("nil lock exposed a file")
	}
	if err := nilLock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxPlatformAdapterErrorTranslationAndOpenContract(t *testing.T) {
	for _, test := range []struct {
		name   string
		cause  error
		marker error
	}{
		{"unsupported", errLinuxOutputUnsupported, outputcap.ErrRecoverableOutputUnsupported},
		{"unsafe", errLinuxOutputUnsafe, outputcap.ErrUnsafeNamespace},
		{"collision", errLinuxOutputCollision, outputcap.ErrNamespaceCollision},
		{"lock busy", errLinuxOutputLockBusy, outputcap.ErrNamespaceLockBusy},
		{"filesystem collision", os.ErrExist, outputcap.ErrNamespaceCollision},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !errors.Is(linuxV3Error(test.cause), test.marker) {
				t.Fatalf("translated error %v does not contain %v", linuxV3Error(test.cause), test.marker)
			}
		})
	}
	if platform, err := Open("relative", false); platform != nil || !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("relative root platform=%v error=%v", platform, err)
	}
	if platform, err := Open(filepath.Join(t.TempDir(), "missing"), false); platform != nil || err == nil {
		t.Fatalf("missing root platform=%v error=%v", platform, err)
	}
}

func TestLinuxPlatformAdapterNamesAreSortedAndBounded(t *testing.T) {
	platform, _ := newLinuxAdapterTestPlatform(t)
	root := platform.Root()
	// Populate through the fixed root handle so the test does not accidentally
	// exercise a different namespace than the capability under test.
	for _, name := range []string{"zeta", "alpha", "middle"} {
		file, err := root.CreateFile(name, false, 0)
		if err != nil {
			t.Fatal(err)
		}
		_ = file.Close()
	}
	names, err := root.Names(8)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha", "middle", "zeta"}
	if !sort.StringsAreSorted(names) || len(names) != len(want) {
		t.Fatalf("names=%v", names)
	}
	for index := range want {
		if names[index] != want[index] {
			t.Fatalf("names=%v want=%v", names, want)
		}
	}
	if _, err := root.Names(1); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("entry bound overflow error=%v", err)
	}
}

func TestLinuxOutputCustomErrors(t *testing.T) {
	cause := errors.New("underlying cause")
	unsupported := &linuxOutputUnsupportedError{operation: "certify", reason: "bad fs", cause: cause}
	if unsupported.Error() == "" || unsupported.Unwrap() != cause || !errors.Is(unsupported, errLinuxOutputUnsupported) {
		t.Fatalf("unsupported error mismatch: %v", unsupported)
	}
	unsafe := &linuxOutputUnsafeError{operation: "open", reason: "unsafe path", cause: cause}
	if unsafe.Error() == "" || unsafe.Unwrap() != cause || !errors.Is(unsafe, errLinuxOutputUnsafe) {
		t.Fatalf("unsafe error mismatch: %v", unsafe)
	}
	collision := &linuxOutputCollisionError{operation: "create", name: "foo.txt", cause: cause}
	if collision.Error() == "" || collision.Unwrap() != cause || !errors.Is(collision, errLinuxOutputCollision) {
		t.Fatalf("collision error mismatch: %v", collision)
	}
}

func TestLinuxPlatformAdapterAdditionalCoverage(t *testing.T) {
	var nilPlatform *linuxV3Platform
	if nilPlatform.Root() != nil || nilPlatform.RootOpenDisposition() != "" {
		t.Fatal("nil platform returned non-empty root disposition")
	}

	platform, _ := newLinuxAdapterTestPlatform(t)
	if platform.RootOpenDisposition() != outputcap.CallerProvidedContainer {
		t.Fatalf("disposition=%v", platform.RootOpenDisposition())
	}

	var nilGuard *linuxOutputPublicOperationGuard
	if nilGuard.Root() != nil {
		t.Fatal("nil guard returned non-nil root")
	}
	guard := &linuxOutputPublicOperationGuard{root: platform.Root()}
	if guard.Root() != platform.Root() {
		t.Fatal("guard root mismatch")
	}

	var nilDir *linuxV3Directory
	if _, err := nilDir.PreparePersistentDirectoryIdentityClaim(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("nil dir prepare claim error=%v", err)
	}
	if err := nilDir.CreateOrdinaryOutputStage(platform.Root(), "stage.tmp", 100); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("nil dir stage error=%v", err)
	}

	rootV3, ok := platform.Root().(*linuxV3Directory)
	if !ok {
		t.Fatalf("root is not linuxV3Directory: %T", platform.Root())
	}
	if err := rootV3.CreateOrdinaryOutputStage(nil, "stage.tmp", 100); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("nil proof stage error=%v", err)
	}
	if err := rootV3.CreateOrdinaryOutputStage(rootV3, "", 100); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("empty name stage error=%v", err)
	}

	claim, err := rootV3.PreparePersistentDirectoryIdentityClaim()
	if err != nil || len(claim) == 0 {
		t.Fatalf("prepare identity claim error=%v len=%d", err, len(claim))
	}

	controlValue, err := rootV3.CreateDirectory("control", true)
	if err != nil {
		t.Fatalf("create private control dir error=%v", err)
	}
	control := controlValue.(*linuxV3Directory)

	if err := rootV3.CreateOrdinaryOutputStage(control, "test_ordinary.tmp", 512); err != nil {
		t.Fatalf("create ordinary stage error=%v", err)
	}

	nativeDir := rootV3.native
	var nilNative *linuxOutputDirectory
	if nilNative.durability() != 0 {
		t.Fatal("nil native durability != 0")
	}
	if nativeDir.durability() != linuxOutputProcessRestartDurability {
		t.Fatalf("durability=%v", nativeDir.durability())
	}

	same, err := nativeDir.SameDirectory(nativeDir)
	if err != nil || !same {
		t.Fatalf("same directory self error=%v same=%v", err, same)
	}

	testFile, err := rootV3.CreateFile("test_file.bin", false, 0)
	if err != nil {
		t.Fatalf("create test file error=%v", err)
	}
	_ = testFile.Close()

	names, err := nativeDir.names(10)
	if err != nil {
		t.Fatalf("native names error=%v", err)
	}
	if len(names) == 0 {
		t.Fatal("expected non-empty native names")
	}

	prefixed, err := nativeDir.namesWithPrefix("test_", 10)
	if err != nil || len(prefixed) == 0 {
		t.Fatalf("prefixed error=%v prefixed=%v", err, prefixed)
	}
}

func TestLinuxReadProcMetadata(t *testing.T) {
	mountInfo, err := linuxReadMountInfo()
	if err != nil || len(mountInfo) == 0 {
		t.Fatalf("read mount info error=%v len=%d", err, len(mountInfo))
	}
	status, err := linuxReadProcessStatus()
	if err != nil || len(status) == 0 {
		t.Fatalf("read process status error=%v len=%d", err, len(status))
	}
	if _, err := linuxReadBoundedProcFile(filepath.Join(t.TempDir(), "missing"), 100); err == nil {
		t.Fatal("expected error for missing proc file")
	}
	tempFile := filepath.Join(t.TempDir(), "large.txt")
	if err := os.WriteFile(tempFile, make([]byte, 200), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := linuxReadBoundedProcFile(tempFile, 100); err == nil {
		t.Fatal("expected error for proc file exceeding safety limit")
	}
}

func TestLinuxLockStableFileErrorBranches(t *testing.T) {
	platform, _ := newLinuxAdapterTestPlatform(t)
	root := platform.Root()
	file, err := root.CreateFile("lock_test.bin", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	v3File := file.(*linuxV3MutableFile)

	origFlock := v3File.state.native.system.flock
	defer func() { v3File.state.native.system.flock = origFlock }()

	v3File.state.native.system.flock = func(int, int) error { return unix.ENOSYS }
	if _, err := linuxLockStableFile(v3File.state.native); !errors.Is(err, errLinuxOutputUnsupported) {
		t.Fatalf("enosys lock error=%v", err)
	}

	v3File.state.native.system.flock = func(int, int) error { return errors.New("flock io error") }
	if _, err := linuxLockStableFile(v3File.state.native); err == nil {
		t.Fatal("expected error for general flock failure")
	}

	readOnlyFile := *v3File.state.native
	readOnlyFile.access = linuxOutputFileObserved
	if _, err := linuxLockStableFile(&readOnlyFile); err == nil {
		t.Fatal("expected error for read-only file lock")
	}
}
