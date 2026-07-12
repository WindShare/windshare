package layout

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

var (
	ErrUnknownPath     = errors.New("layout: path is not present in the layout")
	ErrChunkOutOfRange = errors.New("layout: chunk index is out of range")
)

// Entry is the minimum input required to derive packed-stream geometry.
type Entry struct {
	Path  string
	Size  int64
	IsDir bool
}

// FileRange identifies the portion of one file covered by a chunk.
type FileRange struct {
	Path string
	Off  int64
	N    int64
}

// CompareCanonical orders canonical paths by their UTF-8 bytes.
func CompareCanonical(a, b string) int { return strings.Compare(a, b) }

// SortCanonical mutates entries into sender-side canonical order. Receivers preserve
// authenticated manifest order instead.
func SortCanonical(entries []Entry) {
	slices.SortFunc(entries, func(a, b Entry) int { return CompareCanonical(a.Path, b.Path) })
}

// Layout is an immutable packed-stream geometry derived from manifest array order.
type Layout struct {
	entries  []Entry
	geometry Geometry
	starts   []int64
	files    []int
	order    []int
}

// Selection is the immutable result of resolving selectors against a Layout. Entry
// indices refer to the authenticated input order, while Chunks is normalized in global
// stream order. Keeping both projections together prevents demand and materialization
// from interpreting selectors independently.
type Selection struct {
	entryIndices []int
	chunks       ChunkSet
}

// EntryIndices returns a snapshot so callers cannot mutate the resolved selection.
func (s Selection) EntryIndices() []int { return slices.Clone(s.entryIndices) }

func (s Selection) Chunks() ChunkSet { return s.chunks }

// New validates all geometry before exposing a layout.
func New(entries []Entry, chunkSize int64) (*Layout, error) {
	geometry, err := DeriveGeometry(entries, chunkSize)
	if err != nil {
		return nil, err
	}
	l := &Layout{
		entries:  slices.Clone(entries),
		geometry: geometry,
		starts:   make([]int64, len(entries)),
		order:    make([]int, len(entries)),
	}
	var cursor int64
	for i, entry := range l.entries {
		l.starts[i] = cursor
		l.order[i] = i
		if entry.IsDir || entry.Size == 0 {
			continue
		}
		l.files = append(l.files, i)
		cursor += entry.Size
	}
	slices.SortFunc(l.order, func(a, b int) int {
		return CompareCanonical(l.entries[a].Path, l.entries[b].Path)
	})
	return l, nil
}

func (l *Layout) StreamLen() int64   { return l.geometry.StreamLen() }
func (l *Layout) ChunkSize() int64   { return l.geometry.ChunkSize() }
func (l *Layout) NumChunks() uint64  { return l.geometry.ChunkCount() }
func (l *Layout) Geometry() Geometry { return l.geometry }

// ChunkToRanges maps one global chunk to file-local ranges in stream order.
func (l *Layout) ChunkToRanges(i uint64) ([]FileRange, error) {
	if n := l.NumChunks(); i >= n {
		return nil, fmt.Errorf("%w: %d (count %d)", ErrChunkOutOfRange, i, n)
	}
	lo := int64(i) * l.ChunkSize()
	hi := min(lo+l.ChunkSize(), l.StreamLen())
	j := sort.Search(len(l.files), func(k int) bool {
		entry := l.files[k]
		return l.starts[entry]+l.entries[entry].Size > lo
	})
	var out []FileRange
	for ; j < len(l.files); j++ {
		entry := l.files[j]
		fileStart := l.starts[entry]
		if fileStart >= hi {
			break
		}
		fileEnd := fileStart + l.entries[entry].Size
		overlapStart, overlapEnd := max(lo, fileStart), min(hi, fileEnd)
		out = append(out, FileRange{
			Path: l.entries[entry].Path,
			Off:  overlapStart - fileStart,
			N:    overlapEnd - overlapStart,
		})
	}
	return out, nil
}

// Select resolves file and directory selectors once into entry and chunk projections.
// An exact file selector selects only that file; explicit or implicit directories select
// their subtree. Unknown selectors fail instead of becoming an accidental empty transfer.
func (l *Layout) Select(paths []string) (Selection, error) {
	if len(paths) == 0 {
		return Selection{}, nil
	}
	picked := make([]bool, len(l.entries))
	seenSelectors := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if _, duplicate := seenSelectors[path]; duplicate {
			continue
		}
		seenSelectors[path] = struct{}{}
		matched := false
		entry, exact := l.findEntry(path)
		if exact {
			matched = true
			picked[entry] = true
			if !l.entries[entry].IsDir {
				continue
			}
		}

		prefix := path + "/"
		first := sort.Search(len(l.order), func(i int) bool {
			return l.entries[l.order[i]].Path >= prefix
		})
		for i := first; i < len(l.order) && strings.HasPrefix(l.entries[l.order[i]].Path, prefix); i++ {
			matched = true
			picked[l.order[i]] = true
		}
		if !matched {
			return Selection{}, fmt.Errorf("%w: %q", ErrUnknownPath, path)
		}
	}

	selectedCount := 0
	for _, selected := range picked {
		if selected {
			selectedCount++
		}
	}
	indices := make([]int, 0, selectedCount)
	ranges := make([]ChunkRange, 0, selectedCount)
	for entry, selected := range picked {
		if !selected {
			continue
		}
		indices = append(indices, entry)
		if span, ok := l.spanChunks(entry); ok {
			ranges = append(ranges, span)
		}
	}
	set, err := NewChunkSet(ranges...)
	if err != nil {
		return Selection{}, fmt.Errorf("layout: derived invalid chunk set: %w", err)
	}
	return Selection{entryIndices: indices, chunks: set}, nil
}

// ChunksFor is the compact demand projection of Select.
func (l *Layout) ChunksFor(paths []string) (ChunkSet, error) {
	selection, err := l.Select(paths)
	if err != nil {
		return ChunkSet{}, err
	}
	return selection.Chunks(), nil
}

// FileChunkRange returns the half-open chunk interval covered by one entry.
func (l *Layout) FileChunkRange(path string) (ChunkRange, error) {
	entry, ok := l.findEntry(path)
	if !ok {
		return ChunkRange{}, fmt.Errorf("%w: %q", ErrUnknownPath, path)
	}
	span, _ := l.spanChunks(entry)
	return span, nil
}

func (l *Layout) findEntry(path string) (int, bool) {
	i := sort.Search(len(l.order), func(i int) bool {
		return l.entries[l.order[i]].Path >= path
	})
	if i < len(l.order) && l.entries[l.order[i]].Path == path {
		return l.order[i], true
	}
	return 0, false
}

func (l *Layout) spanChunks(entry int) (ChunkRange, bool) {
	item := &l.entries[entry]
	if item.IsDir || item.Size == 0 {
		return ChunkRange{}, false
	}
	start := l.starts[entry]
	return ChunkRange{
		First: uint64(start / l.ChunkSize()),
		End:   uint64((start+item.Size-1)/l.ChunkSize()) + 1,
	}, true
}
