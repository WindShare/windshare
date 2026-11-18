package dirpublish

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRenamePublishesStagedFiles(t *testing.T) {
	root := t.TempDir()
	staged, target := filepath.Join(root, "staged"), filepath.Join(root, "published")
	if err := os.Mkdir(staged, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "index"), []byte("complete"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Rename(t.Context(), staged, target); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(target, "index"))
	if err != nil || string(data) != "complete" {
		t.Fatalf("published index = %q, %v", data, err)
	}
}
