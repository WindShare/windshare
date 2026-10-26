//go:build linux

package outputlinux

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/unix"
)

func TestLinuxFileCreationFailureReleasesReservedOrigin(t *testing.T) {
	for _, failure := range []string{"origin", "inspection", "initialization", "collision"} {
		t.Run(failure, func(t *testing.T) {
			rootPath := t.TempDir()
			system := linuxHostOutputSystem
			root, err := linuxOpenOutputRoot(rootPath, &system)
			if err != nil {
				t.Fatal(err)
			}
			defer root.close()
			const name = "creation-failure"
			path := filepath.Join(rootPath, name)
			const foreignContent = "existing file"
			if failure == "collision" {
				if err := os.WriteFile(path, []byte(foreignContent), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			openat2, closeFD, fsync, statx := system.openat2, system.close, system.fsync, system.statx
			opened := make(map[int]bool)
			createdFD := -1
			creates := 0
			injected := false
			system.openat2 = func(parent int, relative string, how *unix.OpenHow) (int, error) {
				if relative == name && how.Flags&unix.O_CREAT != 0 {
					creates++
				}
				if failure == "origin" && relative == "." {
					injected = true
					return -1, unix.EIO
				}
				fd, err := openat2(parent, relative, how)
				if err == nil {
					opened[fd] = true
					if relative == name && how.Flags&unix.O_CREAT != 0 {
						createdFD = fd
					}
				}
				return fd, err
			}
			system.statx = func(fd int, path string, flags, mask int, stat *unix.Statx_t) error {
				if failure == "inspection" && fd == createdFD && !injected {
					injected = true
					return unix.EIO
				}
				return statx(fd, path, flags, mask, stat)
			}
			system.close = func(fd int) error {
				delete(opened, fd)
				return closeFD(fd)
			}
			system.fsync = func(fd int) error {
				if failure == "initialization" && !injected {
					injected = true
					return unix.EIO
				}
				return fsync(fd)
			}
			file, err := (&linuxDirectory{native: root}).CreateFile(name, false, 1)
			if file != nil {
				defer file.Close()
				t.Fatal("failed creation returned a usable file")
			}
			if failure == "collision" {
				if !errors.Is(err, outputcap.ErrNamespaceCollision) {
					t.Fatalf("collision error=%v", err)
				}
				content, readErr := os.ReadFile(path)
				if readErr != nil || string(content) != foreignContent {
					t.Fatalf("existing file changed: content=%q error=%v", content, readErr)
				}
			} else {
				if !injected || !errors.Is(err, unix.EIO) {
					t.Fatalf("injected=%t creation error=%v", injected, err)
				}
				if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
					t.Fatalf("failed creation left an entry: %v", statErr)
				}
			}
			if failure == "origin" && creates != 0 {
				t.Fatal("file creation preceded origin reservation")
			}
			if len(opened) != 0 {
				t.Fatalf("failed creation leaked %d native handles", len(opened))
			}
		})
	}
}
