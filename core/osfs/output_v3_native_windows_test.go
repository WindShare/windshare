//go:build windows

package osfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
	"golang.org/x/sys/windows"
)

const (
	windowsNTFSNativeCertificationProfile = "windows-ntfs"
	// Keep the historical probe namespace explicit at the root-native boundary:
	// crash recovery must recognize this persisted pre-cleanup artifact.
	windowsNativeOutputProbePrefix   = ".windshare-output.probe-"
	windowsNativeLegacyStatePrefix   = ".wsresume-output-"
	windowsNativeLegacyJournalSuffix = ".journal"
)

func TestWindowsNTFSNativeCertification(t *testing.T) {
	requireUnprivilegedWindowsNTFSCertification(t)
	platform := openCertifiedNativeOutputForTest(
		t,
		t.TempDir(),
		windowsNTFSNativeCertificationProfile,
		resumestate.CertificationWindowsNTFSProcessRestart,
	)
	if err := platform.Close(); err != nil {
		t.Fatalf("close Windows/NTFS certification authority: %v", err)
	}
}

func TestWindowsNTFSProcessRestartRecovery(t *testing.T) {
	requireUnprivilegedWindowsNTFSCertification(t)
	runNativeOutputProcessRestartRecoveryTest(
		t,
		windowsNTFSNativeCertificationProfile,
		resumestate.CertificationWindowsNTFSProcessRestart,
		windowsNativeOutputProbePrefix+"27182818284590452353602874713526",
		1,
	)
	// CreatePrivateFile is durable before Truncate. Killing at size zero proves
	// recovery accepts that exact pre-truncate cut without broadening later
	// hard-link states to zero-length data objects.
	runNativeOutputProcessRestartRecoveryTest(
		t,
		windowsNTFSNativeCertificationProfile,
		resumestate.CertificationWindowsNTFSProcessRestart,
		windowsNativeOutputProbePrefix+"27182818284590452353602874713526",
		0,
	)
	runNativeOutputSessionProcessRestartRecoveryTest(
		t,
		windowsNTFSNativeCertificationProfile,
		resumestate.CertificationWindowsNTFSProcessRestart,
	)
}

func requireUnprivilegedWindowsNTFSCertification(t *testing.T) {
	t.Helper()
	if !windows.GetCurrentProcessToken().IsElevated() {
		return
	}
	if os.Getenv(nativeOutputCertificationProfileEnvironment) == windowsNTFSNativeCertificationProfile {
		t.Fatal("required Windows/NTFS certification must run as an ordinary non-elevated receiver")
	}
	t.Skip("Windows/NTFS native certification is meaningful only as an ordinary receiver")
}

func TestWindowsNTFSResumeListingIsReadOnlyBeforeRootIdentityBootstrap(t *testing.T) {
	t.Run("empty-root", func(t *testing.T) {
		root := t.TempDir()
		inventory, err := ListResumeState(context.Background(), FilesystemResumeRoot{RootPath: root})
		if err != nil {
			t.Fatalf("list unused NTFS root: %v", err)
		}
		if summaries := inventory.Summaries(); len(summaries) != 0 {
			t.Errorf("unused NTFS root summaries = %+v", summaries)
		}
		if err := inventory.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("legacy-only-root", func(t *testing.T) {
		root := t.TempDir()
		journalName := windowsNativeLegacyStatePrefix +
			strings.Repeat("11", transfer.OutputSessionIdentityBytes) + windowsNativeLegacyJournalSuffix
		if err := os.WriteFile(
			filepath.Join(root, journalName), []byte("historic-v2-journal"), 0o600,
		); err != nil {
			t.Fatal(err)
		}
		inventory, err := ListResumeState(context.Background(), FilesystemResumeRoot{RootPath: root})
		if err != nil {
			t.Fatalf("list legacy-only NTFS root: %v", err)
		}
		defer inventory.Close()
		summaries := inventory.Summaries()
		if len(summaries) != 1 || summaries[0].Reference.Kind() != ResumeStateLegacyUntrusted ||
			windowsNativeHasAttention(summaries[0], "legacy-v2-root-pin-unavailable") {
			t.Fatalf("legacy-only NTFS summaries = %+v", summaries)
		}
	})
}

func windowsNativeHasAttention(summary ResumeStateSummary, expected string) bool {
	for _, attention := range summary.Attention {
		if attention.Code == expected {
			return true
		}
	}
	return false
}

func TestWindowsNTFSCreatesMissingRootThroughCertifiedHandles(t *testing.T) {
	target := filepath.Join(t.TempDir(), "first", "second")
	platform, err := openOutputV3Platform(target, true)
	if err != nil {
		nativeOutputCertificationFailure(t, windowsNTFSNativeCertificationProfile,
			"create missing Windows/NTFS root through certified handles", err)
		return
	}
	defer func() {
		if err := platform.Close(); err != nil {
			t.Errorf("close handle-created Windows/NTFS root: %v", err)
		}
	}()
	if err := platform.ProbeRecoverableFeatures(); err != nil {
		t.Fatalf("prepare persistent Windows/NTFS root identity: %v", err)
	}
	if binding, err := platform.RootBinding(); err != nil || binding.IsZero() {
		t.Fatalf("bind handle-created Windows/NTFS root: zero=%t err=%v", binding.IsZero(), err)
	}
}

func TestWindowsRootCreationDoesNotTraverseReparseAncestor(t *testing.T) {
	carrier := t.TempDir()
	outside := t.TempDir()
	alias := filepath.Join(carrier, "alias")
	if err := os.Symlink(outside, alias); err != nil {
		t.Skipf("create Windows reparse adversary: %v", err)
	}
	unexpected := filepath.Join(outside, "must-not-exist")
	platform, err := openOutputV3Platform(filepath.Join(alias, "must-not-exist"), true)
	if platform != nil {
		_ = platform.Close()
	}
	if err == nil {
		t.Fatal("root creation traversed a reparse ancestor")
	}
	if _, statErr := os.Lstat(unexpected); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("reparse target was mutated before certification: %v", statErr)
	}
}
