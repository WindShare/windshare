//go:build windows

package outputruntime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputwindows"
)

const windowsRuntimeTestRootPattern = "case-*"

func openNativeOutputRuntimeTestPlatform(path string, create bool) (outputcap.Platform, error) {
	return outputwindows.Open(path, create)
}

func newNativeRuntimeTestRootSpec(t testing.TB) runtimeTestRootSpec {
	t.Helper()
	requireDurableFilesystemScenario(t)
	if windowsRuntimeTestBase == "" {
		t.Fatal("Windows output-runtime test base is not initialized")
	}
	root, err := os.MkdirTemp(windowsRuntimeTestBase, windowsRuntimeTestRootPattern)
	if err != nil {
		t.Fatalf("reserve isolated Windows output-runtime root: %v", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatalf("resolve isolated Windows output-runtime root: %v", err)
	}
	if err := requireWindowsRuntimeTestChild(windowsRuntimeTestBase, root); err != nil {
		t.Fatalf("validate isolated Windows output-runtime root: %v", err)
	}
	if err := os.Remove(root); err != nil {
		t.Fatalf("release isolated Windows output-runtime root reservation: %v", err)
	}
	t.Cleanup(func() {
		if err := requireWindowsRuntimeTestChild(windowsRuntimeTestBase, root); err != nil {
			t.Errorf("refuse unsafe Windows output-runtime cleanup: %v", err)
			return
		}
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove isolated Windows output-runtime root: %v", err)
		}
	})
	// Reserving the path beneath the one-time certified base keeps parallel tests
	// isolated while leaving root creation and certification to production code.
	return runtimeTestRootSpec{path: root, create: true}
}

func requireWindowsRuntimeTestChild(base, child string) error {
	baseAbsolute, err := filepath.Abs(base)
	if err != nil {
		return fmt.Errorf("resolve base: %w", err)
	}
	childAbsolute, err := filepath.Abs(child)
	if err != nil {
		return fmt.Errorf("resolve child: %w", err)
	}
	relative, err := filepath.Rel(filepath.Clean(baseAbsolute), filepath.Clean(childAbsolute))
	if err != nil {
		return fmt.Errorf("relativize child: %w", err)
	}
	if relative == "." || relative == ".." || filepath.IsAbs(relative) ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("child %q is not strictly beneath base %q", childAbsolute, baseAbsolute)
	}
	return nil
}
