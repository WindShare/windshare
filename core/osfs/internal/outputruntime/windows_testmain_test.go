//go:build windows

package outputruntime

import (
	"fmt"
	"os"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputwindows"
)

const windowsRuntimeTestBasePattern = ".windshare-outputruntime-test-*"

var windowsRuntimeTestBase string

func TestMain(m *testing.M) {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve Windows output-runtime test home: %v\n", err)
		os.Exit(1)
	}
	base, err := os.MkdirTemp(home, windowsRuntimeTestBasePattern)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reserve Windows output-runtime test base: %v\n", err)
		os.Exit(1)
	}
	if err := os.Remove(base); err != nil {
		fmt.Fprintf(os.Stderr, "release Windows output-runtime test base reservation: %v\n", err)
		os.Exit(1)
	}
	platform, err := outputwindows.Open(base, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create certified Windows output-runtime test base: %v\n", err)
		os.Exit(1)
	}
	if err := platform.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "close certified Windows output-runtime test base: %v\n", err)
		_ = removeWindowsRuntimeTestBase(home, base)
		os.Exit(1)
	}

	// Certification is deliberately package-scoped: child roots inherit the
	// trusted ACL without repeating expensive placement enrollment in every cut.
	// Each test still owns a distinct child, so parallel cases never share
	// namespace mutation authority.
	windowsRuntimeTestBase = base
	code := m.Run()
	windowsRuntimeTestBase = ""
	if err := removeWindowsRuntimeTestBase(home, base); err != nil && code == 0 {
		fmt.Fprintf(os.Stderr, "remove Windows output-runtime test base: %v\n", err)
		code = 1
	}
	os.Exit(code)
}

func removeWindowsRuntimeTestBase(home, base string) error {
	// TestMain owns this one recursive cleanup; keep the same strict descendant
	// check used for child roots so a changed path can never delete the home tree.
	if err := requireWindowsRuntimeTestChild(home, base); err != nil {
		return fmt.Errorf("refuse unsafe Windows output-runtime base cleanup: %w", err)
	}
	return os.RemoveAll(base)
}
