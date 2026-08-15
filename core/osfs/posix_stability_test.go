//go:build linux || darwin

package osfs

import (
	"context"
	"errors"
	"math"
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
	if err := stable.Verify(context.Background()); err != nil {
		t.Fatalf("initial verification = %v", err)
	}
	if err := os.WriteFile(path, []byte("mutated source contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stable.Verify(context.Background()); !errors.Is(err, content.ErrSourceDrift) {
		t.Fatalf("mutation verification = %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := stable.Verify(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled verify error = %v", err)
	}
	if _, err := stable.ReadAt(canceled, buffer, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read error = %v", err)
	}
	if _, err := stable.ReadAt(context.Background(), buffer, math.MaxUint64); !errors.Is(err, content.ErrBlockOutOfRange) {
		t.Fatalf("out of range read error = %v", err)
	}
}

func TestPOSIXStabilityPlatformConstructorsAndBinderEdgeCases(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "test.bin")
	if err := os.WriteFile(path, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	handle, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()

	// platformCatalogBaseline
	ident, cand, err := platformCatalogBaseline(handle)
	if err != nil || ident.IsZero() || cand.IsZero() {
		t.Fatalf("platformCatalogBaseline = (%+v, %+v, %v)", ident, cand, err)
	}

	// newPlatformRootedRevisionSource
	revSource, err := newPlatformRootedRevisionSource([]string{root})
	if err != nil {
		t.Fatalf("newPlatformRootedRevisionSource error = %v", err)
	}
	_ = revSource.Close()

	// openNativeOutputPlatform
	platform, err := openNativeOutputPlatform(root, false)
	if err != nil {
		t.Fatalf("openNativeOutputPlatform error = %v", err)
	}
	_ = platform.Close()

	// POSIXStabilityBinder edge cases
	binder := POSIXStabilityBinder{}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := binder.BindStable(canceled, StableBinding{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled bind error = %v", err)
	}
	if _, err := binder.BindStable(context.Background(), StableBinding{File: nil}); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("nil file bind error = %v", err)
	}

	// Stale candidate / identity mismatch
	var dummyRecord catalog.NodeRecord
	if _, err := binder.BindStable(context.Background(), StableBinding{File: handle, Record: dummyRecord}); !errors.Is(err, content.ErrRevisionStale) {
		t.Fatalf("mismatched record bind error = %v", err)
	}

	// Closed handle error paths
	closedHandle, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = closedHandle.Close()
	if _, _, err := POSIXCatalogBaseline(closedHandle); err == nil {
		t.Fatal("expected error for closed handle baseline")
	}
	if _, err := binder.BindStable(context.Background(), StableBinding{File: closedHandle}); err == nil {
		t.Fatal("expected error for closed handle bind")
	}
}
