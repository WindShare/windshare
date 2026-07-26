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
		root.object.mode = harness.directoryMode
		root.certificate.rootObject.mode = harness.directoryMode
		_, err := root.identityClaim()
		if !errors.Is(err, errOutputAncestryAuthorityDenied) ||
			outputAncestryTraceDecision(err) != FilesystemOutputAncestryAuthorityDenied {
			t.Fatalf("authority trace classification = %v, %v", outputAncestryTraceDecision(err), err)
		}
	})

	t.Run("identity contradiction", func(t *testing.T) {
		root, _ := newLinuxSelectionMetadataRoot(t)
		installLinuxSafeAuthorityHarness(root.system)
		root.system.getVersion = func(int) (uint32, error) { return 0, nil }
		_, err := root.identityClaim()
		if err == nil || errors.Is(err, errOutputAncestryAuthorityDenied) ||
			outputAncestryTraceDecision(err) != FilesystemOutputAncestryStructuralUnsafe {
			t.Fatalf("identity trace classification = %v, %v", outputAncestryTraceDecision(err), err)
		}
	})
}
