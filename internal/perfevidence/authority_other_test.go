//go:build !windows && !linux

package perfevidence

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUnsupportedPlatformFailsClosedBeforePublicationMutation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "evidence")
	if _, err := openOutputRootAuthority(root); err == nil {
		t.Fatal("unsupported platform opened an unsafe publication authority")
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("unsupported authority mutated output path: %v", err)
	}
	if _, err := currentProcessToken(); err == nil {
		t.Fatal("unsupported platform synthesized a process-ownership token")
	}
}
