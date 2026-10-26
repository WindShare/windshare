//go:build linux

package outputlinux

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"github.com/windshare/windshare/osfs/internal/outputprobe"
	"golang.org/x/sys/unix"
)

func TestLinuxProbeRollbackFailurePreservesRestartCut(t *testing.T) {
	rootPath := t.TempDir()
	system := linuxHostOutputSystem
	root, err := linuxOpenOutputRoot(rootPath, &system)
	if err != nil {
		t.Fatal(err)
	}
	defer root.close()
	rootCapability := &linuxDirectory{native: root}
	const name = ".windshare-output.probe-00112233445566778899aabbccddeeff"
	directory, err := rootCapability.CreateDirectory(name, true)
	if err != nil {
		t.Fatal(err)
	}
	workspace := outputprobe.NewWorkspace(rootCapability, name, directory)
	defer workspace.Close()
	if err := workspace.SafePublish(outputprobe.LinuxExt4); err != nil {
		t.Fatal(err)
	}
	openat2, fsync, unlinkat := system.openat2, system.fsync, system.unlinkat
	temporaryFD := -1
	createFailed, rollbackFailed := false, false
	system.openat2 = func(parent int, relative string, how *unix.OpenHow) (int, error) {
		fd, err := openat2(parent, relative, how)
		if err == nil && relative == outputprobe.TemporaryRecordName && how.Flags&unix.O_CREAT != 0 {
			temporaryFD = fd
		}
		return fd, err
	}
	system.fsync = func(fd int) error {
		if fd == temporaryFD && !createFailed {
			createFailed = true
			return unix.EIO
		}
		return fsync(fd)
	}
	system.unlinkat = func(parent int, relative string, flags int) error {
		if relative == outputprobe.TemporaryRecordName && !rollbackFailed {
			rollbackFailed = true
			return unix.EBUSY
		}
		return unlinkat(parent, relative, flags)
	}
	proofErr := workspace.RangeRecovery()
	system.openat2, system.fsync, system.unlinkat = openat2, fsync, unlinkat
	if !createFailed || !rollbackFailed || !errors.Is(proofErr, unix.EIO) || !errors.Is(proofErr, unix.EBUSY) {
		t.Fatalf("create failed=%t rollback failed=%t error=%v", createFailed, rollbackFailed, proofErr)
	}
	if err := workspace.Cleanup(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("cleanup without a returned witness=%v", err)
	}
	for _, entry := range []string{outputprobe.RecordName, outputprobe.TemporaryRecordName} {
		if _, err := os.Stat(filepath.Join(rootPath, name, entry)); err != nil {
			t.Fatalf("cleanup lost the restart cut at %s: %v", entry, err)
		}
	}
	if err := outputprobe.Recover(rootCapability, []string{name}, outputprobe.LinuxExt4); err != nil {
		t.Fatalf("restart recovery after transient create and rollback failures: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, name)); !os.IsNotExist(err) {
		t.Fatalf("recovered probe remains: %v", err)
	}
}
