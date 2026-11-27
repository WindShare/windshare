package osfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

// A shell entered through a symlinked directory reports the link in $PWD, which
// Go's Getwd (and so filepath.Abs) returns. `wind share .` and a completed
// `link/` both name the directory the kernel reaches, never the link entry.
func TestSelectionNamingTheDirectoryItselfFollowsAFinalLink(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	if err := os.MkdirAll(filepath.Join(target, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	t.Chdir(link)
	separator := string(filepath.Separator)
	for name, selection := range map[string]string{
		"dot in linked cwd":  ".",
		"trailing separator": link + separator,
		"trailing dot":       link + separator + ".",
		"parent of a child":  "inner" + separator + "..",
	} {
		t.Run(name, func(t *testing.T) {
			source, err := NewSelectedFileSource(context.Background(), SelectedCatalogSourceConfig{
				Paths: []string{selection}, SyntheticRoot: catalog.DirectoryID{1},
			})
			if err != nil {
				t.Fatalf("selection %q = %v", selection, err)
			}
			t.Cleanup(func() { _ = source.Close() })
			root := source.SelectedRoots()[0]
			if _, directory := root.DirectoryID(); !directory || root.Entry().Name() != "link" {
				t.Fatalf("selected root = %q directory=%v, want the directory under its typed name", root.Entry().Name(), directory)
			}
			children := &collectingScanChildren{}
			if _, err := source.ScanDirectory(context.Background(), catalog.ScanRequest{
				Directory: root, Work: &countingScanWork{}, Children: children,
			}); err != nil || len(children.items) != 2 {
				t.Fatalf("scan = %d children, %v", len(children.items), err)
			}
		})
	}
	elsewhere := filepath.Join(parent, "elsewhere", "child")
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(target, "jump")); err != nil {
		t.Fatal(err)
	}
	// The link entry itself, and a lexical path the kernel resolves to another
	// directory, stay unsupported sources.
	for _, selection := range []string{link, link + separator + "jump" + separator + ".."} {
		source, err := NewSelectedFileSource(context.Background(), SelectedCatalogSourceConfig{
			Paths: []string{selection}, SyntheticRoot: catalog.DirectoryID{1},
		})
		if source != nil || !errors.Is(err, content.ErrUnsupportedStability) {
			t.Fatalf("selection %q = %v, %v", selection, source, err)
		}
	}
}
