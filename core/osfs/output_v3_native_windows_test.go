//go:build windows

package osfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"golang.org/x/sys/windows"
)

const (
	windowsNTFSNativeCertificationProfile = "windows-ntfs"
	// Keep the probe namespace explicit at the root-native boundary so recovery
	// recognizes this bounded pre-cleanup artifact.
	windowsNativeOutputProbePrefix = ".windshare-output.probe-"
)

func TestWindowsNTFSNativeCertification(t *testing.T) {
	requireUnprivilegedWindowsNTFSCertification(t)
	platform := openCertifiedNativeOutputForTest(
		t,
		t.TempDir(),
		windowsNTFSNativeCertificationProfile,
		outputcap.CertificationWindowsNTFSProcessRestart,
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
		outputcap.CertificationWindowsNTFSProcessRestart,
		windowsNativeOutputProbePrefix+"27182818284590452353602874713526",
		1,
	)
	// CreatePrivateFile is durable before Truncate. Killing at size zero proves
	// recovery accepts that exact pre-truncate cut without broadening later
	// hard-link states to zero-length data objects.
	runNativeOutputProcessRestartRecoveryTest(
		t,
		windowsNTFSNativeCertificationProfile,
		outputcap.CertificationWindowsNTFSProcessRestart,
		windowsNativeOutputProbePrefix+"27182818284590452353602874713526",
		0,
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

func TestWindowsNTFSCreatesMissingRootThroughCertifiedHandles(t *testing.T) {
	target := filepath.Join(t.TempDir(), "first", "second")
	platform, err := openNativeOutputPlatform(target, true)
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
	platform, err := openNativeOutputPlatform(filepath.Join(alias, "must-not-exist"), true)
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
