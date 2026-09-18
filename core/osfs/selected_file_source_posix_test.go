//go:build linux || darwin

package osfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

func TestSelectedFileSourceRetainsOriginalRootAfterNameReplacement(t *testing.T) {
	base := t.TempDir()
	selectedPath, movedPath := filepath.Join(base, "selected"), filepath.Join(base, "moved")
	if err := os.Mkdir(selectedPath, 0700); err != nil {
		t.Fatal(err)
	}
	original := []byte("authorized original bytes")
	if err := os.WriteFile(filepath.Join(selectedPath, "file.bin"), original, 0600); err != nil {
		t.Fatal(err)
	}
	var allocations byte
	source, err := NewSelectedFileSource(context.Background(), SelectedCatalogSourceConfig{Paths: []string{selectedPath}, SyntheticRoot: catalog.DirectoryID{1}, Identities: CatalogIdentitySourceFunc(func() ([catalog.IdentityBytes]byte, error) {
		allocations++
		if allocations == 1 {
			if err := os.Rename(selectedPath, movedPath); err != nil {
				return [catalog.IdentityBytes]byte{}, err
			}
			if err := os.Mkdir(selectedPath, 0700); err != nil {
				return [catalog.IdentityBytes]byte{}, err
			}
			if err := os.WriteFile(filepath.Join(selectedPath, "file.bin"), []byte("outside original selection"), 0600); err != nil {
				return [catalog.IdentityBytes]byte{}, err
			}
		}
		return [catalog.IdentityBytes]byte{allocations + 1}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	foreign, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{Paths: []string{filepath.Join(selectedPath, "file.bin")}, SyntheticRoot: catalog.DirectoryID{1}})
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	if stable, err := source.OpenStable(context.Background(), foreign.SelectedRoots()[0]); err == nil {
		_ = stable.Close()
		t.Fatal("replacement pathname expanded the original selected-root authority")
	} else if !errors.Is(err, content.ErrRevisionStale) {
		t.Fatalf("foreign object rejection = %v", err)
	}
	children := &collectingScanChildren{}
	if _, err := source.ScanDirectory(context.Background(), catalog.ScanRequest{Directory: source.SelectedRoots()[0], Work: &countingScanWork{}, Children: children}); err != nil {
		t.Fatal(err)
	}
	if len(children.items) != 1 {
		t.Fatalf("original-root children = %+v", children.items)
	}
	child := children.items[0]
	parent, _ := source.SelectedRoots()[0].DirectoryID()
	record, err := catalog.NewFileNodeRecord(child.FileID, parent, child.Name, child.SourceReference, child.SourceIdentity, child.VersionCandidate, child.ExpectedSize, child.ModifiedTime)
	if err != nil {
		t.Fatal(err)
	}
	stable, err := source.OpenStable(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	defer stable.Close()
	payload := make([]byte, stable.ExactSize())
	if _, err := stable.ReadAt(context.Background(), payload, 0); err != nil || !bytes.Equal(payload, original) {
		t.Fatalf("retained original-root read = %q, %v", payload, err)
	}
}
