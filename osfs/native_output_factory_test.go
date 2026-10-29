//go:build windows || linux || darwin

package osfs

import (
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func TestNativeOutputDestinationCapabilities(t *testing.T) {
	path := filepath.Join(t.TempDir(), "first", "second")
	platform, err := openNativeOutputPlatform(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	if platform.RootOpenDisposition() != outputcap.AuthorityCreatedRoot {
		t.Fatal("new destination did not retain its creation disposition")
	}
	source, ok := platform.(interface {
		DestinationCapabilities() (outputcap.DestinationCapabilities, error)
	})
	if !ok {
		t.Fatal("production destination cannot report its capabilities")
	}
	capabilities, err := source.DestinationCapabilities()
	if err != nil {
		t.Fatal(err)
	}
	mode, err := outputcap.SelectExecutionMode(capabilities)
	if err != nil || mode != outputcap.ExecutionResumable {
		t.Fatalf("local destination cannot retain restart progress: mode=%v err=%v", mode, err)
	}
	reopened, err := openNativeOutputPlatform(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.RootOpenDisposition() != outputcap.CallerProvidedContainer {
		t.Fatal("reopening a destination claimed creation ownership")
	}
	if same, err := reopened.Root().SameDirectory(platform.Root()); err != nil || !same {
		t.Fatalf("reopen changed the destination: same=%t err=%v", same, err)
	}
}
