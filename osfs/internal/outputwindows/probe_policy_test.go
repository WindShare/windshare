//go:build windows

package outputwindows

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"github.com/windshare/windshare/osfs/internal/outputprobe"
	"golang.org/x/sys/windows"
)

func TestWindowsProbeCleanupProofRevalidatesPrivateParent(t *testing.T) {
	_, guard := windowsOpenGuardedTestRoot(t)
	root := &windowsOutputDirectory{native: guard.Root()}
	directory, err := root.CreateDirectory("probe-parent-policy", true)
	if err != nil {
		t.Fatal(err)
	}
	workspace := outputprobe.NewWorkspace(root, "probe-parent-policy", directory)
	native := directory.(*windowsOutputDirectory).native
	originalInspector := native.inspector
	t.Cleanup(func() {
		native.inspector = originalInspector
		if err := workspace.Cleanup(); err != nil {
			t.Error(err)
		}
	})
	for _, proof := range []func() error{
		func() error { return workspace.SafePublish(outputprobe.WindowsNTFS) },
		workspace.RangeRecovery, workspace.OperationRecovery,
	} {
		if err := proof(); err != nil {
			t.Fatal(err)
		}
	}
	native.inspector = windowsObjectInspectorFunc(func(handle windows.Handle) (windowsObjectFacts, error) {
		facts, err := originalInspector.Inspect(handle)
		if handle == native.handle() {
			facts.attributes &^= windows.FILE_ATTRIBUTE_HIDDEN
		}
		return facts, err
	})
	if err := verifyWindowsProbeCleanup(workspace); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("unprotected parent cleanup proof=%v", err)
	}
}
