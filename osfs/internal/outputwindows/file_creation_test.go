//go:build windows

package outputwindows

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputprobe"
	"golang.org/x/sys/windows"
)

func TestWindowsProbeCreationVerificationFailureRollsBack(t *testing.T) {
	for _, target := range []string{outputprobe.StageName, outputprobe.TemporaryRecordName} {
		t.Run(target, func(t *testing.T) {
			rootPath := windowsNativeTestTempDir(t)
			platform, err := openWindowsOutputPlatform(rootPath)
			if err != nil {
				t.Fatal(err)
			}
			defer platform.Close()
			const name = ".windshare-output.probe-00112233445566778899aabbccddeeff"
			directory, err := platform.root.CreatePrivateDirectory(name)
			if err != nil {
				t.Fatal(err)
			}
			workspace := outputprobe.NewWorkspace(
				&windowsOutputDirectory{native: platform.root}, name,
				&windowsOutputDirectory{native: directory},
			)
			defer workspace.Close()
			if target == outputprobe.TemporaryRecordName {
				if err := workspace.SafePublish(outputprobe.SizedDataLinks); err != nil {
					t.Fatal(err)
				}
			}
			originalInspector := directory.inspector
			injected := errors.New("file verification unavailable after native create")
			targetPath := filepath.Join(rootPath, name, target)
			didFail := false
			directory.inspector = windowsObjectInspectorFunc(func(handle windows.Handle) (windowsObjectFacts, error) {
				facts, inspectErr := originalInspector.Inspect(handle)
				_, targetErr := os.Stat(targetPath)
				if inspectErr == nil && targetErr == nil && !didFail &&
					facts.attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
					didFail = true
					return windowsObjectFacts{}, injected
				}
				return facts, inspectErr
			})
			var proofErr error
			if target == outputprobe.StageName {
				proofErr = workspace.SafePublish(outputprobe.SizedDataLinks)
			} else {
				proofErr = workspace.RangeRecovery()
			}
			directory.inspector = originalInspector
			if !didFail || !errors.Is(proofErr, injected) {
				t.Fatalf("injected=%t proof error=%v", didFail, proofErr)
			}
			if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
				t.Fatalf("failed creation was not rolled back: %v", err)
			}
			if err := workspace.Cleanup(); err != nil {
				t.Fatalf("cleanup after rejected creation: %v", err)
			}
			if _, err := os.Stat(filepath.Join(rootPath, name)); !os.IsNotExist(err) {
				t.Fatalf("probe namespace remains: %v", err)
			}
			guard, err := platform.acquirePublicOperationGuard()
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			if err := guard.Root().probeRecoverableFeatures(); err != nil {
				t.Fatalf("next probe rejected the output root: %v", err)
			}
		})
	}
}
