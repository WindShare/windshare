package osfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
)

func TestSelectedCatalogSourceRejectsRootsThatShareAName(t *testing.T) {
	base := t.TempDir()
	write := func(parts ...string) string {
		t.Helper()
		path := filepath.Join(append([]string{base}, parts...)...)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	first, same, folded, other := write("a", "notes.txt"), write("b", "notes.txt"), write("c", "NOTES.TXT"), write("d", "other.txt")
	synthetic, _ := catalog.DirectoryIDFromBytes(identity16(1))
	for name, paths := range map[string][]string{
		"same name":           {first, same},
		"case-folded name":    {first, other, folded},
		"same path twice":     {first, first},
		"equivalent spelling": {same, filepath.Join(filepath.Dir(same), "..", "b", "notes.txt")},
	} {
		t.Run(name, func(t *testing.T) {
			source, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{Paths: paths, SyntheticRoot: synthetic})
			if err == nil {
				_ = source.Close()
				t.Fatal("selected roots with colliding names were accepted")
			}
			var selected *catalog.SelectionError
			if !errors.Is(err, catalog.ErrSiblingCollision) || !errors.As(err, &selected) || selected.Label != paths[len(paths)-1] {
				t.Fatalf("collision did not identify the later selected path: %v", err)
			}
		})
	}
	source, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{Paths: []string{first, other}, SyntheticRoot: synthetic})
	if err != nil {
		t.Fatalf("distinct names were rejected: %v", err)
	}
	_ = source.Close()
}

func TestSelectedCatalogSourceRejectsCanonicallyEquivalentNames(t *testing.T) {
	base := t.TempDir()
	composed, decomposed := "caf\u00e9.txt", "cafe\u0301.txt"
	first, second := filepath.Join(base, "a", composed), filepath.Join(base, "b", decomposed)
	for _, path := range []string{first, second} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	synthetic, _ := catalog.DirectoryIDFromBytes(identity16(1))
	source, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{Paths: []string{first, second}, SyntheticRoot: synthetic})
	if err == nil {
		_ = source.Close()
		t.Fatal("NFC and NFD spellings of one name were accepted")
	}
	var selected *catalog.SelectionError
	if !errors.Is(err, catalog.ErrSiblingCollision) || !errors.As(err, &selected) || selected.Label != second {
		t.Fatalf("collision did not identify the later selected path: %v", err)
	}
}

// selectedRootFixture creates count files with distinct non-ASCII names, one per directory.
func selectedRootFixture(tb testing.TB, count int) []string {
	tb.Helper()
	base := tb.TempDir()
	paths := make([]string, count)
	for index := range paths {
		dir := filepath.Join(base, fmt.Sprintf("d%04d", index))
		if err := os.Mkdir(dir, 0o700); err != nil {
			tb.Fatal(err)
		}
		paths[index] = filepath.Join(dir, fmt.Sprintf("\u00c4\u00d6\u00dc-%04d.txt", index))
		if err := os.WriteFile(paths[index], nil, 0o600); err != nil {
			tb.Fatal(err)
		}
	}
	return paths
}

func TestSelectedCatalogSourceCollisionCheckScalesLinearly(t *testing.T) {
	paths := selectedRootFixture(t, catalog.MaxSelectedRoots)
	synthetic, _ := catalog.DirectoryIDFromBytes(identity16(1))
	start := time.Now()
	source, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{Paths: paths, SyntheticRoot: synthetic})
	if err != nil {
		t.Fatalf("distinct names were rejected: %v", err)
	}
	_ = source.Close()
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("opening %d distinct roots took %v; the collision check is not linear", len(paths), elapsed)
	}
	// A collision with the very first root must still be found and name both paths.
	clash := filepath.Join(filepath.Dir(paths[1]), "\u00e4\u00f6\u00fc-0000.txt")
	if err := os.WriteFile(clash, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	paths[len(paths)-1] = clash
	if source, err = NewSelectedCatalogSource(SelectedCatalogSourceConfig{Paths: paths, SyntheticRoot: synthetic}); err == nil {
		_ = source.Close()
		t.Fatal("late collision with the first root was accepted")
	}
	if !errors.Is(err, catalog.ErrSiblingCollision) || !strings.Contains(err.Error(), strconv.Quote(paths[0])) {
		t.Fatalf("collision did not name the earlier path: %v", err)
	}
}

func BenchmarkSelectedCatalogSourceMaxRoots(b *testing.B) {
	paths := selectedRootFixture(b, catalog.MaxSelectedRoots)
	synthetic, _ := catalog.DirectoryIDFromBytes(identity16(1))
	b.ResetTimer()
	for b.Loop() {
		source, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{Paths: paths, SyntheticRoot: synthetic})
		if err != nil {
			b.Fatal(err)
		}
		_ = source.Close()
	}
}
