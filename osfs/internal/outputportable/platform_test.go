package outputportable

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func TestOrdinaryFilesPublishWithoutLinksAndNeverOverwrite(t *testing.T) {
	path := t.TempDir()
	p, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	d := p.Root().(*directory)
	f, err := d.CreateFile("partial", true, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt([]byte("data"), 0); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "final"), []byte("user"), 0o600); err != nil {
		t.Fatal(err)
	}
	if outcome, err := d.PublishFileNoReplace(f, "final"); err != nil || outcome != outputcap.PublishNoReplaceCollision {
		t.Fatalf("collision: %v %v", outcome, err)
	}
	if bytes, err := os.ReadFile(filepath.Join(path, "final")); err != nil || string(bytes) != "user" {
		t.Fatalf("foreign final changed: %q %v", bytes, err)
	}
	if err := os.Remove(filepath.Join(path, "final")); err != nil {
		t.Fatal(err)
	}
	if outcome, err := d.PublishFileNoReplace(f, "final"); err != nil || outcome != outputcap.PublishNoReplaceCommitted {
		t.Fatalf("publish: %v %v", outcome, err)
	}
	if _, err := os.Stat(filepath.Join(path, "partial")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stage retained: %v", err)
	}
}

func TestDirectoryReservationAndCleanupReleaseGoRoots(t *testing.T) {
	p, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	d := p.Root().(*directory)
	c, err := d.CreateDirectory("candidate", true)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	installed, err := d.InstallDirectoryNoReplace(c, "reserved")
	if err != nil {
		t.Fatal(err)
	}
	defer installed.Close()
	if err := d.RemoveDirectory("reserved", installed); err != nil {
		t.Fatal(err)
	}
}
