//go:build linux

package outputlinux

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"golang.org/x/sys/unix"
)

func TestLinuxDirectoryOpenRejectsNestedMountIdentity(t *testing.T) {
	requireUnprivilegedLinuxExt4Certification(t)
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "nested"), linuxOutputDirectoryMode); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(rootPath, false)
	if errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) {
		t.Skipf("test volume is outside the certified Linux/ext4 profile: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	platform := opened.(*linuxV3Platform)
	defer platform.Close()

	system := *platform.root.native.system
	hostOpenat2 := system.openat2
	hostStatx := system.statx
	nestedFD := -1
	mismatchCalls := 0
	system.openat2 = func(dirfd int, path string, how *unix.OpenHow) (int, error) {
		fd, openErr := hostOpenat2(dirfd, path, how)
		if openErr == nil && path == "nested" {
			if how.Resolve&uint64(unix.RESOLVE_NO_XDEV) == 0 {
				t.Fatal("selected-parent open omitted RESOLVE_NO_XDEV")
			}
			nestedFD = fd
		}
		return fd, openErr
	}
	system.statx = func(fd int, path string, flags int, mask int, stat *unix.Statx_t) error {
		if statErr := hostStatx(fd, path, flags, mask, stat); statErr != nil {
			return statErr
		}
		if fd == nestedFD && path == "" && mask&unix.STATX_MNT_ID_UNIQUE != 0 {
			stat.Mnt_id = platform.root.native.certificate.mount.uniqueMountID + 1
			mismatchCalls++
		}
		return nil
	}
	platform.root.native.system = &system

	directory, err := platform.Root().OpenDirectory("nested", false)
	if directory != nil {
		_ = directory.Close()
	}
	if !errors.Is(err, outputcap.ErrUnsafeNamespace) || mismatchCalls == 0 {
		t.Fatalf("nested mount identity = %v, mismatch checks=%d", err, mismatchCalls)
	}
}
