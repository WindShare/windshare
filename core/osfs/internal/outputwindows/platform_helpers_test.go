//go:build windows

package outputwindows

import (
	"errors"
	"os"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
	"golang.org/x/sys/windows"
)

func TestWindowsOutputV3PlatformClosedSurfaceRejectsOperations(t *testing.T) {
	var nilPlatform *windowsOutputV3Platform
	closed := &windowsOutputV3Platform{}

	for name, platform := range map[string]*windowsOutputV3Platform{
		"nil":    nilPlatform,
		"closed": closed,
	} {
		t.Run(name, func(t *testing.T) {
			if platform.Root() != nil {
				t.Fatal("closed platform exposed a root")
			}
			if _, err := platform.AcquirePublicOperationGuard(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
				t.Fatalf("closed guard acquisition error = %v", err)
			}
			if _, err := platform.RootBinding(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
				t.Fatalf("closed root binding error = %v", err)
			}
			if err := platform.ProbeRecoverableFeatures(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
				t.Fatalf("closed feature probe error = %v", err)
			}
			if err := platform.ValidateSelectionMetadata(transfer.OutputSelection{}); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
				t.Fatalf("closed metadata validation error = %v", err)
			}
			if err := platform.Close(); err != nil {
				t.Fatalf("closed platform close = %v", err)
			}
		})
	}
}

func TestWindowsOutputV3PlatformStaticContractAndCanonicalWrappers(t *testing.T) {
	platform := &windowsOutputV3Platform{}
	if got := platform.Certification(); got != resumestate.CertificationWindowsNTFSProcessRestart {
		t.Fatalf("certification = %v", got)
	}
	if got := platform.Durability(); got != transfer.DurabilityProcessRestart {
		t.Fatalf("durability = %v", got)
	}

	if err := platform.ValidateModifiedTime(catalog.ModifiedTime{}); err != nil {
		t.Fatalf("absent modified time = %v", err)
	}
	modified, err := catalog.NewModifiedTime(1_700_000_000, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	if err := platform.ValidateModifiedTime(modified); err != nil {
		t.Fatalf("valid modified time = %v", err)
	}

	locator, err := platform.CanonicalLocatorKey("Folder/Readme.txt")
	if err != nil {
		t.Fatalf("canonical locator = %v", err)
	}
	if locator == "" {
		t.Fatal("canonical locator key is empty")
	}
	component, err := platform.CanonicalComponentKey("Readme.txt")
	if err != nil {
		t.Fatalf("canonical component = %v", err)
	}
	if component == "" {
		t.Fatal("canonical component key is empty")
	}

	if _, err := platform.CanonicalLocatorKey("../escape"); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("unsafe locator error = %v", err)
	}
	if _, err := platform.CanonicalComponentKey("../escape"); err == nil {
		t.Fatal("unsafe component was accepted")
	}
}

func TestWindowsOutputV3PublicOperationGuardClosedSurfaceIsIdempotent(t *testing.T) {
	var nilGuard *windowsOutputV3PublicOperationGuard
	for name, guard := range map[string]*windowsOutputV3PublicOperationGuard{
		"nil":   nilGuard,
		"empty": {},
	} {
		t.Run(name, func(t *testing.T) {
			if guard.Root() != nil {
				t.Fatal("closed guard exposed a root")
			}
			if err := guard.Close(); err != nil {
				t.Fatalf("closed guard close = %v", err)
			}
		})
	}
}

func TestWindowsV3DirectoryNameHelpersRejectInvalidOrClosedAuthorities(t *testing.T) {
	var directory *windowsV3Directory
	include := func(string) bool { return true }

	if _, err := directory.namesMatching(-1, include); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("negative enumeration bound error = %v", err)
	}
	if _, err := directory.namesMatching(0, nil); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("nil enumeration filter error = %v", err)
	}
	if _, err := directory.namesMatching(0, include); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("closed enumeration error = %v", err)
	}
	if _, err := directory.namesWithPrefix("Read", 1); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("closed prefix enumeration error = %v", err)
	}
	if err := directory.validatePublicEntryNames(nil); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("closed public-name validation error = %v", err)
	}
	if err := directory.validatePublicEntryName("Readme.txt"); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("closed single-name validation error = %v", err)
	}
}

func TestWindowsV3DirectoryPublicNameValidationAcceptsEmptyPlan(t *testing.T) {
	// The empty plan is a meaningful no-op after a canonical selection has been
	// reduced to zero public entries; it must not force a native directory scan.
	directory := &windowsV3Directory{
		file:      &os.File{},
		inspector: windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) { return windowsV3HandleFacts{}, nil }),
		policy:    &windowsV3PrivatePolicy{},
	}
	if err := directory.validatePublicEntryNames(nil); err != nil {
		t.Fatalf("empty public-name validation = %v", err)
	}
}

func TestWindowsOutputV3DirectoryClosedSurfaceRejectsAllCapabilities(t *testing.T) {
	var directory *windowsOutputV3Directory
	modified := catalog.ModifiedTime{}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := directory.Duplicate(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("duplicate error = %v", err)
	}
	if err := directory.Sync(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("sync error = %v", err)
	}
	if _, err := directory.Names(1); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("names error = %v", err)
	}
	if _, err := directory.NamesWithPrefix("x", 1); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("prefix names error = %v", err)
	}
	if _, err := directory.ObserveEntry("x"); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("observe error = %v", err)
	}
	if _, _, err := directory.ClassifyExactEntry("x"); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("classify error = %v", err)
	}
	for name, err := range map[string]error{
		"validate name":  directory.ValidatePublicEntryName("x"),
		"validate names": directory.ValidatePublicEntryNames(nil),
		"set time":       directory.SetModifiedTime(modified),
	} {
		if !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Errorf("%s error = %v", name, err)
		}
	}
	if _, err := directory.OpenEntry("x"); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("open entry error = %v", err)
	}
	if _, err := directory.EntryMatches("x", nil); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("entry match error = %v", err)
	}
	if _, err := directory.OpenPinnedDirectory(nil, false); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("open pinned directory error = %v", err)
	}
	if err := directory.RemoveEntry("x", nil); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("remove entry error = %v", err)
	}
	if _, err := directory.IdentityClaim(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("identity claim error = %v", err)
	}
	if _, err := directory.PrepareIdentityClaim(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("prepare identity claim error = %v", err)
	}
	if _, err := directory.SameDirectory(nil); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("same directory error = %v", err)
	}
	if _, err := directory.OpenDirectory("x", false); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("open directory error = %v", err)
	}
	if _, err := directory.CreateDirectory("x", false); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("create directory error = %v", err)
	}
	if _, err := directory.InstallDirectoryNoReplace(nil, "x"); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("install directory error = %v", err)
	}
	if err := directory.RemoveDirectory("x", nil); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("remove directory error = %v", err)
	}
	if _, err := directory.CreateFile("x", false, -1); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("create file error = %v", err)
	}
	if _, err := directory.OpenFile("x", false, false); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("open file error = %v", err)
	}
	if _, err := directory.LinkFileNoReplace(nil, "x"); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("link file error = %v", err)
	}
	if err := directory.ReplacePrivateFile(nil, "x"); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("replace file error = %v", err)
	}
	if err := directory.RemoveFile("x", nil); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("remove file error = %v", err)
	}
	if _, _, err := directory.AcquireLock("x", false); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("acquire lock error = %v", err)
	}
}

func TestWindowsOutputV3FileEntryAndLockClosedSurfaceIsSafe(t *testing.T) {
	var file *windowsOutputV3File
	if _, err := file.ReadAt(nil, 0); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("read error = %v", err)
	}
	if _, err := file.WriteAt(nil, 0); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("write error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("sync error = %v", err)
	}
	if err := file.Truncate(-1); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("truncate error = %v", err)
	}
	if _, err := file.Size(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("size error = %v", err)
	}
	if _, err := file.AllocatedSize(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("allocated size error = %v", err)
	}
	if err := file.SetModifiedTime(catalog.ModifiedTime{}); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("modified time error = %v", err)
	}
	if _, err := file.MetadataMatches(0, catalog.ModifiedTime{}); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("metadata match error = %v", err)
	}
	if _, err := file.SameFile(nil); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("same file error = %v", err)
	}

	var entry *windowsOutputV3EntryRef
	if got := entry.Kind(); got != outputcap.EntryAbsent {
		t.Fatalf("closed entry kind = %v", got)
	}
	if _, err := entry.AllocatedSize(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("entry size error = %v", err)
	}
	if err := entry.Close(); err != nil {
		t.Fatal(err)
	}
	var lock *windowsOutputV3Lock
	if lock.File() != nil {
		t.Fatal("closed lock exposed a file")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsOutputV3FileLiveSurfacePreservesPinnedAuthorityThroughSettlement(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	t.Cleanup(func() {
		if err := platform.Close(); err != nil {
			t.Errorf("close platform: %v", err)
		}
	})
	directory := &windowsOutputV3Directory{native: platform.Root()}

	const name = "capability-live-surface.bin"
	created, err := directory.CreateFile(name, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := created.Close(); err != nil {
			t.Errorf("close file: %v", err)
		}
	})
	file, ok := created.(*windowsOutputV3File)
	if !ok {
		t.Fatalf("created file type = %T", created)
	}

	payload := []byte("pinned Windows capability")
	if written, err := file.WriteAt(payload, 0); err != nil || written != len(payload) {
		t.Fatalf("write = %d, %v", written, err)
	}
	if err := file.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if size, err := file.Size(); err != nil || size != uint64(len(payload)) {
		t.Fatalf("size = %d, %v", size, err)
	}
	if _, err := file.AllocatedSize(); err != nil {
		t.Fatalf("allocated size: %v", err)
	}
	read := make([]byte, len(payload))
	if count, err := file.ReadAt(read, 0); err != nil || count != len(payload) || string(read) != string(payload) {
		t.Fatalf("read = %d, %q, %v", count, read, err)
	}

	modified, err := catalog.NewModifiedTime(1_700_000_000, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.SetModifiedTime(modified); err != nil {
		t.Fatalf("set modified time: %v", err)
	}
	if matches, err := file.MetadataMatches(uint64(len(payload)), modified); err != nil || !matches {
		t.Fatalf("metadata match = %t, %v", matches, err)
	}

	opened, err := directory.OpenFile(name, true, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := opened.Close(); err != nil {
			t.Errorf("close peer: %v", err)
		}
	})
	peer, ok := opened.(*windowsOutputV3File)
	if !ok {
		t.Fatalf("opened file type = %T", opened)
	}
	if same, err := file.SameFile(peer); err != nil || !same {
		t.Fatalf("same file = %t, %v", same, err)
	}
	identity, err := file.CloseRevalidationIdentity()
	if err != nil || identity.IsZero() {
		t.Fatalf("primary close identity is zero: %v", err)
	}
	peerIdentity, err := peer.CloseRevalidationIdentity()
	if err != nil || !identity.Equal(peerIdentity) {
		t.Fatalf("peer close identity differs: %v", err)
	}
	if err := peer.Close(); err != nil {
		t.Fatalf("close peer: %v", err)
	}

	const truncatedSize = 7
	if err := file.Truncate(truncatedSize); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if size, err := file.Size(); err != nil || size != truncatedSize {
		t.Fatalf("truncated size = %d, %v", size, err)
	}
	// Unlinking while the pinned handle remains live proves settlement does not
	// silently exchange name authority for a later path re-resolution.
	if err := directory.RemoveFile(name, file); err != nil {
		t.Fatalf("remove pinned file: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close unlinked file: %v", err)
	}
	if file.native != nil {
		t.Fatal("closed wrapper retained native authority")
	}
	if err := file.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}
