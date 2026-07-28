//go:build linux

package outputlinux

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"golang.org/x/sys/unix"
)

func TestLinuxDirectoryIdentitySeparatesAuthorityDenialFromIdentityContradiction(t *testing.T) {
	t.Run("authority denied", func(t *testing.T) {
		root, harness := newLinuxSelectionMetadataRoot(t)
		installLinuxSafeAuthorityHarness(root.system)
		harness.directoryMode = uint16(unix.S_IFDIR | 0o770)
		_, err := (&linuxV3Directory{native: root}).IdentityClaim()
		if !errors.Is(err, outputfault.ErrAncestryAuthorityDenied) ||
			!errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("authority denial lost its backend taxonomy: %v", err)
		}
	})

	t.Run("identity contradiction", func(t *testing.T) {
		root, _ := newLinuxSelectionMetadataRoot(t)
		installLinuxSafeAuthorityHarness(root.system)
		originalStatx := root.system.statx
		root.system.statx = func(fd int, path string, flags int, mask int, stat *unix.Statx_t) error {
			if err := originalStatx(fd, path, flags, mask, stat); err != nil {
				return err
			}
			if mask&unix.STATX_BTIME != 0 {
				stat.Btime.Sec++
			}
			return nil
		}
		_, err := (&linuxV3Directory{native: root}).IdentityClaim()
		if err == nil || !errors.Is(err, outputcap.ErrUnsafeNamespace) ||
			errors.Is(err, outputfault.ErrAncestryAuthorityDenied) {
			t.Fatalf("identity contradiction lost its backend taxonomy: %v", err)
		}
	})
}
