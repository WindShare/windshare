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

func TestSymlinkedContainerRequiresAgreeingKernelResolution(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	other := filepath.Join(base, "other")
	for _, dir := range []string{target, other} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	container := filepath.Join(base, "downloads")
	if err := os.Symlink(target, container); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	dangling := filepath.Join(base, "dangling")
	if err := os.Symlink(filepath.Join(base, "missing"), dangling); err != nil {
		t.Fatal(err)
	}
	refusal := errors.New("kernel refused the link")
	cases := []struct {
		name   string
		path   string
		follow func(string) (os.FileInfo, error)
	}{
		{"dangling", dangling, os.Stat},
		// Linux fs.protected_symlinks refuses the kernel follow (EACCES) while
		// user-space readlink still resolves the link.
		{"kernel refusal", container, func(string) (os.FileInfo, error) { return nil, refusal }},
		{"diverging target", container, func(string) (os.FileInfo, error) { return os.Stat(other) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			previous := followContainerLink
			followContainerLink = tc.follow
			t.Cleanup(func() { followContainerLink = previous })
			for _, create := range []bool{false, true} {
				p, err := Open(tc.path, create)
				if !errors.Is(err, outputcap.ErrUnsafeNamespace) {
					if p != nil {
						_ = p.Close()
					}
					t.Fatalf("Open(create=%t) = %v, want ErrUnsafeNamespace", create, err)
				}
			}
		})
	}
	if entries, err := os.ReadDir(other); err != nil || len(entries) != 0 {
		t.Fatalf("refused container changed another directory: %v %v", entries, err)
	}
}
