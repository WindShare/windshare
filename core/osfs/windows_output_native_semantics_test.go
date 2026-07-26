//go:build windows

package osfs

import (
	"bytes"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"golang.org/x/sys/windows"
)

func TestWindowsV3NativeUTF16NameAdmissionIsScalarExact(t *testing.T) {
	for _, test := range []struct {
		name  string
		units []uint16
		want  bool
	}{
		{name: "empty", want: true},
		{name: "BMP", units: []uint16{'a', 0x20ac}, want: true},
		{name: "surrogate pair", units: []uint16{0xd83d, 0xde00}, want: true},
		{name: "embedded NUL", units: []uint16{'a', 0, 'b'}},
		{name: "trailing high surrogate", units: []uint16{'a', 0xd800}},
		{name: "high surrogate before BMP", units: []uint16{0xd800, 'a'}},
		{name: "high surrogate before high surrogate", units: []uint16{0xd800, 0xd801}},
		{name: "lone low surrogate", units: []uint16{0xdc00}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := windowsV3ValidUTF16Name(test.units); got != test.want {
				t.Fatalf("valid UTF-16 = %t, want %t", got, test.want)
			}
		})
	}
}

func TestWindowsV3UnsupportedNativeErrorTaxonomySurvivesWrapping(t *testing.T) {
	for _, native := range []error{
		windows.ERROR_INVALID_PARAMETER,
		windows.ERROR_NOT_SUPPORTED,
		windows.ERROR_CALL_NOT_IMPLEMENTED,
	} {
		if !windowsV3IsUnsupportedNative(errors.Join(errors.New("context"), native)) {
			t.Fatalf("wrapped native error %v was not classified as unsupported", native)
		}
	}
	for _, native := range []error{nil, windows.ERROR_ACCESS_DENIED, windows.ERROR_FILE_EXISTS} {
		if windowsV3IsUnsupportedNative(native) {
			t.Fatalf("native error %v was misclassified as unsupported", native)
		}
	}
}

func TestWindowsV3NativePathAndCreationStatusAreExact(t *testing.T) {
	for _, test := range []struct {
		name string
		path string
		want string
	}{
		{name: "volume GUID UNC", path: `\\?\UNC\server\share\dir`, want: `\??\UNC\server\share\dir`},
		{name: "volume GUID drive", path: `\\?\C:\dir`, want: `\??\C:\dir`},
		{name: "UNC", path: `\\server\share\dir`, want: `\??\UNC\server\share\dir`},
		{name: "drive", path: `C:\dir`, want: `\??\C:\dir`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := windowsV3NTPath(test.path); got != test.want {
				t.Fatalf("NT path = %q, want %q", got, test.want)
			}
		})
	}
	if _, err := windowsV3VolumePath("invalid\x00path"); err == nil {
		t.Fatal("volume lookup accepted an embedded NUL")
	}
	handle, status, err := windowsV3OpenNativeWithOptions(
		windows.InvalidHandle, "invalid\x00name", 0, windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE, windows.FILE_ATTRIBUTE_NORMAL, nil, 0, 0,
	)
	if err == nil || handle != windows.InvalidHandle || status != 0 {
		t.Fatalf("embedded-NUL native open = handle %#x status %d error %v", handle, status, err)
	}

	for _, test := range []struct {
		name        string
		disposition uint32
		status      uintptr
		created     bool
		wantError   bool
	}{
		{name: "create created", disposition: windows.FILE_CREATE, status: windowsV3FileCreated, created: true},
		{name: "create opened", disposition: windows.FILE_CREATE, status: windowsV3FileOpened, wantError: true},
		{name: "open opened", disposition: windows.FILE_OPEN, status: windowsV3FileOpened},
		{name: "open created", disposition: windows.FILE_OPEN, status: windowsV3FileCreated, wantError: true},
		{name: "open-if created", disposition: windows.FILE_OPEN_IF, status: windowsV3FileCreated, created: true},
		{name: "open-if opened", disposition: windows.FILE_OPEN_IF, status: windowsV3FileOpened},
		{name: "open-if unknown", disposition: windows.FILE_OPEN_IF, status: 99, wantError: true},
		{name: "unsupported disposition", disposition: windows.FILE_OVERWRITE, status: windowsV3FileOpened, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			created, err := windowsV3CreationStatus(test.disposition, test.status)
			if created != test.created || (err != nil) != test.wantError {
				t.Fatalf("creation status = created %t error %v", created, err)
			}
		})
	}

	statusError := windows.NTStatus(0xc0000034)
	if got := normalizeWindowsV3NTError(statusError); got != statusError.Errno() {
		t.Fatalf("normalized NTSTATUS = %v, want %v", got, statusError.Errno())
	}
	sentinel := errors.New("sentinel")
	if normalizeWindowsV3NTError(nil) != nil || normalizeWindowsV3NTError(sentinel) != sentinel {
		t.Fatal("NT error normalization changed nil or a non-NT error")
	}
}

func TestWindowsV3PrivatePolicyRejectsBroadProtectedACL(t *testing.T) {
	if _, err := (*windowsV3PrivatePolicy)(nil).descriptor(false); err == nil {
		t.Fatal("nil private policy produced a descriptor")
	}
	if err := (*windowsV3PrivatePolicy)(nil).verify(windows.InvalidHandle, false); err == nil {
		t.Fatal("nil private policy verified an object")
	}
	policy, err := newWindowsV3PrivatePolicy()
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []bool{false, true} {
		descriptor, err := policy.descriptor(directory)
		if err != nil || descriptor == nil {
			t.Fatalf("private descriptor directory=%t: descriptor=%v error=%v", directory, descriptor, err)
		}
		control, _, err := descriptor.Control()
		if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
			t.Fatalf("private descriptor directory=%t is not protected: control=%#x error=%v", directory, control, err)
		}
	}

	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	broad, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	handle, _, err := windowsV3OpenNative(
		platform.Root().handle(), "broad-acl.bin", windowsV3PrivateFileAccess(), windows.FILE_CREATE,
		windows.FILE_NON_DIRECTORY_FILE, windows.FILE_ATTRIBUTE_NORMAL, broad,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = windowsV3RemoveHandle(handle)
		_ = windows.CloseHandle(handle)
	}()
	if err := policy.verify(handle, false); err == nil {
		t.Fatal("broad protected Everyone ACL satisfied the private-object policy")
	}
	if err := windowsV3RemoveHandle(handle); err != nil {
		t.Fatal(err)
	}
	if err := windows.CloseHandle(handle); err != nil {
		t.Fatal(err)
	}
	handle = windows.InvalidHandle
	if err := platform.Root().Sync(); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsV3NativeAllocationAndFileMetadataRemainHandleBound(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	root := platform.Root()
	const name = "native-metadata.bin"
	payload := bytes.Repeat([]byte{0x5a}, 8193)
	file, err := root.CreatePrivateFile(name)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	written, err := file.WriteAt(payload, 0)
	if err != nil || written != len(payload) {
		t.Fatalf("write = %d, %v", written, err)
	}
	modified, err := catalog.NewModifiedTime(1_700_000_123, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.setModifiedTime(modified); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	size, err := file.Size()
	if err != nil || size != uint64(len(payload)) {
		t.Fatalf("size = %d, error = %v", size, err)
	}
	allocated, err := file.allocatedSize()
	if err != nil || allocated < size {
		t.Fatalf("allocated size = %d for logical size %d, error = %v", allocated, size, err)
	}
	pinned, err := root.openPinnedEntry(name)
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.close()
	pinnedAllocation, err := pinned.allocatedSize()
	if err != nil || pinnedAllocation != allocated {
		t.Fatalf("pinned allocation = %d, handle allocation = %d, error = %v", pinnedAllocation, allocated, err)
	}
	matches, err := file.metadataMatches(size, modified)
	if err != nil || !matches {
		t.Fatalf("exact metadata match = %t, error = %v", matches, err)
	}
	if matches, err := file.metadataMatches(size+1, modified); err != nil || matches {
		t.Fatalf("wrong-size metadata match = %t, error = %v", matches, err)
	}
	differentTime, err := catalog.NewModifiedTime(1_700_000_124, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	if matches, err := file.metadataMatches(size, differentTime); err != nil || matches {
		t.Fatalf("wrong-time metadata match = %t, error = %v", matches, err)
	}
	if matches, err := file.metadataMatches(size, catalog.ModifiedTime{}); err != nil || !matches {
		t.Fatalf("unspecified-time metadata match = %t, error = %v", matches, err)
	}
	if _, err := (&windowsV3File{}).allocatedSize(); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("closed allocation authority = %v", err)
	}
	if _, err := (&windowsV3File{}).metadataMatches(0, catalog.ModifiedTime{}); !errors.Is(err, errWindowsV3OutputUnsafe) {
		t.Fatalf("closed metadata authority = %v", err)
	}
}

func TestWindowsV3ModifiedTimeMatchingHonorsDeclaredPrecision(t *testing.T) {
	for _, test := range []struct {
		name         string
		nanoseconds  uint32
		precision    catalog.TimePrecision
		withinTicks  uint64
		outsideTicks uint64
	}{
		{
			name: "seconds", precision: catalog.TimePrecisionSeconds,
			withinTicks:  windowsV3FiletimeTicksPerSecond - 1,
			outsideTicks: windowsV3FiletimeTicksPerSecond,
		},
		{
			name: "milliseconds", nanoseconds: 123_000_000, precision: catalog.TimePrecisionMilliseconds,
			withinTicks: 9_999, outsideTicks: 10_000,
		},
		{
			name: "nanoseconds", nanoseconds: 123_456_700, precision: catalog.TimePrecisionNanoseconds,
			outsideTicks: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			modified, err := catalog.NewModifiedTime(1_700_000_000, test.nanoseconds, test.precision)
			if err != nil {
				t.Fatal(err)
			}
			ticks, present, err := windowsV3ModifiedTimeTicks(modified)
			if err != nil || !present {
				t.Fatalf("ticks = %d, present = %t, error = %v", ticks, present, err)
			}
			if !windowsV3ModifiedTimeMatches(ticks+test.withinTicks, modified) {
				t.Fatal("timestamp within declared precision did not match")
			}
			if windowsV3ModifiedTimeMatches(ticks+test.outsideTicks, modified) {
				t.Fatal("timestamp outside declared precision matched")
			}
		})
	}
	if !windowsV3ModifiedTimeMatches(^uint64(0), catalog.ModifiedTime{}) {
		t.Fatal("unspecified modified time constrained native metadata")
	}
}
