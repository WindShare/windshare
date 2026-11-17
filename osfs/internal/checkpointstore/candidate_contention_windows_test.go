//go:build windows

package checkpointstore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/osfs/internal/outputportable"
	"golang.org/x/sys/windows"
)

func TestForeignReadHandleReportsCheckpointContentionWithoutRunningOperation(t *testing.T) {
	directory := t.TempDir()
	platform, err := outputportable.Open(directory, false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	registry, err := OpenOperationRegistry(platform.Root())
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	fixture := ordinaryRegistryFixture(t, 0x31)
	lockPath := filepath.Join(directory, OrdinaryRegistryDirectoryV1, ordinaryLeasesDirectory, activeLockName(fixture.key))
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := windows.UTF16PtrFromString(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	// A foreign reader can deny write sharing without holding a WindShare lease.
	// The resulting busy fault cannot promise that another download will finish.
	handle, err := windows.CreateFile(path, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	foreignReader := os.NewFile(uintptr(handle), lockPath)
	defer foreignReader.Close()
	blocked, lookup, err := registry.BeginActive(fixture.key)
	if err == nil {
		_ = blocked.Close()
		t.Fatal("admitted a download while the foreign handle denies write sharing")
	}
	if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) || lookup.State().Valid() {
		t.Fatalf("foreign handle became an active operation: lookup=%v err=%v", lookup.State(), err)
	}
	value, ok := fault.NormalizeBoundaryError(err).Fault()
	code, checkpoint := value.CheckpointCode()
	if !ok || !checkpoint || code != fault.CheckpointBusy || value.Scope() != fault.ScopeOutputPause {
		t.Fatalf("sharing violation lost its storage classification: %v", value)
	}
	if err := foreignReader.Close(); err != nil {
		t.Fatal(err)
	}
	admission, lookup, err := registry.BeginActive(fixture.key)
	if err != nil {
		t.Fatal(err)
	}
	defer admission.Close()
	if lookup.State() != ActiveLookupNone {
		t.Fatalf("released foreign handle did not allow a new download: %v", lookup.State())
	}
}
