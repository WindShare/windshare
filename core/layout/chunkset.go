package layout

import (
	"errors"
	"fmt"
	"iter"
	"slices"
)

var ErrInvalidChunkRange = errors.New("layout: invalid half-open chunk range")

// ChunkRange is a half-open global chunk interval [First, End).
type ChunkRange struct {
	First uint64
	End   uint64
}

// ChunkSet is an immutable normalized set of chunk intervals. Intervals are sorted,
// disjoint, and non-adjacent, so set operations scale with interval count rather than
// the number of chunks represented. The zero value is an empty set.
type ChunkSet struct {
	ranges []ChunkRange
	count  uint64
}

// NewChunkSet snapshots and normalizes ranges. Empty ranges are ignored; a reversed
// range is rejected because silently repairing it would hide geometry defects.
func NewChunkSet(ranges ...ChunkRange) (ChunkSet, error) {
	normalized := slices.Clone(ranges)
	for _, r := range normalized {
		if r.End < r.First {
			return ChunkSet{}, fmt.Errorf("%w: [%d,%d)", ErrInvalidChunkRange, r.First, r.End)
		}
	}
	return normalizeChunkRanges(normalized), nil
}

// FullChunkSet compactly represents [0, count).
func FullChunkSet(count uint64) ChunkSet {
	if count == 0 {
		return ChunkSet{}
	}
	return ChunkSet{ranges: []ChunkRange{{End: count}}, count: count}
}

func normalizeChunkRanges(ranges []ChunkRange) ChunkSet {
	ranges = slices.DeleteFunc(ranges, func(r ChunkRange) bool { return r.First == r.End })
	if len(ranges) == 0 {
		return ChunkSet{}
	}
	slices.SortFunc(ranges, func(a, b ChunkRange) int {
		if a.First < b.First {
			return -1
		}
		if a.First > b.First {
			return 1
		}
		if a.End < b.End {
			return -1
		}
		if a.End > b.End {
			return 1
		}
		return 0
	})

	out := make([]ChunkRange, 0, len(ranges))
	for _, next := range ranges {
		if len(out) == 0 || next.First > out[len(out)-1].End {
			out = append(out, next)
			continue
		}
		if next.End > out[len(out)-1].End {
			out[len(out)-1].End = next.End
		}
	}
	var count uint64
	for _, r := range out {
		count += r.End - r.First
	}
	return ChunkSet{ranges: out, count: count}
}

func (s ChunkSet) IsEmpty() bool { return len(s.ranges) == 0 }
func (s ChunkSet) Count() uint64 { return s.count }

// Ranges returns a snapshot so callers cannot mutate the normalized representation.
func (s ChunkSet) Ranges() []ChunkRange { return slices.Clone(s.ranges) }

func (s ChunkSet) Contains(chunk uint64) bool {
	i, ok := slices.BinarySearchFunc(s.ranges, chunk, func(r ChunkRange, value uint64) int {
		switch {
		case r.End <= value:
			return -1
		case r.First > value:
			return 1
		default:
			return 0
		}
	})
	return ok && i < len(s.ranges)
}

// Iter yields chunks in ascending order without first materializing a slice.
func (s ChunkSet) Iter() iter.Seq[uint64] {
	return func(yield func(uint64) bool) {
		for _, r := range s.ranges {
			for chunk := r.First; chunk < r.End; chunk++ {
				if !yield(chunk) {
					return
				}
			}
		}
	}
}

func (s ChunkSet) Union(other ChunkSet) ChunkSet {
	if s.IsEmpty() {
		return other
	}
	if other.IsEmpty() {
		return s
	}
	ranges := make([]ChunkRange, 0, len(s.ranges)+len(other.ranges))
	ranges = append(ranges, s.ranges...)
	ranges = append(ranges, other.ranges...)
	return normalizeChunkRanges(ranges)
}

// Subtract returns s \ other using only interval arithmetic.
func (s ChunkSet) Subtract(other ChunkSet) ChunkSet {
	if s.IsEmpty() || other.IsEmpty() {
		return s
	}
	out := make([]ChunkRange, 0, len(s.ranges))
	j := 0
	for _, base := range s.ranges {
		cursor := base.First
		for j < len(other.ranges) && other.ranges[j].End <= cursor {
			j++
		}
		for k := j; k < len(other.ranges) && other.ranges[k].First < base.End; k++ {
			cut := other.ranges[k]
			if cut.First > cursor {
				out = append(out, ChunkRange{First: cursor, End: min(cut.First, base.End)})
			}
			if cut.End >= base.End {
				cursor = base.End
				break
			}
			if cut.End > cursor {
				cursor = cut.End
			}
		}
		if cursor < base.End {
			out = append(out, ChunkRange{First: cursor, End: base.End})
		}
	}
	return normalizeChunkRanges(out)
}
