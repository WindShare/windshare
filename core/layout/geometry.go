package layout

import (
	"errors"
	"fmt"
)

const (
	// MinChunkSize keeps encryption and scheduling overhead proportionate even for small
	// shares. One KiB is also the smallest geometry used by the cross-language vectors.
	MinChunkSize int64 = 1 << 10

	// MaxChunkSize caps every plaintext, ciphertext, and reassembly allocation for one
	// block. Four MiB is the documented product ceiling; the cryptographic segment span
	// is a separate concern owned by chunk.
	MaxChunkSize int64 = 4 << 20

	// MaxChunkCount caps every dense per-chunk state vector. At one bit per chunk this
	// is 8 MiB; demand itself remains compact in ChunkSet intervals.
	MaxChunkCount uint64 = 1 << 26

	// MaxChunkStateBytes documents the reproducible dense-state budget for Go and
	// TypeScript implementations.
	MaxChunkStateBytes uint64 = MaxChunkCount / 8

	// MaxStreamBytes is the largest stream representable by the maximum chunk size and
	// bounded chunk-state count. It remains exactly representable in JavaScript.
	MaxStreamBytes int64 = int64(MaxChunkCount) * MaxChunkSize
)

var (
	ErrChunkSizeNotPow2  = errors.New("layout: chunk size must be a positive power of two")
	ErrChunkSizeTooSmall = errors.New("layout: chunk size is below MinChunkSize")
	ErrChunkSizeTooLarge = errors.New("layout: chunk size exceeds MaxChunkSize")
	ErrNegativeSize      = errors.New("layout: entry size is negative")
	ErrNegativeStreamLen = errors.New("layout: stream length is negative")
	ErrStreamTooLarge    = errors.New("layout: stream length exceeds MaxStreamBytes")
	ErrTooManyChunks     = errors.New("layout: chunk count exceeds MaxChunkCount")
	ErrDuplicatePath     = errors.New("layout: duplicate entry path")
)

// Geometry is the validated packed-stream shape. Its fields stay private so callers
// cannot construct a shape that bypasses the resource bounds.
type Geometry struct {
	chunkSize  int64
	streamLen  int64
	chunkCount uint64
}

func (g Geometry) ChunkSize() int64   { return g.chunkSize }
func (g Geometry) StreamLen() int64   { return g.streamLen }
func (g Geometry) ChunkCount() uint64 { return g.chunkCount }

// ValidateGeometry is the single authority for chunk size, stream length, and dense
// chunk-state count. The quotient/remainder form avoids addition overflow.
func ValidateGeometry(chunkSize, streamLen int64) (Geometry, error) {
	switch {
	case chunkSize <= 0 || chunkSize&(chunkSize-1) != 0:
		return Geometry{}, fmt.Errorf("%w: %d", ErrChunkSizeNotPow2, chunkSize)
	case chunkSize < MinChunkSize:
		return Geometry{}, fmt.Errorf("%w: %d < %d", ErrChunkSizeTooSmall, chunkSize, MinChunkSize)
	case chunkSize > MaxChunkSize:
		return Geometry{}, fmt.Errorf("%w: %d > %d", ErrChunkSizeTooLarge, chunkSize, MaxChunkSize)
	case streamLen < 0:
		return Geometry{}, fmt.Errorf("%w: %d", ErrNegativeStreamLen, streamLen)
	case streamLen > MaxStreamBytes:
		return Geometry{}, fmt.Errorf("%w: %d > %d", ErrStreamTooLarge, streamLen, MaxStreamBytes)
	}

	count := uint64(streamLen / chunkSize)
	if streamLen%chunkSize != 0 {
		count++
	}
	if count > MaxChunkCount {
		return Geometry{}, fmt.Errorf("%w: %d > %d", ErrTooManyChunks, count, MaxChunkCount)
	}
	return Geometry{chunkSize: chunkSize, streamLen: streamLen, chunkCount: count}, nil
}

// DeriveGeometry validates entry sizes and derives packed-stream length in manifest
// array order. It allocates by entry count only; chunk count is validated before any
// bitfield or request-state allocation can occur.
func DeriveGeometry(entries []Entry, chunkSize int64) (Geometry, error) {
	if _, err := ValidateGeometry(chunkSize, 0); err != nil {
		return Geometry{}, err
	}

	seen := make(map[string]struct{}, len(entries))
	var streamLen int64
	for _, entry := range entries {
		if _, exists := seen[entry.Path]; exists {
			return Geometry{}, fmt.Errorf("%w: %q", ErrDuplicatePath, entry.Path)
		}
		seen[entry.Path] = struct{}{}
		if entry.Size < 0 {
			return Geometry{}, fmt.Errorf("%w: %q size=%d", ErrNegativeSize, entry.Path, entry.Size)
		}
		if entry.IsDir || entry.Size == 0 {
			continue
		}
		if entry.Size > MaxStreamBytes-streamLen {
			return Geometry{}, fmt.Errorf("%w: prefix sum at %q", ErrStreamTooLarge, entry.Path)
		}
		streamLen += entry.Size
	}
	return ValidateGeometry(chunkSize, streamLen)
}
