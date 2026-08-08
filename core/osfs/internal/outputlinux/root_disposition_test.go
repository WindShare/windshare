//go:build linux

package outputlinux

import (
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func TestLinuxOpenReportsExactRootDisposition(t *testing.T) {
	requireUnprivilegedLinuxExt4Certification(t)
	base := t.TempDir()

	existing, err := Open(base, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := existing.RootOpenDisposition(); got != outputcap.CallerProvidedContainer {
		_ = existing.Close()
		t.Fatalf("existing root disposition = %q", got)
	}
	if err := existing.Close(); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(base, "created-output")
	created, err := Open(target, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := created.RootOpenDisposition(); got != outputcap.AuthorityCreatedRoot {
		_ = created.Close()
		t.Fatalf("created root disposition = %q", got)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(target, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("close reopened root: %v", err)
		}
	}()
	if got := reopened.RootOpenDisposition(); got != outputcap.CallerProvidedContainer {
		t.Fatalf("reopened root disposition = %q", got)
	}
}
