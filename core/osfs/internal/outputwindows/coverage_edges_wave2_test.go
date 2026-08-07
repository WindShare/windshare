//go:build windows

package outputwindows

import (
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"golang.org/x/sys/windows"
)

func TestWindowsV3Wave2ClosedPrivateIdentityAndPureGuards(t *testing.T) {
	var directory *windowsOutputV3Directory
	if _, err := directory.PreparePrivateIdentityClaim(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("closed private identity preparation = %v", err)
	}
	if _, err := directory.PrivateIdentityClaim(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("closed private identity claim = %v", err)
	}

	validProbe := windowsV3OutputProbePrefix + strings.Repeat("a", windowsV3OutputProbeRandomBytes*2)
	for name, value := range map[string]struct {
		probe string
		want  bool
	}{
		"valid":        {probe: validProbe, want: true},
		"wrong prefix": {probe: "other-" + strings.Repeat("a", windowsV3OutputProbeRandomBytes*2)},
		"wrong length": {probe: windowsV3OutputProbePrefix + "a"},
		"upper hex":    {probe: windowsV3OutputProbePrefix + strings.Repeat("A", windowsV3OutputProbeRandomBytes*2)},
		"non hex":      {probe: windowsV3OutputProbePrefix + strings.Repeat("g", windowsV3OutputProbeRandomBytes*2)},
	} {
		t.Run("probe/"+name, func(t *testing.T) {
			if got := windowsV3CanonicalProbeName(value.probe); got != value.want {
				t.Fatalf("canonical probe = %t, want %t", got, value.want)
			}
		})
	}

	for name, test := range map[string]struct {
		requested, normalized, opened string
		caseSensitive                 bool
		want                          bool
	}{
		"exact case-sensitive":     {"Name", "Name", "Other", true, true},
		"opened case-sensitive":    {"Name", "Other", "Name", true, true},
		"mismatch case-sensitive":  {"Name", "name", "NAME", true, false},
		"ordinal case-insensitive": {"Name", "name", "other", false, true},
		"ordinal mismatch":         {"Name", "other", "else", false, false},
	} {
		t.Run("leaf/"+name, func(t *testing.T) {
			got, err := windowsV3PlacementLeafNamesMatch(test.requested, test.normalized, test.opened, test.caseSensitive)
			if err != nil || got != test.want {
				t.Fatalf("leaf match = %t, %v; want %t", got, err, test.want)
			}
		})
	}
}

func TestWindowsV3Wave2ModifiedTimeBoundaries(t *testing.T) {
	if ticks, present, err := windowsV3ModifiedTimeTicks(catalog.ModifiedTime{}); err != nil || present || ticks != 0 {
		t.Fatalf("absent modified time = %d/%t/%v", ticks, present, err)
	}
	misaligned, err := catalog.NewModifiedTime(1, 1, catalog.TimePrecisionNanoseconds)
	if err != nil {
		t.Fatal(err)
	}
	if err := windowsV3ValidateModifiedTime(misaligned); err == nil {
		t.Fatal("non-FILETIME-aligned nanoseconds accepted")
	}
	preEpoch, err := catalog.NewModifiedTime(-20_000_000_000, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	if err := windowsV3ValidateModifiedTime(preEpoch); err == nil {
		t.Fatal("pre-NTFS epoch timestamp accepted")
	}
	overflow, err := catalog.NewModifiedTime(catalog.MaxSafeInteger, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	if err := windowsV3ValidateModifiedTime(overflow); err == nil {
		t.Fatal("overflow timestamp accepted")
	}
	valid, err := catalog.NewModifiedTime(1_700_000_000, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	if ticks, present, err := windowsV3ModifiedTimeTicks(valid); err != nil || !present || ticks == 0 {
		t.Fatalf("valid modified time = %d/%t/%v", ticks, present, err)
	}
}

func TestWindowsV3Wave2NativeGuardsFailBeforeNativeMutation(t *testing.T) {
	inspectErr := errors.New("synthetic inspector failure")
	native := &windowsV3Directory{
		file: os.NewFile(1, "wave2-directory"),
		path: `C:\wave2-directory`,
		inspector: windowsV3HandleInspectorFunc(func(windows.Handle) (windowsV3HandleFacts, error) {
			return windowsV3HandleFacts{}, inspectErr
		}),
		enumerate: &sync.Mutex{},
	}
	defer native.file.Close()

	if err := native.setModifiedTime(catalog.ModifiedTime{}); err == nil {
		t.Fatal("directory modified-time mutation crossed a failed identity inspection")
	}
	if err := native.scanPublicEntryAuthorities(map[string]*windowsV3PublicEntryAuthority{}); err == nil {
		t.Fatal("public-name scan crossed a failed identity inspection")
	}
}

func TestWindowsV3Wave2LiveAdapterPreservesCapabilityAuthority(t *testing.T) {
	native := openWindowsV3TestPlatform(t)
	platform := &windowsOutputV3Platform{
		native: native,
		root:   &windowsOutputV3Directory{native: native.Root()},
	}
	t.Cleanup(func() {
		if err := platform.Close(); err != nil {
			t.Errorf("close platform: %v", err)
		}
	})
	if _, err := platform.RootBinding(); err != nil {
		t.Fatalf("root binding: %v", err)
	}
	if err := platform.ProbeRecoverableFeatures(); err != nil {
		t.Fatalf("recoverable feature probe: %v", err)
	}
	modified, err := catalog.NewModifiedTime(1_700_000_000, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	selection := windowsSelectionMetadataSelection(t, []windowsSelectionMetadataFile{{size: 8, modified: modified}})
	if err := platform.ValidateSelectionMetadata(selection); err != nil {
		t.Fatalf("selection metadata: %v", err)
	}

	operationGuard, err := platform.AcquirePublicOperationGuard()
	if err != nil {
		t.Fatalf("public operation guard: %v", err)
	}
	t.Cleanup(func() {
		if err := operationGuard.Close(); err != nil {
			t.Errorf("close retained public operation guard: %v", err)
		}
	})
	root, ok := operationGuard.Root().(*windowsOutputV3Directory)
	if !ok || root == nil {
		t.Fatal("public operation guard omitted its root authority")
	}
	if err := root.native.probeRecoverableFeatures(); err != nil {
		t.Fatalf("retained-root feature probe: %v", err)
	}
	transientGuard, err := platform.AcquirePublicOperationGuard()
	if err != nil {
		t.Fatalf("transient public operation guard: %v", err)
	}
	if err := transientGuard.Close(); err != nil || transientGuard.Root() != nil {
		t.Fatalf("settled public operation guard retained authority: %v", err)
	}

	if err := root.Sync(); err != nil {
		t.Fatalf("sync root: %v", err)
	}
	if _, err := root.PrepareIdentityClaim(); err != nil && !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("prepare root identity: %v", err)
	}
	if _, err := root.IdentityClaim(); err != nil && !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("read root identity: %v", err)
	}
	duplicate, err := root.Duplicate()
	if err != nil {
		t.Fatalf("duplicate root: %v", err)
	}
	if same, err := root.SameDirectory(duplicate); err != nil || !same {
		t.Fatalf("duplicate root identity = %t, %v", same, err)
	}
	if err := duplicate.Close(); err != nil {
		t.Fatalf("close duplicate root: %v", err)
	}
	if err := root.SetModifiedTime(catalog.ModifiedTime{}); err != nil {
		t.Fatalf("no-op root modified time: %v", err)
	}

	const publicFileName = "wave2-public.bin"
	publicFile, err := root.CreateFile(publicFileName, false, 8)
	if err != nil {
		t.Fatalf("create public file: %v", err)
	}
	if err := publicFile.Sync(); err != nil {
		t.Fatalf("sync public file: %v", err)
	}
	if err := root.ValidatePublicEntryNames([]string{publicFileName, "wave2-missing.bin"}); err != nil {
		t.Fatalf("validate public names: %v", err)
	}
	if err := root.ValidatePublicEntryName(publicFileName); err != nil {
		t.Fatalf("validate one public name: %v", err)
	}
	if names, err := root.Names(1024); err != nil || !wave2HasName(names, publicFileName) {
		t.Fatalf("root names = %v, %v", names, err)
	}
	if names, err := root.NamesWithPrefix("wave2-", 1024); err != nil || !wave2HasName(names, publicFileName) {
		t.Fatalf("root prefix names = %v, %v", names, err)
	}
	if kind, err := root.ObserveEntry(publicFileName); err != nil || kind != outputcap.EntryRegularFile {
		t.Fatalf("public file observation = %v, %v", kind, err)
	}
	if kind, exact, err := root.ClassifyExactEntry(publicFileName); err != nil || !exact || kind != outputcap.EntryRegularFile {
		t.Fatalf("public file classification = %v/%t, %v", kind, exact, err)
	}
	pinned, err := root.OpenEntry(publicFileName)
	if err != nil {
		t.Fatalf("pin public file: %v", err)
	}
	if pinned.Kind() != outputcap.EntryRegularFile {
		t.Fatalf("pinned public file kind = %v", pinned.Kind())
	}
	if _, err := pinned.AllocatedSize(); err != nil {
		t.Fatalf("pinned allocation: %v", err)
	}
	if matches, err := root.EntryMatches(publicFileName, pinned); err != nil || !matches {
		t.Fatalf("pinned public file match = %t, %v", matches, err)
	}
	if err := pinned.Close(); err != nil {
		t.Fatalf("close public file pin: %v", err)
	}
	if err := root.RemoveFile(publicFileName, publicFile); err != nil {
		t.Fatalf("remove public file: %v", err)
	}
	if err := publicFile.Close(); err != nil {
		t.Fatalf("close removed public file: %v", err)
	}
	const entryFileName = "wave2-entry.bin"
	entryFile, err := root.CreateFile(entryFileName, false, 1)
	if err != nil {
		t.Fatalf("create entry-removal file: %v", err)
	}
	entryPin, err := root.OpenEntry(entryFileName)
	if err != nil {
		t.Fatalf("pin entry-removal file: %v", err)
	}
	if err := root.RemoveEntry(entryFileName, entryPin); err != nil {
		t.Fatalf("remove pinned entry: %v", err)
	}
	if err := entryPin.Close(); err != nil {
		t.Fatalf("close removed entry pin: %v", err)
	}
	if err := entryFile.Close(); err != nil {
		t.Fatalf("close removed entry file: %v", err)
	}

	const publicDirectoryName = "wave2-public-directory"
	publicDirectory, err := root.CreateDirectory(publicDirectoryName, false)
	if err != nil {
		t.Fatalf("create public directory: %v", err)
	}
	openedPublic, err := root.OpenDirectory(publicDirectoryName, false)
	if err != nil {
		t.Fatalf("open public directory: %v", err)
	}
	pinnedDirectory, err := root.OpenEntry(publicDirectoryName)
	if err != nil {
		t.Fatalf("pin public directory: %v", err)
	}
	openedPinned, err := root.OpenPinnedDirectory(pinnedDirectory, false)
	if err != nil {
		t.Fatalf("open pinned public directory: %v", err)
	}
	if same, err := publicDirectory.SameDirectory(openedPublic); err != nil || !same {
		t.Fatalf("public directory identity = %t, %v", same, err)
	}
	if err := openedPinned.Close(); err != nil {
		t.Fatalf("close pinned public directory: %v", err)
	}
	if err := pinnedDirectory.Close(); err != nil {
		t.Fatalf("close public directory pin: %v", err)
	}
	if err := openedPublic.Close(); err != nil {
		t.Fatalf("close opened public directory: %v", err)
	}
	publicNative, ok := publicDirectory.(*windowsOutputV3Directory)
	if !ok {
		t.Fatalf("public directory capability = %T", publicDirectory)
	}
	publicPath := publicNative.native.path
	publicDuplicate, err := publicDirectory.Duplicate()
	if err != nil {
		t.Fatalf("duplicate public directory: %v", err)
	}
	if err := publicDirectory.Close(); err != nil {
		t.Fatalf("close original public directory: %v", err)
	}
	if err := root.RemoveDirectory(publicDirectoryName, publicDuplicate); err != nil {
		// Public handles intentionally do not request delete sharing. Preserve the
		// observed capability behavior while still removing this test-only leaf.
		_ = publicDuplicate.Close()
		if removeErr := os.Remove(publicPath); removeErr != nil {
			t.Fatalf("remove public directory: %v (fallback %v)", err, removeErr)
		}
	}
	if err := publicDuplicate.Close(); err != nil {
		t.Fatalf("close removed public directory: %v", err)
	}

	const privateDirectoryName = "wave2-private"
	privateDirectory, err := root.CreateDirectory(privateDirectoryName, true)
	if err != nil {
		t.Fatalf("create private directory: %v", err)
	}
	privateWindowsDirectory, ok := privateDirectory.(*windowsOutputV3Directory)
	if !ok {
		t.Fatalf("private directory capability = %T", privateDirectory)
	}
	if _, err := privateWindowsDirectory.PreparePrivateIdentityClaim(); err != nil {
		t.Fatalf("prepare private identity: %v", err)
	}
	if _, err := privateWindowsDirectory.PrivateIdentityClaim(); err != nil {
		t.Fatalf("read private identity: %v", err)
	}
	openedPrivate, err := root.OpenDirectory(privateDirectoryName, true)
	if err != nil {
		t.Fatalf("open private directory: %v", err)
	}
	if same, err := privateDirectory.SameDirectory(openedPrivate); err != nil || !same {
		t.Fatalf("private directory identity = %t, %v", same, err)
	}
	if err := openedPrivate.Close(); err != nil {
		t.Fatalf("close opened private directory: %v", err)
	}
	if err := root.RemoveDirectory(privateDirectoryName, privateDirectory); err != nil {
		t.Fatalf("remove private directory: %v", err)
	}
	if err := privateDirectory.Close(); err != nil {
		t.Fatalf("close removed private directory: %v", err)
	}

	const privateFileName = "wave2-private.bin"
	privateFile, err := root.CreateFile(privateFileName, true, 4)
	if err != nil {
		t.Fatalf("create private file: %v", err)
	}
	privatePayload := []byte("wind")
	if written, err := privateFile.WriteAt(privatePayload, 0); err != nil || written != len(privatePayload) {
		t.Fatalf("write private file = %d, %v", written, err)
	}
	if err := privateFile.Sync(); err != nil {
		t.Fatalf("sync private file: %v", err)
	}
	readBack := make([]byte, len(privatePayload))
	if read, err := privateFile.ReadAt(readBack, 0); err != nil || read != len(readBack) || string(readBack) != string(privatePayload) {
		t.Fatalf("read private file = %d/%q, %v", read, readBack, err)
	}
	if err := privateFile.Truncate(2); err != nil {
		t.Fatalf("truncate private file: %v", err)
	}
	if size, err := privateFile.Size(); err != nil || size != 2 {
		t.Fatalf("private file size = %d, %v", size, err)
	}
	if _, err := privateFile.AllocatedSize(); err != nil {
		t.Fatalf("private file allocation: %v", err)
	}
	if err := privateFile.SetModifiedTime(modified); err != nil {
		t.Fatalf("private file modified time: %v", err)
	}
	if matches, err := privateFile.MetadataMatches(2, modified); err != nil || !matches {
		t.Fatalf("private file metadata = %t, %v", matches, err)
	}
	openedPrivateFile, err := root.OpenFile(privateFileName, true, false)
	if err != nil {
		t.Fatalf("open private file read-only: %v", err)
	}
	if same, err := privateFile.SameFile(openedPrivateFile); err != nil || !same {
		t.Fatalf("private file identity = %t, %v", same, err)
	}
	if err := openedPrivateFile.Close(); err != nil {
		t.Fatalf("close opened private file: %v", err)
	}
	if identityProvider, ok := privateFile.(outputcap.CloseRevalidationIdentityProvider); ok {
		if identity, err := identityProvider.CloseRevalidationIdentity(); err != nil || identity.IsZero() {
			t.Fatalf("private close identity = %v, %v", identity, err)
		}
	}
	if err := privateFile.Close(); err != nil {
		t.Fatalf("close private file: %v", err)
	}
	cleanupPrivateFile, err := root.OpenFile(privateFileName, true, true)
	if err != nil {
		t.Fatalf("reopen private file for cleanup: %v", err)
	}
	if err := root.RemoveFile(privateFileName, cleanupPrivateFile); err != nil {
		t.Fatalf("remove private file: %v", err)
	}
	if err := cleanupPrivateFile.Close(); err != nil {
		t.Fatalf("close cleaned private file: %v", err)
	}

	lock, created, err := root.AcquireLock("wave2.lock", false)
	if err != nil || !created || lock.File() == nil {
		t.Fatalf("create lock = %t/%T, %v", created, lock, err)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("close created lock: %v", err)
	}
	existing, created, err := root.AcquireLock("wave2.lock", true)
	if err != nil || created || existing.File() == nil {
		t.Fatalf("open existing lock = %t/%T, %v", created, existing, err)
	}
	if err := existing.Close(); err != nil {
		t.Fatalf("close existing lock: %v", err)
	}
}

func wave2HasName(names []string, want string) bool {
	return slices.Contains(names, want)
}
