//go:build windows

package outputwindows

import (
	"errors"
	"io"
	"reflect"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"golang.org/x/sys/windows"
)

func TestWindowsRecoveryDurabilityOrdinaryStageSync(t *testing.T) {
	requireUnprivilegedWindowsNTFSCertification(t)
	_, guard := windowsV3OpenGuardedTestRoot(t)
	root := &windowsOutputV3Directory{native: guard.Root()}

	const stageName = "recovery-durability-stage.bin"
	created, err := root.CreateFile(stageName, true, 4)
	if err != nil {
		t.Fatal(err)
	}
	assertWindowsFileAccessMethods(t, created, []string{
		"Close", "MetadataMatches", "ReadAt", "SameFile", "SetModifiedTime", "Size", "Sync", "WriteAt",
	})
	if written, writeErr := created.WriteAt([]byte("data"), 0); writeErr != nil || written != 4 {
		t.Fatalf("write ordinary stage = %d, %v", written, writeErr)
	}
	if err := errors.Join(created.Sync(), created.Close()); err != nil {
		t.Fatal(err)
	}

	observed, err := root.OpenObservedFile(stageName, true)
	if err != nil {
		t.Fatal(err)
	}
	defer observed.Close()
	recovery, err := root.OpenRecoveryDurabilityFile(stageName, true)
	if err != nil {
		t.Fatal(err)
	}
	defer recovery.Close()

	if size, sizeErr := recovery.Size(); sizeErr != nil || size != 4 {
		t.Fatalf("recovery size = %d, %v", size, sizeErr)
	}
	if same, sameErr := observed.SameFile(recovery); sameErr != nil || !same {
		t.Fatalf("observed/recovery identity = %t, %v", same, sameErr)
	}
	if err := recovery.Sync(); err != nil {
		t.Fatalf("real FlushFileBuffers through recovery authority: %v", err)
	}

	assertWindowsObservedFileMethodSet(t, observed)
	assertWindowsRecoveryFileMethodSet(t, recovery)
	assertWindowsFileAccessMethods(t, observed, []string{"Close", "MetadataMatches", "ReadAt", "SameFile", "Size"})
	assertWindowsFileAccessMethods(t, recovery, []string{"Close", "SameFile", "Size", "Sync"})
}

func assertWindowsFileAccessMethods(t *testing.T, file any, want []string) {
	t.Helper()
	access := reflect.TypeOf(file)
	got := make([]string, 0, access.NumMethod())
	for method := range access.Methods() {
		got = append(got, method.Name)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%T method set = %v, want %v", file, got, want)
	}
}

func assertWindowsObservedFileMethodSet(t *testing.T, file outputcap.ObservedFile) {
	t.Helper()
	if _, ok := any(file).(io.WriterAt); ok {
		t.Fatal("observed wrapper exposed data mutation")
	}
	if _, ok := any(file).(outputcap.RecoveryDurabilityFile); ok {
		t.Fatal("observed wrapper exposed durability mutation")
	}
	if _, ok := any(file).(outputcap.MutableFile); ok {
		t.Fatal("observed wrapper widened to mutable authority")
	}
}

func assertWindowsRecoveryFileMethodSet(t *testing.T, file outputcap.RecoveryDurabilityFile) {
	t.Helper()
	if _, ok := any(file).(io.ReaderAt); ok {
		t.Fatal("recovery wrapper exposed content observation")
	}
	if _, ok := any(file).(io.WriterAt); ok {
		t.Fatal("recovery wrapper exposed data mutation")
	}
	if _, ok := any(file).(outputcap.ObservedFile); ok {
		t.Fatal("recovery wrapper widened to observation authority")
	}
	if _, ok := any(file).(outputcap.MutableFile); ok {
		t.Fatal("recovery wrapper widened to mutable authority")
	}
}

func TestWindowsRecoveryDurabilityFileAccessMask(t *testing.T) {
	mask := windowsV3RecoveryDurabilityFileAccess()
	for name, required := range map[string]uint32{
		"append":          windows.FILE_APPEND_DATA,
		"read attributes": windows.FILE_READ_ATTRIBUTES,
		"read control":    windows.READ_CONTROL,
		"synchronize":     windows.SYNCHRONIZE,
	} {
		if mask&required != required {
			t.Errorf("recovery mask %#x lacks %s (%#x)", mask, name, required)
		}
	}
	for name, forbidden := range map[string]uint32{
		"arbitrary write":  windows.FILE_WRITE_DATA,
		"generic write":    windows.GENERIC_WRITE,
		"delete":           windows.DELETE,
		"write attributes": windows.FILE_WRITE_ATTRIBUTES,
	} {
		if mask&forbidden != 0 {
			t.Errorf("recovery mask %#x includes %s (%#x)", mask, name, forbidden)
		}
	}
}

func TestWindowsErrorTaxonomyFreezesClosedNativeClass(t *testing.T) {
	tests := []struct {
		name  string
		cause error
		class outputcap.NativeErrorClass
	}{
		{name: "access denied", cause: windows.ERROR_ACCESS_DENIED, class: outputcap.NativeErrorAccessDenied},
		{name: "sharing violation", cause: windows.ERROR_SHARING_VIOLATION, class: outputcap.NativeErrorSharingViolation},
		{name: "not found", cause: windows.ERROR_FILE_NOT_FOUND, class: outputcap.NativeErrorNotFound},
		{name: "invalid handle", cause: windows.ERROR_INVALID_HANDLE, class: outputcap.NativeErrorInvalidHandle},
		{name: "unsupported", cause: windows.ERROR_NOT_SUPPORTED, class: outputcap.NativeErrorUnsupported},
		{name: "io", cause: windows.ERROR_CRC, class: outputcap.NativeErrorIO},
		{name: "unknown", cause: errors.New("provider path and detail canary"), class: outputcap.NativeErrorUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := windowsV3NativeOperationFailure("native operation canary", "path canary", test.cause)
			class, ok := outputcap.ClassifyNativeError(windowsOutputV3Error(err))
			if !ok || class != test.class {
				t.Fatalf("class = %v, %t, want %v", class, ok, test.class)
			}
			if class.String() == "native operation canary" || class.String() == "path canary" ||
				class.String() == test.cause.Error() {
				t.Fatalf("closed class exposed provider text: %q", class)
			}
		})
	}
}
