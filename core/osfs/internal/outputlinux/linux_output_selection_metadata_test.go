//go:build linux

package outputlinux

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	"golang.org/x/sys/unix"
)

type linuxSelectionMetadataHarness struct {
	size                   uint64
	seconds                int64
	nanoseconds            uint32
	truncateCalls          []uint64
	modifiedCalls          []linuxSelectionMetadataTime
	syncCalls              []int
	openCalls              int
	closeCalls             int
	duplicateCalls         int
	duplicateCloseCalls    int
	rejectSize             uint64
	roundModifiedNsec      bool
	roundModifiedOnSync    bool
	omitProbeBirthTime     bool
	omitDirectoryBirthTime bool
	directoryMode          uint16
	ownerUID               uint32
}

type linuxSelectionMetadataTime struct {
	seconds     int64
	nanoseconds uint32
}

func TestLinuxSelectionMetadataUsesOneAnonymousInodeAndBoundedWitnesses(t *testing.T) {
	secondMinimum := linuxSelectionMetadataModified(t, 1_600_000_000, 0, catalog.TimePrecisionSeconds)
	secondMaximum := linuxSelectionMetadataModified(t, 1_800_000_000, 0, catalog.TimePrecisionSeconds)
	nanosecondMinimum := linuxSelectionMetadataModified(t, 1_700_000_001, 100, catalog.TimePrecisionNanoseconds)
	nanosecondMiddle := linuxSelectionMetadataModified(t, 1_700_000_001, 200, catalog.TimePrecisionNanoseconds)
	nanosecondMaximum := linuxSelectionMetadataModified(t, 1_700_000_001, 300, catalog.TimePrecisionNanoseconds)
	selection := linuxSelectionMetadataSelection(t, []linuxSelectionMetadataFile{
		{size: 4096, modified: secondMaximum},
		{size: 8192, modified: nanosecondMiddle},
		{size: 16_384, modified: secondMinimum},
		{size: 1024, modified: nanosecondMaximum},
		{size: 2048, modified: nanosecondMinimum},
	})
	root, harness := newLinuxSelectionMetadataRoot(t)
	if err := root.validateSelectionMetadata(selection); err != nil {
		t.Fatal(err)
	}
	if harness.openCalls != 1 || harness.closeCalls != 1 {
		t.Fatalf("anonymous probe opens=%d closes=%d", harness.openCalls, harness.closeCalls)
	}
	if !reflect.DeepEqual(harness.truncateCalls, []uint64{16_384}) {
		t.Fatalf("sparse size witnesses = %v", harness.truncateCalls)
	}
	wantModified := []linuxSelectionMetadataTime{
		{seconds: secondMinimum.Seconds()},
		{seconds: secondMaximum.Seconds()},
		{seconds: nanosecondMinimum.Seconds(), nanoseconds: nanosecondMinimum.Nanoseconds()},
		{seconds: nanosecondMaximum.Seconds(), nanoseconds: nanosecondMaximum.Nanoseconds()},
	}
	if !reflect.DeepEqual(harness.modifiedCalls, wantModified) {
		t.Fatalf("bounded modified-time witnesses = %v", harness.modifiedCalls)
	}
	if len(harness.syncCalls) != 1+len(wantModified) {
		t.Fatalf("probe sync calls = %v", harness.syncCalls)
	}
}

func TestLinuxSelectionMetadataRejectsNativeSizeAndTimestampRounding(t *testing.T) {
	modified := linuxSelectionMetadataModified(t, 1_700_000_001, 123_456_789, catalog.TimePrecisionNanoseconds)
	selection := linuxSelectionMetadataSelection(t, []linuxSelectionMetadataFile{{size: 8192, modified: modified}})

	t.Run("size", func(t *testing.T) {
		root, harness := newLinuxSelectionMetadataRoot(t)
		harness.rejectSize = 8192
		if err := root.validateSelectionMetadata(selection); !errors.Is(err, errLinuxOutputUnsupported) {
			t.Fatalf("unrepresentable size error = %v", err)
		}
		if harness.closeCalls != 1 {
			t.Fatalf("failed size probe close count = %d", harness.closeCalls)
		}
	})

	t.Run("timestamp before writeback", func(t *testing.T) {
		root, harness := newLinuxSelectionMetadataRoot(t)
		harness.roundModifiedNsec = true
		if err := root.validateSelectionMetadata(selection); !errors.Is(err, errLinuxOutputUnsupported) {
			t.Fatalf("rounded timestamp error = %v", err)
		}
		if harness.closeCalls != 1 {
			t.Fatalf("failed timestamp probe close count = %d", harness.closeCalls)
		}
	})

	t.Run("timestamp during writeback", func(t *testing.T) {
		root, harness := newLinuxSelectionMetadataRoot(t)
		harness.roundModifiedOnSync = true
		if err := root.validateSelectionMetadata(selection); !errors.Is(err, errLinuxOutputUnsupported) {
			t.Fatalf("writeback-rounded timestamp error = %v", err)
		}
		if len(harness.syncCalls) < 2 {
			t.Fatalf("writeback was not exercised before observation: syncs=%v", harness.syncCalls)
		}
		if harness.closeCalls != 1 {
			t.Fatalf("failed writeback probe close count = %d", harness.closeCalls)
		}
	})
}

func TestLinuxExtendedTimestampLayoutClassification(t *testing.T) {
	for _, test := range []struct {
		name     string
		modified catalog.ModifiedTime
		want     bool
	}{
		{name: "absent"},
		{
			name:     "signed minimum seconds",
			modified: linuxSelectionMetadataModified(t, math.MinInt32, 0, catalog.TimePrecisionSeconds),
		},
		{
			name:     "signed maximum nanosecond declaration with zero fraction",
			modified: linuxSelectionMetadataModified(t, math.MaxInt32, 0, catalog.TimePrecisionNanoseconds),
		},
		{
			name: "millisecond fraction",
			modified: linuxSelectionMetadataModified(
				t, 1_700_000_001, 1_000_000, catalog.TimePrecisionMilliseconds,
			),
			want: true,
		},
		{
			name: "nanosecond fraction",
			modified: linuxSelectionMetadataModified(
				t, 1_700_000_001, 1, catalog.TimePrecisionNanoseconds,
			),
			want: true,
		},
		{
			name: "extended future",
			modified: linuxSelectionMetadataModified(
				t, int64(math.MaxInt32)+1, 0, catalog.TimePrecisionSeconds,
			),
			want: true,
		},
		{
			name: "extended past",
			modified: linuxSelectionMetadataModified(
				t, int64(math.MinInt32)-1, 0, catalog.TimePrecisionSeconds,
			),
			want: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := linuxModifiedTimeRequiresExtendedInodeFields(test.modified); got != test.want {
				t.Fatalf("requires extended inode fields = %t, want %t", got, test.want)
			}
		})
	}
}

func TestLinuxSelectionMetadataRequiresPersistentExtendedTimestampLayout(t *testing.T) {
	fractional := linuxSelectionMetadataModified(
		t, 1_700_000_001, 1_000_000, catalog.TimePrecisionMilliseconds,
	)
	legacyMinimum := linuxSelectionMetadataModified(t, math.MinInt32, 0, catalog.TimePrecisionSeconds)
	legacyMaximum := linuxSelectionMetadataModified(t, math.MaxInt32, 0, catalog.TimePrecisionSeconds)
	extendedFuture := linuxSelectionMetadataModified(t, int64(math.MaxInt32)+1, 0, catalog.TimePrecisionSeconds)
	extendedPast := linuxSelectionMetadataModified(t, int64(math.MinInt32)-1, 0, catalog.TimePrecisionSeconds)

	for _, test := range []struct {
		name     string
		modified []catalog.ModifiedTime
		wantErr  bool
	}{
		{name: "legacy signed minimum and maximum", modified: []catalog.ModifiedTime{legacyMinimum, legacyMaximum}},
		{name: "nonzero milliseconds", modified: []catalog.ModifiedTime{fractional}, wantErr: true},
		{name: "future epoch extension", modified: []catalog.ModifiedTime{extendedFuture}, wantErr: true},
		{name: "past epoch extension", modified: []catalog.ModifiedTime{extendedPast}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			files := make([]linuxSelectionMetadataFile, len(test.modified))
			for index, modified := range test.modified {
				files[index] = linuxSelectionMetadataFile{modified: modified}
			}
			root, harness := newLinuxSelectionMetadataRoot(t)
			harness.omitProbeBirthTime = true
			err := root.validateSelectionMetadata(linuxSelectionMetadataSelection(t, files))
			if test.wantErr {
				if !errors.Is(err, errLinuxOutputUnsupported) {
					t.Fatalf("missing persistent layout error = %v", err)
				}
			} else if err != nil {
				t.Fatalf("legacy timestamp validation = %v", err)
			}
			if harness.closeCalls != 1 {
				t.Fatalf("anonymous layout witness close count = %d", harness.closeCalls)
			}
		})
	}
}

func TestLinuxSelectionMetadataPolicyIsNotPublishedBeforeProbeSuccess(t *testing.T) {
	fractional := linuxSelectionMetadataModified(
		t, 1_700_000_001, 1, catalog.TimePrecisionNanoseconds,
	)
	selection := linuxSelectionMetadataSelectionWithDirectories(t, []linuxSelectionMetadataDirectory{
		{path: "nested", modified: fractional},
	})
	root, harness := newLinuxSelectionMetadataRoot(t)
	harness.omitProbeBirthTime = true
	platform := &linuxV3Platform{root: &linuxV3Directory{native: root}}
	if err := platform.ValidateSelectionMetadata(selection); !errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) {
		t.Fatalf("failed layout probe error = %v", err)
	}
	if platform.root.metadataPolicy != nil {
		t.Fatal("selection policy became authoritative before metadata certification")
	}
	if harness.closeCalls != 1 {
		t.Fatalf("failed policy probe close count = %d", harness.closeCalls)
	}
}

func TestLinuxTimestampMutationAndComparisonRecheckExactInodeLayout(t *testing.T) {
	modified := linuxSelectionMetadataModified(
		t, 1_700_000_001, 1, catalog.TimePrecisionNanoseconds,
	)

	root, harness := newLinuxSelectionMetadataRoot(t)
	harness.omitDirectoryBirthTime = true
	if err := root.setModifiedTime(modified); !errors.Is(err, errLinuxOutputUnsupported) {
		t.Fatalf("directory timestamp mutation layout error = %v", err)
	}
	if _, err := root.metadataMatches(0, modified); !errors.Is(err, errLinuxOutputUnsupported) {
		t.Fatalf("directory timestamp comparison layout error = %v", err)
	}

	root, harness = newLinuxSelectionMetadataRoot(t)
	harness.omitProbeBirthTime = true
	object := root.certificate.rootObject
	object.inode++
	object.kind = unix.S_IFREG
	file := &linuxOutputRegularFile{
		system: root.system, fd: 11, certificate: root.certificate, object: object, writable: true,
	}
	if err := file.setModifiedTime(modified); !errors.Is(err, errLinuxOutputUnsupported) {
		t.Fatalf("file timestamp mutation layout error = %v", err)
	}
	if _, err := file.metadataMatches(0, modified); !errors.Is(err, errLinuxOutputUnsupported) {
		t.Fatalf("file timestamp comparison layout error = %v", err)
	}
}

func TestLinuxDirectoryWalkCarriesImmutableSelectionMetadataPolicy(t *testing.T) {
	root, harness := newLinuxSelectionMetadataRoot(t)
	policy := &linuxSelectionMetadataPolicy{
		extendedTimestampDirectories: []string{"nested/fractional"},
	}
	parent := &linuxV3Directory{
		native: root, metadataPolicy: policy, selectionPath: "nested",
	}
	childNative := &linuxOutputDirectory{
		system: root.system, fd: root.fd, certificate: root.certificate, object: root.object,
	}
	bound, err := parent.bindDirectoryOrigin(childNative, "fractional")
	if err != nil {
		t.Fatal(err)
	}
	child := bound.(*linuxV3Directory)
	if child.metadataPolicy != policy || child.selectionPath != "nested/fractional" {
		t.Fatalf("child policy/path = (%p, %q)", child.metadataPolicy, child.selectionPath)
	}
	if harness.duplicateCalls != 1 {
		t.Fatalf("fixed parent duplicate calls = %d", harness.duplicateCalls)
	}
	if err := child.origin.parent.close(); err != nil {
		t.Fatal(err)
	}
	if harness.duplicateCloseCalls != 1 {
		t.Fatalf("fixed parent duplicate close calls = %d", harness.duplicateCloseCalls)
	}
	child.origin = nil
}

func TestLinuxSelectionMetadataPolicyProvesExactSelectedDirectoryInode(t *testing.T) {
	legacy := linuxSelectionMetadataModified(t, 1_700_000_001, 0, catalog.TimePrecisionSeconds)
	fractional := linuxSelectionMetadataModified(
		t, 1_700_000_001, 1, catalog.TimePrecisionNanoseconds,
	)
	selection := linuxSelectionMetadataSelectionWithDirectories(t, []linuxSelectionMetadataDirectory{
		{path: "legacy", modified: legacy},
		{path: "nested", modified: legacy},
		{path: "nested/fractional", modified: fractional},
	})
	root, harness := newLinuxSelectionMetadataRoot(t)
	platform := &linuxV3Platform{root: &linuxV3Directory{native: root}}
	if err := platform.ValidateSelectionMetadata(selection); err != nil {
		t.Fatalf("attach selection metadata policy: %v", err)
	}
	policy := platform.root.metadataPolicy
	if err := platform.ValidateSelectionMetadata(selection); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("rebind immutable selection policy error = %v", err)
	}
	if platform.root.metadataPolicy != policy {
		t.Fatal("selection metadata policy changed after it became authoritative")
	}
	if !reflect.DeepEqual(policy.extendedTimestampDirectories, []string{"nested/fractional"}) {
		t.Fatalf("extended directory policy = %v", policy.extendedTimestampDirectories)
	}
	if policy.requiresExtendedTimestamp("legacy") || policy.requiresExtendedTimestamp("nested") {
		t.Fatal("legacy directories unexpectedly require extended inode fields")
	}
	if !policy.requiresExtendedTimestamp("nested/fractional") {
		t.Fatal("fractional selected directory lacks an extended-inode policy")
	}

	harness.omitDirectoryBirthTime = true
	directory := &linuxV3Directory{
		native: root, metadataPolicy: policy, selectionPath: "nested/fractional",
	}
	if err := directory.ValidateMetadataAuthority(); !errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) {
		t.Fatalf("selected directory without persistent layout error = %v", err)
	}

	root, harness = newLinuxSelectionMetadataRoot(t)
	harness.omitDirectoryBirthTime = true
	directory = &linuxV3Directory{native: root, metadataPolicy: policy, selectionPath: "legacy"}
	if err := directory.ValidateMetadataAuthority(); err != nil {
		t.Fatalf("legacy selected directory authority = %v", err)
	}
}

type linuxSelectionMetadataDirectory struct {
	path     string
	modified catalog.ModifiedTime
}

type linuxSelectionMetadataFile struct {
	size     uint64
	modified catalog.ModifiedTime
}

func linuxSelectionMetadataSelection(
	t *testing.T,
	files []linuxSelectionMetadataFile,
) transfer.OutputSelection {
	t.Helper()
	return linuxSelectionMetadataPlan(t, nil, files)
}

func linuxSelectionMetadataSelectionWithDirectories(
	t *testing.T,
	directories []linuxSelectionMetadataDirectory,
) transfer.OutputSelection {
	t.Helper()
	return linuxSelectionMetadataPlan(t, directories, nil)
}

func linuxSelectionMetadataPlan(
	t *testing.T,
	directories []linuxSelectionMetadataDirectory,
	files []linuxSelectionMetadataFile,
) transfer.OutputSelection {
	t.Helper()
	share := linuxTestIdentity16[catalog.ShareInstance](0x91)
	root := linuxTestIdentity16[catalog.DirectoryID](0x92)
	generation := linuxTestIdentity16[catalog.DirectoryGeneration](0x93)
	selectedDirectories := make([]transfer.OutputSelectionDirectory, len(directories))
	for index, directory := range directories {
		selectedDirectories[index] = transfer.OutputSelectionDirectory{
			Path:         directory.path,
			DirectoryID:  linuxTestIdentity16[catalog.DirectoryID](byte(0xb0 + index)),
			Generation:   linuxTestIdentity16[catalog.DirectoryGeneration](byte(0xc0 + index)),
			ModifiedTime: directory.modified,
		}
	}
	selectedFiles := make([]transfer.OutputSelectionFile, len(files))
	for index, file := range files {
		selectedFiles[index] = transfer.OutputSelectionFile{
			Path:              fmt.Sprintf("file-%03d.bin", index),
			FileID:            linuxTestIdentity16[catalog.FileID](byte(0xa0 + index)),
			ParentDirectoryID: root, ParentGeneration: generation,
			ExpectedSize: file.size, ModifiedTime: file.modified,
		}
	}
	plan, err := transfer.NewOutputSelection(
		share, root, generation, selectedDirectories, selectedFiles,
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

func linuxSelectionMetadataModified(
	t *testing.T,
	seconds int64,
	nanoseconds uint32,
	precision catalog.TimePrecision,
) catalog.ModifiedTime {
	t.Helper()
	modified, err := catalog.NewModifiedTime(seconds, nanoseconds, precision)
	if err != nil {
		t.Fatal(err)
	}
	return modified
}

func newLinuxSelectionMetadataRoot(
	t *testing.T,
) (*linuxOutputDirectory, *linuxSelectionMetadataHarness) {
	t.Helper()
	const (
		rootFD      = 10
		probeFD     = 11
		duplicateFD = 12
	)
	harness := &linuxSelectionMetadataHarness{}
	filesystemID := [2]int32{17, 29}
	system := &linuxOutputSystem{}
	system.openat2 = func(directory int, path string, how *unix.OpenHow) (int, error) {
		if directory == rootFD && path == "." && how.Flags&uint64(unix.O_TMPFILE) != 0 &&
			how.Flags&uint64(unix.O_RDWR) != 0 {
			harness.openCalls++
			return probeFD, nil
		}
		if directory == rootFD && path == "." && how.Flags&uint64(unix.O_DIRECTORY) != 0 {
			harness.duplicateCalls++
			return duplicateFD, nil
		}
		t.Fatalf("metadata harness open dir=%d path=%q how=%+v", directory, path, *how)
		return -1, unix.EINVAL
	}
	system.close = func(fd int) error {
		switch fd {
		case probeFD:
			harness.closeCalls++
		case duplicateFD:
			harness.duplicateCloseCalls++
		default:
			t.Fatalf("close fd=%d", fd)
		}
		return nil
	}
	system.ftruncate = func(fd int, size int64) error {
		if fd != probeFD || size < 0 {
			return unix.EINVAL
		}
		if uint64(size) == harness.rejectSize && harness.rejectSize != 0 {
			return unix.EFBIG
		}
		harness.size = uint64(size)
		harness.truncateCalls = append(harness.truncateCalls, uint64(size))
		return nil
	}
	system.utimensat = func(fd int, path string, times []unix.Timespec, flags int) error {
		if fd != probeFD || path != "" || flags != unix.AT_EMPTY_PATH || len(times) != 2 {
			return unix.EINVAL
		}
		harness.seconds = int64(times[1].Sec)
		harness.nanoseconds = uint32(times[1].Nsec)
		if harness.roundModifiedNsec {
			harness.nanoseconds = harness.nanoseconds / 1_000_000 * 1_000_000
		}
		harness.modifiedCalls = append(harness.modifiedCalls, linuxSelectionMetadataTime{
			seconds: int64(times[1].Sec), nanoseconds: uint32(times[1].Nsec),
		})
		return nil
	}
	system.fsync = func(fd int) error {
		if fd != probeFD {
			return unix.EBADF
		}
		harness.syncCalls = append(harness.syncCalls, fd)
		if harness.roundModifiedOnSync {
			harness.nanoseconds = harness.nanoseconds / 1_000_000 * 1_000_000
		}
		return nil
	}
	system.statx = func(fd int, _ string, _ int, mask int, stat *unix.Statx_t) error {
		mode := harness.directoryMode
		if mode == 0 {
			mode = uint16(unix.S_IFDIR | 0o755)
		}
		inode := uint64(linuxTestRootInode)
		size := uint64(0)
		if fd == probeFD {
			mode = uint16(unix.S_IFREG | linuxOutputStateFileMode)
			inode++
			size = harness.size
		}
		returnedMask := uint32(mask)
		if (fd == probeFD && harness.omitProbeBirthTime) ||
			(fd == rootFD && harness.omitDirectoryBirthTime) {
			returnedMask &^= uint32(unix.STATX_BTIME)
		}
		*stat = unix.Statx_t{
			Mask: returnedMask, Mode: mode, Ino: inode, Size: size,
			Dev_major: linuxTestDeviceMajor, Dev_minor: linuxTestDeviceMinor,
			Mnt_id: linuxTestUniqueMountID, Uid: harness.ownerUID,
			Mtime: unix.StatxTimestamp{Sec: harness.seconds, Nsec: harness.nanoseconds},
			Btime: unix.StatxTimestamp{Sec: 1_500_000_000},
		}
		return nil
	}
	system.fstatfs = func(_ int, stat *unix.Statfs_t) error {
		reflect.ValueOf(stat).Elem().FieldByName("Type").SetInt(linuxExt4SuperMagic)
		stat.Fsid.Val = filesystemID
		return nil
	}
	system.getVersion = func(fd int) (uint32, error) {
		if fd == probeFD {
			return linuxTestGeneration + 1, nil
		}
		return linuxTestGeneration, nil
	}
	system.getFlags = func(int) (uint32, error) { return 0, nil }
	system.getFilesystemUUID = func(int) ([linuxFilesystemUUIDBytes]byte, error) {
		return linuxTestFilesystemUUID, nil
	}
	system.restartIdentity = linuxStatxBirthTimeRestartIdentityProvider{}
	system.geteuid = func() int { return int(harness.ownerUID) }
	mount := linuxMountIdentity{
		uniqueMountID: linuxTestUniqueMountID,
		deviceMajor:   linuxTestDeviceMajor, deviceMinor: linuxTestDeviceMinor,
		runtimeFilesystemID: filesystemID,
		filesystemUUID:      linuxTestFilesystemUUID,
	}
	rootObject := linuxOpenHandleIdentity{
		mountID:     linuxTestUniqueMountID,
		deviceMajor: linuxTestDeviceMajor, deviceMinor: linuxTestDeviceMinor,
		inode: linuxTestRootInode, kind: unix.S_IFDIR,
	}
	certificate := linuxOutputCertificate{
		mount:      mount,
		rootObject: rootObject,
		rootRestartIdentity: linuxDirectoryRestartIdentity{
			mount: mount, inode: linuxTestRootInode, kind: unix.S_IFDIR,
			birthSeconds: 1_500_000_000,
			generation:   linuxTestGeneration, hasGenerationProof: true,
		},
		durability: linuxOutputProcessRestartDurability,
	}
	return &linuxOutputDirectory{
		system: system, fd: rootFD, certificate: certificate, object: certificate.rootObject,
	}, harness
}
