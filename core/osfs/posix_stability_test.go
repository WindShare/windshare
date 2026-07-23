//go:build linux || darwin

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

func TestPOSIXStabilityBinderValidatesCandidateAndDetectsMutation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "source.bin")
	data := []byte("stable source")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	baselineHandle, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	identity, candidate, err := POSIXCatalogBaseline(baselineHandle)
	_ = baselineHandle.Close()
	if err != nil {
		t.Fatal(err)
	}
	var fileID catalog.FileID
	fileID[0] = 1
	var parent catalog.DirectoryID
	parent[0] = 2
	locator, _ := catalog.NewLocator(0, "source.bin")
	record, err := catalog.NewFileNodeRecord(fileID, parent, "source.bin", locator, identity, candidate, uint64(len(data)), catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	source, err := NewRootedRevisionSource(RootedRevisionSourceConfig{RootPaths: []string{root}, Binder: POSIXStabilityBinder{}})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	stable, err := source.OpenStable(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	defer stable.Close()
	buffer := make([]byte, len(data))
	if _, err := stable.ReadAt(context.Background(), buffer, 0); err != nil {
		t.Fatal(err)
	}
	if string(buffer) != string(data) || stable.ExactSize() != uint64(len(data)) || !stable.ModifiedTime().Present() {
		t.Fatalf("stable file metadata/data = %q / %d / %+v", buffer, stable.ExactSize(), stable.ModifiedTime())
	}
	if err := os.WriteFile(path, []byte("source changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stable.Verify(context.Background()); !errors.Is(err, content.ErrSourceDrift) {
		t.Fatalf("mutation verification = %v", err)
	}
}
