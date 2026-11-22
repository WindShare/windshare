//go:build windows || linux || darwin

package osfs

import (
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/osfs/internal/output"
)

func TestNativeOutputDestinationCapabilities(t *testing.T) {
	path := filepath.Join(t.TempDir(), "first", "second")
	root, disposition, err := output.OpenContainer(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if disposition != output.AuthorityCreatedRoot {
		t.Fatal("new destination did not retain its creation disposition")
	}
	capabilities, err := output.ProbeDestination(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := capabilities.ValidateSupport(); err != nil {
		t.Fatalf("local destination cannot retain restart progress: err=%v", err)
	}
	reopened, reopenedDisp, err := output.OpenContainer(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopenedDisp != output.CallerProvidedContainer {
		t.Fatal("reopening a destination claimed creation ownership")
	}
	if !reopened.SameDirectory(root) {
		t.Fatal("reopen changed the destination")
	}
}
