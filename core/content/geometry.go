package content

import (
	"errors"
	"fmt"
	"slices"

	"github.com/windshare/windshare/core/catalog"
)

const (
	MaxInitialRangesPerFile    = 256
	MaxInitialRangesPerRequest = 1_024
	MaxRequestedBlockIndices   = 256
)

var (
	ErrInvalidGeometry   = errors.New("invalid file geometry")
	ErrBlockOutOfRange   = errors.New("file-local block index is out of range")
	ErrInvalidBlockRef   = errors.New("block reference does not match the leased revision")
	ErrNonCanonicalRange = errors.New("byte ranges are not canonical")
	ErrBlockRequestLimit = errors.New("block request exceeds its index limit")
	ErrInitialRangeLimit = errors.New("open revision request exceeds its initial-range limit")
)

type FileGeometry struct {
	exactSize uint64
	chunkSize uint32
}

func NewFileGeometry(exactSize uint64, chunkSize uint32) (FileGeometry, error) {
	if exactSize > catalog.MaxFileSize || chunkSize < catalog.MinChunkSize || chunkSize > catalog.MaxChunkSize || chunkSize&(chunkSize-1) != 0 {
		return FileGeometry{}, fmt.Errorf("%w: size=%d chunk=%d", ErrInvalidGeometry, exactSize, chunkSize)
	}
	return FileGeometry{exactSize: exactSize, chunkSize: chunkSize}, nil
}

func (g FileGeometry) ExactSize() uint64 { return g.exactSize }
func (g FileGeometry) ChunkSize() uint32 { return g.chunkSize }
func (g FileGeometry) BlockCount() uint64 {
	if g.exactSize == 0 {
		return 0
	}
	return 1 + (g.exactSize-1)/uint64(g.chunkSize)
}

func (g FileGeometry) BlockPlainLength(index uint64) (uint32, error) {
	if index >= g.BlockCount() {
		return 0, ErrBlockOutOfRange
	}
	if index < g.BlockCount()-1 {
		return g.chunkSize, nil
	}
	remainder := g.exactSize % uint64(g.chunkSize)
	if remainder == 0 {
		return g.chunkSize, nil
	}
	return uint32(remainder), nil
}

func (g FileGeometry) BlockOffset(index uint64) (uint64, error) {
	if index >= g.BlockCount() {
		return 0, ErrBlockOutOfRange
	}
	return index * uint64(g.chunkSize), nil
}

type Range struct {
	Offset uint64
	End    uint64
}

func (r Range) Length() uint64 { return r.End - r.Offset }

type RangeSet struct{ ranges []Range }

func NewRangeSet(ranges []Range) (RangeSet, error) {
	owned := slices.Clone(ranges)
	for index, current := range owned {
		if current.Offset >= current.End || current.End > catalog.MaxFileSize {
			return RangeSet{}, fmt.Errorf("%w: invalid range %d", ErrNonCanonicalRange, index)
		}
		if index > 0 && current.Offset <= owned[index-1].End {
			return RangeSet{}, fmt.Errorf("%w: range %d is unsorted, overlapping, or adjacent", ErrNonCanonicalRange, index)
		}
	}
	return RangeSet{ranges: owned}, nil
}

func (s RangeSet) Ranges() []Range { return slices.Clone(s.ranges) }
func (s RangeSet) IsEmpty() bool   { return len(s.ranges) == 0 }
func (s RangeSet) Len() int        { return len(s.ranges) }

func (g FileGeometry) BlocksForRanges(ranges RangeSet) ([]uint64, error) {
	result := make([]uint64, 0)
	for _, requested := range ranges.ranges {
		if requested.End > g.exactSize {
			return nil, ErrBlockOutOfRange
		}
		first := requested.Offset / uint64(g.chunkSize)
		last := (requested.End - 1) / uint64(g.chunkSize)
		for index := first; index <= last; index++ {
			if len(result) == 0 || result[len(result)-1] != index {
				if len(result) == MaxRequestedBlockIndices {
					return nil, ErrBlockRequestLimit
				}
				result = append(result, index)
			}
		}
	}
	return result, nil
}

type BlockRef struct {
	fileID          catalog.FileID
	fileRevision    FileRevision
	localBlockIndex uint64
}

func NewBlockRef(fileID catalog.FileID, revision FileRevision, localBlockIndex uint64, geometry FileGeometry) (BlockRef, error) {
	if fileID.IsZero() || revision.IsZero() {
		return BlockRef{}, ErrInvalidBlockRef
	}
	if localBlockIndex >= geometry.BlockCount() {
		return BlockRef{}, ErrBlockOutOfRange
	}
	return BlockRef{fileID: fileID, fileRevision: revision, localBlockIndex: localBlockIndex}, nil
}

func (r BlockRef) FileID() catalog.FileID     { return r.fileID }
func (r BlockRef) FileRevision() FileRevision { return r.fileRevision }
func (r BlockRef) LocalBlockIndex() uint64    { return r.localBlockIndex }
