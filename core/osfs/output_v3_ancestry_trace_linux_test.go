//go:build linux

package osfs

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLinuxOutputAncestryTraceSeparatesAuthorityFromIdentityContradictions(t *testing.T) {
	t.Run("authority denied", func(t *testing.T) {
		root, harness := newLinuxSelectionMetadataRoot(t)
		installLinuxSafeAuthorityHarness(root.system)
		harness.directoryMode = uint16(unix.S_IFDIR | 0o770)
		_, err := root.identityClaim()
		if !errors.Is(err, errOutputAncestryAuthorityDenied) ||
			outputAncestryTraceDecision(err) != FilesystemOutputAncestryAuthorityDenied {
			t.Fatalf("authority trace classification = %v, %v", outputAncestryTraceDecision(err), err)
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
		_, err := root.identityClaim()
		if err == nil || errors.Is(err, errOutputAncestryAuthorityDenied) ||
			outputAncestryTraceDecision(err) != FilesystemOutputAncestryStructuralUnsafe {
			t.Fatalf("identity trace classification = %v, %v", outputAncestryTraceDecision(err), err)
		}
	})
}
