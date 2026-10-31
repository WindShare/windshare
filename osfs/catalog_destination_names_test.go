package osfs

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestSelectedCatalogSourceRetainsDestinationSpecificNames(t *testing.T) {
	root := t.TempDir()
	names := []string{"ordinary.txt", "NUL .txt", "COM1 .photos"}
	if runtime.GOOS != "windows" {
		names = append(names, "NUL", "question?.txt", "trailing-space ", "trailing-dot.")
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	synthetic, _ := catalog.DirectoryIDFromBytes(identity16(1))
	source, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{Paths: []string{root}, SyntheticRoot: synthetic})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	work, children := &countingScanWork{}, &collectingScanChildren{}
	result, err := source.ScanDirectory(context.Background(), catalog.ScanRequest{
		Directory: source.SelectedRoots()[0], Work: work, Children: children,
	})
	if err != nil || result.OmittedCount != 0 || len(children.items) != len(names) {
		t.Fatalf("scan lost readable entries: result=%+v children=%+v err=%v", result, children.items, err)
	}
	seen := make(map[string]bool)
	for _, child := range children.items {
		location, err := parseSourceReference(child.SourceReference)
		if err != nil || location.RelativePath() != child.Name {
			t.Fatalf("source identity lost original spelling: child=%+v err=%v", child, err)
		}
		seen[child.Name] = true
	}
	for _, name := range names {
		if !seen[name] {
			t.Fatalf("source name %q was omitted or rewritten", name)
		}
	}
}

func TestSelectedDestinationSpecificNameRetainsReadableRevision(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "NUL .txt")
	payload := []byte("ordinary file bytes, not a device")
	if err := os.WriteFile(filename, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	synthetic, _ := catalog.DirectoryIDFromBytes(identity16(2))
	source, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{Paths: []string{filename}, SyntheticRoot: synthetic})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	revisions, err := source.RevisionSource()
	if err != nil {
		t.Fatal(err)
	}
	defer revisions.Close()
	stable, err := revisions.OpenStable(context.Background(), source.SelectedRoots()[0])
	if err != nil {
		t.Fatal(err)
	}
	defer stable.Close()
	actual := make([]byte, len(payload))
	if count, err := stable.ReadAt(context.Background(), actual, 0); err != nil || count != len(payload) || string(actual) != string(payload) {
		t.Fatalf("read = %d %q, %v", count, actual, err)
	}
}
