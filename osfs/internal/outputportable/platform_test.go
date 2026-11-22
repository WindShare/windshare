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

func TestCallerSymlinkedContainerOpensItsTargetWithoutFollowingInnerLinks(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(target, "elsewhere"), 0o755); err != nil {
		t.Fatal(err)
	}
	container := filepath.Join(base, "downloads")
	if err := os.Symlink(target, container); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(target, "elsewhere"), filepath.Join(target, "inner")); err != nil {
		t.Fatal(err)
	}
	for _, create := range []bool{false, true} {
		p, err := Open(container, create)
		if err != nil {
			t.Fatalf("Open(symlinked container, create=%t) = %v", create, err)
		}
		if p.RootOpenDisposition() != outputcap.CallerProvidedContainer {
			t.Fatalf("existing linked container disposition = %v", p.RootOpenDisposition())
		}
		d := p.Root().(*directory)
		info, err := os.Stat(target)
		if err != nil || !os.SameFile(d.info, info) {
			t.Fatalf("root is not pinned to the link target: %v", err)
		}
		// Duplicate re-validates the pinned path; a pinned symlink would fail here.
		duplicate, err := d.Duplicate()
		if err != nil {
			t.Fatalf("duplicate pinned root: %v", err)
		}
		if err := duplicate.Close(); err != nil {
			t.Fatal(err)
		}
		if inner, err := d.OpenDirectory("inner", false); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			if inner != nil {
				_ = inner.Close()
			}
			t.Fatalf("inner symlink followed: %v", err)
		}
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
