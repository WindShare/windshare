package layout_test

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/core/layout"
)

func TestGeometryStateBounds(t *testing.T) {
	if layout.MinChunkSize != 1<<10 || layout.MaxChunkSize != 4<<20 ||
		layout.MaxChunkCount != 1<<26 || layout.MaxChunkStateBytes != 8<<20 {
		t.Fatalf("geometry constants=%d/%d/%d/%d", layout.MinChunkSize, layout.MaxChunkSize, layout.MaxChunkCount, layout.MaxChunkStateBytes)
	}
	if layout.MaxChunkStateBytes != layout.MaxChunkCount/8 {
		t.Fatalf("state budget=%d, want %d", layout.MaxChunkStateBytes, layout.MaxChunkCount/8)
	}
	if want := int64(layout.MaxChunkCount) * layout.MaxChunkSize; layout.MaxStreamBytes != want {
		t.Fatalf("stream budget=%d, want %d", layout.MaxStreamBytes, want)
	}

	maxAtMinimum := int64(layout.MaxChunkCount) * layout.MinChunkSize
	geometry, err := layout.ValidateGeometry(layout.MinChunkSize, maxAtMinimum)
	if err != nil {
		t.Fatalf("exact MaxChunkCount boundary: %v", err)
	}
	if geometry.ChunkCount() != layout.MaxChunkCount {
		t.Fatalf("chunk count=%d, want %d", geometry.ChunkCount(), layout.MaxChunkCount)
	}
	if _, err := layout.ValidateGeometry(layout.MinChunkSize, maxAtMinimum+1); !errors.Is(err, layout.ErrTooManyChunks) {
		t.Fatalf("over state boundary: err=%v, want ErrTooManyChunks", err)
	}
	if _, err := layout.ValidateGeometry(layout.MinChunkSize, -1); !errors.Is(err, layout.ErrNegativeStreamLen) {
		t.Fatalf("negative stream: err=%v", err)
	}
	if _, err := layout.ValidateGeometry(layout.MaxChunkSize, layout.MaxStreamBytes); err != nil {
		t.Fatalf("MaxStreamBytes with bounded chunk state: %v", err)
	}
	if _, err := layout.ValidateGeometry(layout.MaxChunkSize, layout.MaxStreamBytes+1); !errors.Is(err, layout.ErrStreamTooLarge) {
		t.Fatalf("over byte boundary: err=%v, want ErrStreamTooLarge", err)
	}
}

func TestDeriveGeometryUsesFileBytesOnly(t *testing.T) {
	geometry, err := layout.DeriveGeometry([]layout.Entry{
		{Path: "dir", Size: layout.MaxStreamBytes, IsDir: true},
		{Path: "file", Size: layout.MinChunkSize + 1},
	}, layout.MinChunkSize)
	if err != nil {
		t.Fatalf("DeriveGeometry: %v", err)
	}
	if geometry.StreamLen() != layout.MinChunkSize+1 || geometry.ChunkCount() != 2 {
		t.Fatalf("geometry=%d bytes/%d chunks", geometry.StreamLen(), geometry.ChunkCount())
	}
}
