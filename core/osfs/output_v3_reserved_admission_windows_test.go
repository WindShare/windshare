//go:build windows

package osfs

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/transfer"
)

func TestWindowsV3ReservedSelectionFamiliesRejectBeforeProbe(t *testing.T) {
	root := t.TempDir()
	authority := v3RecoveryAuthority(t, root, nil)
	mutationTrap := trapWindowsV3ObjectIDMutation(authority)
	probeCalls := 0
	nativeFactory := authority.platformFactory
	authority.platformFactory = func(path string, create bool) (outputV3Platform, error) {
		platform, err := nativeFactory(path, create)
		if err != nil {
			return nil, err
		}
		return &windowsV3ReservedProbeCountingPlatform{outputV3Platform: platform, calls: &probeCalls}, nil
	}

	tests := []struct {
		name      string
		selection func(*testing.T) transfer.OutputSelection
	}{
		{
			name: "control exact root file",
			selection: func(t *testing.T) transfer.OutputSelection {
				return v3RecoverySelectionPaths(t, []string{".windshare-output"}, 1)
			},
		},
		{
			name: "control mixed-case descendant",
			selection: func(t *testing.T) transfer.OutputSelection {
				return windowsV3TestDirectorySelection(t, ".WiNdShArE-OuTpUt/child")
			},
		},
		{
			name: "bootstrap descendant",
			selection: func(t *testing.T) transfer.OutputSelection {
				return windowsV3TestDirectorySelection(t, ".windshare-output.bootstrap-dead/child")
			},
		},
		{
			name: "probe mixed-case descendant",
			selection: func(t *testing.T) transfer.OutputSelection {
				return windowsV3TestDirectorySelection(t, ".WiNdShArE-OuTpUt.PrObE-dead/child")
			},
		},
		{
			name: "metadata probe descendant",
			selection: func(t *testing.T) transfer.OutputSelection {
				return windowsV3TestDirectorySelection(t, ".windshare-output.metadata-probe-dead/child")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session, err := authority.OpenSelection(context.Background(), test.selection(t))
			if session != nil {
				_, _ = session.PauseJob(context.Background(), transfer.JobPauseOutputFailure)
				t.Fatal("reserved selection unexpectedly returned a session")
			}
			if !errors.Is(err, errReservedOutputPath) {
				t.Fatalf("reserved selection error = %v, want reserved-path rejection", err)
			}
			requireWindowsV3FreshSelectionFault(t, err)
		})
	}
	if probeCalls != 0 {
		t.Fatalf("reserved selections reached the recoverability probe %d times", probeCalls)
	}
	if calls := mutationTrap.calls.Load(); calls != 0 {
		t.Fatalf("reserved selections invoked CreateOrGet %d times", calls)
	}
	assertWindowsV3StaticAdmissionLeftRootUntouched(t, root)
}

func TestWindowsV3ReservedComponentKeyAllowsNormalSelection(t *testing.T) {
	root := t.TempDir()
	platform, err := openOutputV3Platform(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	if err := validateReservedOutputSelection(
		platform, v3RecoverySelectionPaths(t, []string{"ordinary.bin"}, 1),
	); err != nil {
		t.Fatalf("normal selection was treated as internal output state: %v", err)
	}
	assertWindowsV3StaticAdmissionLeftRootUntouched(t, root)
}

func TestWindowsV3PlatformEquivalentSelectionRejectsBeforeProbe(t *testing.T) {
	root := t.TempDir()
	authority := v3RecoveryAuthority(t, root, nil)
	mutationTrap := trapWindowsV3ObjectIDMutation(authority)
	probeCalls := 0
	nativeFactory := authority.platformFactory
	authority.platformFactory = func(path string, create bool) (outputV3Platform, error) {
		platform, err := nativeFactory(path, create)
		if err != nil {
			return nil, err
		}
		return &windowsV3ReservedProbeCountingPlatform{outputV3Platform: platform, calls: &probeCalls}, nil
	}
	selection := v3RecoverySelectionPaths(t, []string{"Alias.bin", "alias.bin"}, 1)

	session, err := authority.OpenSelection(context.Background(), selection)
	if session != nil {
		_, _ = session.PauseJob(context.Background(), transfer.JobPauseOutputFailure)
		t.Fatal("platform-equivalent selection unexpectedly returned a session")
	}
	if !strings.Contains(err.Error(), "platform-equivalent") {
		t.Fatalf("platform-equivalent selection error = %v", err)
	}
	requireWindowsV3FreshSelectionFault(t, err)
	if probeCalls != 0 {
		t.Fatalf("platform-equivalent selection reached the recoverability probe %d times", probeCalls)
	}
	if calls := mutationTrap.calls.Load(); calls != 0 {
		t.Fatalf("platform-equivalent selection invoked CreateOrGet %d times", calls)
	}
	assertWindowsV3StaticAdmissionLeftRootUntouched(t, root)
}

type windowsV3ReservedProbeCountingPlatform struct {
	outputV3Platform
	calls *int
}

func (platform *windowsV3ReservedProbeCountingPlatform) ProbeRecoverableFeatures() error {
	(*platform.calls)++
	return platform.outputV3Platform.ProbeRecoverableFeatures()
}

func assertWindowsV3StaticAdmissionLeftRootUntouched(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("static admission rejection changed output root: entries=%v error=%v", entries, err)
	}
}

func requireWindowsV3FreshSelectionFault(t *testing.T, err error) {
	t.Helper()
	var fault *transfer.OutputFault
	if !errors.As(err, &fault) || fault.Scope() != transfer.OutputFaultSession ||
		fault.Code() != transfer.OutputFaultNamespaceUnsafe {
		t.Fatalf("static selection error = %v, want session-scoped namespace rejection", err)
	}
	var sessionErr *transfer.OutputSessionError
	if errors.As(err, &sessionErr) {
		t.Fatalf("fresh static selection rejection requested an explicit pause: %v", err)
	}
	if fault.RequiresJobPause() {
		t.Fatalf("fresh static selection rejection requested an effective pause: %v", err)
	}
}
