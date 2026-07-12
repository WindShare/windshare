package layout_test

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/layout"
)

func file(path string, size int64) layout.Entry {
	return layout.Entry{Path: path, Size: size}
}

func dir(path string) layout.Entry {
	return layout.Entry{Path: path, IsDir: true}
}

func mustNew(t *testing.T, entries []layout.Entry, chunkSize int64) *layout.Layout {
	t.Helper()
	l, err := layout.New(entries, chunkSize)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return l
}

func collectChunks(set layout.ChunkSet) []uint64 {
	return slices.Collect(set.Iter())
}

// sceneEntries uses unit-scaled sizes so the original cross-file geometry remains
// readable while satisfying the protocol's minimum chunk size:
//
//	stream: a [0,10u) | b [10u,20u) | video [20u,50u) | tail [50u,55u)
//	chunks: [0,16u), [16u,32u), [32u,48u), [48u,55u)
func sceneEntries() []layout.Entry {
	return []layout.Entry{
		dir("docs"),
		file("docs/a.txt", 10*sceneUnit),
		file("docs/b.txt", 10*sceneUnit),
		file("empty", 0),
		dir("media"),
		file("media/v.mp4", 30*sceneUnit),
		file("tail", 5*sceneUnit),
	}
}

const (
	sceneChunkSize = layout.MinChunkSize
	sceneUnit      = sceneChunkSize / 16
)

func TestNewChunkSizeValidation(t *testing.T) {
	for _, chunkSize := range []int64{-8, -1, 0, 3, 6, 12, 1<<20 + 1} {
		if _, err := layout.New(nil, chunkSize); !errors.Is(err, layout.ErrChunkSizeNotPow2) {
			t.Errorf("chunkSize=%d: err=%v, want ErrChunkSizeNotPow2", chunkSize, err)
		}
	}
	if _, err := layout.New(nil, layout.MinChunkSize/2); !errors.Is(err, layout.ErrChunkSizeTooSmall) {
		t.Errorf("below minimum: err=%v, want ErrChunkSizeTooSmall", err)
	}
	if _, err := layout.New(nil, layout.MaxChunkSize*2); !errors.Is(err, layout.ErrChunkSizeTooLarge) {
		t.Errorf("above maximum: err=%v, want ErrChunkSizeTooLarge", err)
	}
	for _, chunkSize := range []int64{layout.MinChunkSize, 2 * layout.MinChunkSize, 1 << 20, layout.MaxChunkSize} {
		if _, err := layout.New(nil, chunkSize); err != nil {
			t.Errorf("chunkSize=%d was unexpectedly rejected: %v", chunkSize, err)
		}
	}
}

func TestNewEntryValidation(t *testing.T) {
	tests := []struct {
		name    string
		entries []layout.Entry
		wantErr error
	}{
		{"文件负size", []layout.Entry{file("a", -1)}, layout.ErrNegativeSize},
		{"目录负size", []layout.Entry{{Path: "d", Size: -5, IsDir: true}}, layout.ErrNegativeSize},
		{"前缀和超限", []layout.Entry{file("a", layout.MaxStreamBytes), file("b", 1)}, layout.ErrStreamTooLarge},
		{"单文件超限", []layout.Entry{file("a", layout.MaxStreamBytes+1)}, layout.ErrStreamTooLarge},
		{"重复文件path", []layout.Entry{file("p", 1), file("p", 2)}, layout.ErrDuplicatePath},
		{"目录与文件同path", []layout.Entry{dir("p"), file("p", 1)}, layout.ErrDuplicatePath},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := layout.New(tc.entries, sceneChunkSize); !errors.Is(err, tc.wantErr) {
				t.Errorf("err=%v, want %v", err, tc.wantErr)
			}
		})
	}

	// 前缀和恰为上限:合法边界,不得误拒。
	// At the byte ceiling the chunk size must also keep dense state within MaxChunkCount.
	l := mustNew(t, []layout.Entry{file("a", layout.MaxStreamBytes-1), file("b", 1)}, layout.MaxChunkSize)
	if l.StreamLen() != layout.MaxStreamBytes {
		t.Errorf("StreamLen=%d, want MaxStreamBytes", l.StreamLen())
	}
}

func TestGeometry(t *testing.T) {
	tests := []struct {
		name          string
		entries       []layout.Entry
		chunkSize     int64
		wantStreamLen int64
		wantNumChunks uint64
	}{
		{"空集", nil, layout.MinChunkSize, 0, 0},
		{"仅目录与空文件", []layout.Entry{dir("d"), file("e", 0), dir("d/x")}, layout.MinChunkSize, 0, 0},
		{"单字节文件", []layout.Entry{file("f", 1)}, layout.MinChunkSize, 1, 1},
		{"恰好整块", []layout.Entry{file("f", layout.MinChunkSize)}, layout.MinChunkSize, layout.MinChunkSize, 1},
		{"末块短一字节溢出", []layout.Entry{file("f", layout.MinChunkSize+1)}, layout.MinChunkSize, layout.MinChunkSize + 1, 2},
		{"恰好两整块", []layout.Entry{file("f", 2*layout.MinChunkSize)}, layout.MinChunkSize, 2 * layout.MinChunkSize, 2},
		{"目录与空文件不移位", []layout.Entry{file("a", 3), dir("d"), file("e", 0), file("b", 5)}, layout.MinChunkSize, 8, 1},
		// 目录 size 不参与前缀和:即便敌意清单给目录标了非零 size,双端推导也一致。
		{"目录标了非零size被忽略", []layout.Entry{{Path: "d", Size: 5, IsDir: true}}, layout.MinChunkSize, 0, 0},
		{"最小块可用", []layout.Entry{file("f", 3*layout.MinChunkSize)}, layout.MinChunkSize, 3 * layout.MinChunkSize, 3},
		{"海量小文件并块", manySmallFiles(10, 3), layout.MinChunkSize, 30, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := mustNew(t, tc.entries, tc.chunkSize)
			if got := l.StreamLen(); got != tc.wantStreamLen {
				t.Errorf("StreamLen=%d, want %d", got, tc.wantStreamLen)
			}
			if got := l.NumChunks(); got != tc.wantNumChunks {
				t.Errorf("NumChunks=%d, want %d", got, tc.wantNumChunks)
			}
			if got := l.ChunkSize(); got != tc.chunkSize {
				t.Errorf("ChunkSize=%d, want %d", got, tc.chunkSize)
			}
		})
	}
}

func manySmallFiles(n int, size int64) []layout.Entry {
	entries := make([]layout.Entry, 0, n)
	for i := range n {
		entries = append(entries, file(fmt.Sprintf("s/%02d", i), size))
	}
	return entries
}

func TestChunkToRanges(t *testing.T) {
	tests := []struct {
		name      string
		entries   []layout.Entry
		chunkSize int64
		chunk     uint64
		want      []layout.FileRange
	}{
		{"跨块边界:块0含a全量+b头部", sceneEntries(), sceneChunkSize, 0, []layout.FileRange{
			{Path: "docs/a.txt", Off: 0, N: 10 * sceneUnit},
			{Path: "docs/b.txt", Off: 0, N: 6 * sceneUnit},
		}},
		{"跨块边界:块1含b尾部+v头部", sceneEntries(), sceneChunkSize, 1, []layout.FileRange{
			{Path: "docs/b.txt", Off: 6 * sceneUnit, N: 4 * sceneUnit},
			{Path: "media/v.mp4", Off: 0, N: 12 * sceneUnit},
		}},
		{"整块落在单文件中段", sceneEntries(), sceneChunkSize, 2, []layout.FileRange{
			{Path: "media/v.mp4", Off: 12 * sceneUnit, N: 16 * sceneUnit},
		}},
		{"末块短:v尾部+tail全量", sceneEntries(), sceneChunkSize, 3, []layout.FileRange{
			{Path: "media/v.mp4", Off: 28 * sceneUnit, N: 2 * sceneUnit},
			{Path: "tail", Off: 0, N: 5 * sceneUnit},
		}},
		{"单文件恰好整块", []layout.Entry{file("f", layout.MinChunkSize)}, layout.MinChunkSize, 0, []layout.FileRange{
			{Path: "f", Off: 0, N: layout.MinChunkSize},
		}},
		{"单文件的末短块", []layout.Entry{file("f", 2*layout.MinChunkSize+4)}, layout.MinChunkSize, 2, []layout.FileRange{
			{Path: "f", Off: 2 * layout.MinChunkSize, N: 4},
		}},
		{"海量小文件并入一块", manySmallFiles(5, 2), layout.MinChunkSize, 0, []layout.FileRange{
			{Path: "s/00", Off: 0, N: 2}, {Path: "s/01", Off: 0, N: 2}, {Path: "s/02", Off: 0, N: 2},
			{Path: "s/03", Off: 0, N: 2}, {Path: "s/04", Off: 0, N: 2},
		}},
		{"末块单字节", []layout.Entry{file("f", layout.MinChunkSize+1)}, layout.MinChunkSize, 1, []layout.FileRange{
			{Path: "f", Off: layout.MinChunkSize, N: 1},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := mustNew(t, tc.entries, tc.chunkSize)
			got, err := l.ChunkToRanges(tc.chunk)
			if err != nil {
				t.Fatalf("ChunkToRanges(%d): %v", tc.chunk, err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("ChunkToRanges(%d)=%v, want %v", tc.chunk, got, tc.want)
			}
		})
	}
}

func TestChunkToRangesOutOfRange(t *testing.T) {
	l := mustNew(t, sceneEntries(), sceneChunkSize)
	if _, err := l.ChunkToRanges(4); !errors.Is(err, layout.ErrChunkOutOfRange) {
		t.Errorf("block 4 out of range: err=%v, want ErrChunkOutOfRange", err)
	}

	empty := mustNew(t, []layout.Entry{dir("d"), file("e", 0)}, layout.MinChunkSize)
	if _, err := empty.ChunkToRanges(0); !errors.Is(err, layout.ErrChunkOutOfRange) {
		t.Errorf("block 0 in empty stream: err=%v, want ErrChunkOutOfRange", err)
	}
}

func TestChunksFor(t *testing.T) {
	tests := []struct {
		name  string
		paths []string
		want  []uint64
	}{
		{"单文件单块", []string{"docs/a.txt"}, []uint64{0}},
		{"文件跨块边界含共享块", []string{"docs/b.txt"}, []uint64{0, 1}},
		{"选中目录=连续块区间", []string{"docs"}, []uint64{0, 1}},
		{"大文件多块", []string{"media/v.mp4"}, []uint64{1, 2, 3}},
		{"空文件无块", []string{"empty"}, nil},
		{"末尾小文件", []string{"tail"}, []uint64{3}},
		{"多选去重排序", []string{"media", "docs"}, []uint64{0, 1, 2, 3}},
		{"重复选择同一路径", []string{"tail", "tail"}, []uint64{3}},
		{"全选", []string{"docs", "empty", "media", "tail"}, []uint64{0, 1, 2, 3}},
		{"空选择", nil, nil},
	}
	l := mustNew(t, sceneEntries(), sceneChunkSize)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			set, err := l.ChunksFor(tc.paths)
			if err != nil {
				t.Fatalf("ChunksFor(%v): %v", tc.paths, err)
			}
			got := collectChunks(set)
			if !slices.Equal(got, tc.want) {
				t.Errorf("ChunksFor(%v)=%v, want %v", tc.paths, got, tc.want)
			}
		})
	}

	if _, err := l.ChunksFor([]string{"docs", "nope"}); !errors.Is(err, layout.ErrUnknownPath) {
		t.Errorf("unknown path: err=%v, want ErrUnknownPath", err)
	}
}

func TestSelectKeepsEntryAndChunkProjectionsTogether(t *testing.T) {
	entries := []layout.Entry{
		file("z", layout.MinChunkSize),
		dir("tree"),
		file("tree/b", layout.MinChunkSize),
		file("tree/a", 0),
	}
	l := mustNew(t, entries, layout.MinChunkSize)
	selection, err := l.Select([]string{"tree", "tree"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got, want := selection.EntryIndices(), []int{1, 2, 3}; !slices.Equal(got, want) {
		t.Fatalf("entry indices=%v, want %v", got, want)
	}
	if got, want := selection.Chunks().Ranges(), []layout.ChunkRange{{First: 1, End: 2}}; !slices.Equal(got, want) {
		t.Fatalf("chunks=%v, want %v", got, want)
	}

	indices := selection.EntryIndices()
	indices[0] = 0
	if got := selection.EntryIndices(); !slices.Equal(got, []int{1, 2, 3}) {
		t.Fatalf("EntryIndices exposed mutable storage: %v", got)
	}
	chunks, err := l.ChunksFor([]string{"tree"})
	if err != nil {
		t.Fatalf("ChunksFor: %v", err)
	}
	if !slices.Equal(chunks.Ranges(), selection.Chunks().Ranges()) {
		t.Fatalf("ChunksFor drifted from Select: %v != %v", chunks.Ranges(), selection.Chunks().Ranges())
	}
}

// 隐式目录:清单未显式列目录条目时,按前缀仍可选中整棵子树。
func TestChunksForImplicitDir(t *testing.T) {
	l := mustNew(t, []layout.Entry{file("x/y/f1", layout.MinChunkSize), file("x/y/f2", layout.MinChunkSize)}, layout.MinChunkSize)
	for _, p := range []string{"x", "x/y"} {
		set, err := l.ChunksFor([]string{p})
		if err != nil {
			t.Fatalf("ChunksFor(%q): %v", p, err)
		}
		got := collectChunks(set)
		if want := []uint64{0, 1}; !slices.Equal(got, want) {
			t.Errorf("ChunksFor(%q)=%v, want %v", p, got, want)
		}
	}
}

// 字节序上兄弟条目 "a!"(0x21 < '/'=0x2f)恰好落在 "a" 与 "a/…" 之间:
// 选中目录 "a" 绝不能把它误收进来。
func TestChunksForPrefixBoundary(t *testing.T) {
	l := mustNew(t, []layout.Entry{dir("a"), file("a!", layout.MinChunkSize), file("a/f", layout.MinChunkSize)}, layout.MinChunkSize)
	set, err := l.ChunksFor([]string{"a"})
	if err != nil {
		t.Fatalf("ChunksFor: %v", err)
	}
	got := collectChunks(set)
	if want := []uint64{1}; !slices.Equal(got, want) {
		t.Errorf("ChunksFor(a)=%v, want %v without selecting sibling a!", got, want)
	}
}

func TestChunksForExactFileDoesNotSelectDescendants(t *testing.T) {
	l := mustNew(t, []layout.Entry{
		file("node", layout.MinChunkSize),
		file("node/child", layout.MinChunkSize),
	}, layout.MinChunkSize)
	set, err := l.ChunksFor([]string{"node"})
	if err != nil {
		t.Fatalf("ChunksFor: %v", err)
	}
	if got := set.Ranges(); !slices.Equal(got, []layout.ChunkRange{{First: 0, End: 1}}) {
		t.Fatalf("exact file selected descendants: %v", got)
	}
}

// 目录子树里只有空文件:选中合法、需求集为空(物化不经块管线)。
func TestChunksForDirOfEmpties(t *testing.T) {
	l := mustNew(t, []layout.Entry{dir("d"), file("d/e1", 0), file("d/e2", 0)}, layout.MinChunkSize)
	set, err := l.ChunksFor([]string{"d"})
	if err != nil {
		t.Fatalf("ChunksFor: %v", err)
	}
	if !set.IsEmpty() {
		t.Errorf("ChunksFor(d)=%v, want empty", set.Ranges())
	}
}

func TestFileChunkRange(t *testing.T) {
	l := mustNew(t, sceneEntries(), sceneChunkSize)
	tests := []struct {
		path string
		want layout.ChunkRange
	}{
		{"docs/a.txt", layout.ChunkRange{First: 0, End: 1}},
		{"docs/b.txt", layout.ChunkRange{First: 0, End: 2}},
		{"media/v.mp4", layout.ChunkRange{First: 1, End: 4}},
		{"tail", layout.ChunkRange{First: 3, End: 4}},
		{"empty", layout.ChunkRange{}}, // size=0 不占流 → 空区间
		{"docs", layout.ChunkRange{}},  // 目录不占流 → 空区间
	}
	for _, tc := range tests {
		got, err := l.FileChunkRange(tc.path)
		if err != nil {
			t.Fatalf("FileChunkRange(%q): %v", tc.path, err)
		}
		if got != tc.want {
			t.Errorf("FileChunkRange(%q)=%v, want %v", tc.path, got, tc.want)
		}
	}

	if _, err := l.FileChunkRange("nope"); !errors.Is(err, layout.ErrUnknownPath) {
		t.Errorf("unknown path: err=%v, want ErrUnknownPath", err)
	}
}

func TestSortCanonical(t *testing.T) {
	entries := []layout.Entry{
		file("b", 1), file("é", 1), file("a/x", 1),
		file("Z", 1), file("a!", 1), dir("a"),
	}
	layout.SortCanonical(entries)
	// UTF-8 字节序:大写 < 小写("Z"<"a"),短前缀在前("a"<"a!"),
	// "a!"(0x21) < "a/x"(0x2f),多字节 "é"(0xC3A9) 殿后。
	want := []string{"Z", "a", "a!", "a/x", "b", "é"}
	for i, w := range want {
		if entries[i].Path != w {
			t.Fatalf("sorted [%d]=%q, want %q (full order %v)", i, entries[i].Path, w, entries)
		}
	}

	if layout.CompareCanonical("a", "b") >= 0 || layout.CompareCanonical("b", "a") <= 0 ||
		layout.CompareCanonical("x", "x") != 0 {
		t.Error("CompareCanonical does not define bytewise total order")
	}
}

// 布局承诺构造时快照输入:调用方随后改动切片不得影响已派生几何。
func TestNewSnapshotsEntries(t *testing.T) {
	entries := []layout.Entry{file("f", layout.MinChunkSize)}
	l := mustNew(t, entries, layout.MinChunkSize)
	entries[0].Size = 999
	if l.StreamLen() != layout.MinChunkSize {
		t.Errorf("StreamLen=%d, want %d; input slice mutation leaked into layout", l.StreamLen(), layout.MinChunkSize)
	}
}

// MaxStreamBytes 级别的流:块号运算须在 uint64/int64 内全程无溢出。
// Only point queries are allowed here; MaxChunkCount must remain an interval, never a slice.
func TestHugeStreamArithmetic(t *testing.T) {
	const chunkSize = layout.MaxChunkSize
	l := mustNew(t, []layout.Entry{file("big", layout.MaxStreamBytes)}, chunkSize)

	const wantChunks = layout.MaxChunkCount
	if got := l.NumChunks(); got != wantChunks {
		t.Fatalf("NumChunks=%d, want %d", got, wantChunks)
	}

	r, err := l.FileChunkRange("big")
	if err != nil {
		t.Fatalf("FileChunkRange: %v", err)
	}
	if want := (layout.ChunkRange{First: 0, End: wantChunks}); r != want {
		t.Errorf("FileChunkRange=%v, want %v", r, want)
	}

	last, err := l.ChunkToRanges(wantChunks - 1)
	if err != nil {
		t.Fatalf("ChunkToRanges for final block: %v", err)
	}
	want := []layout.FileRange{{Path: "big", Off: layout.MaxStreamBytes - chunkSize, N: chunkSize}}
	if !slices.Equal(last, want) {
		t.Errorf("final block=%v, want %v", last, want)
	}

	if _, err := l.ChunkToRanges(wantChunks); !errors.Is(err, layout.ErrChunkOutOfRange) {
		t.Errorf("out-of-range block: err=%v, want ErrChunkOutOfRange", err)
	}
}

// 往返一致性(互逆性质):对随机生成的布局验证
//  1. 每块的 ranges 恰好铺满该块(总量、块内衔接、文件内边界);
//  2. ChunkToRanges 提到的文件,其 ChunksFor 必含该块;
//  3. 每个占流文件 ChunksFor 的每一块,其 ChunkToRanges 必提到该文件;
//  4. FileChunkRange 展开 == ChunksFor 单文件结果;
//  5. 全选 == [0, N) 完整需求集。
func TestRoundTripConsistency(t *testing.T) {
	// 固定种子的 PCG:跨平台跨运行确定复现(测试确定性,§7)。
	rng := rand.New(rand.NewPCG(20260708, 42))
	var entries []layout.Entry
	size := map[string]int64{}
	arrayOrder := map[string]int{} // path → 占流序号,校验块内 ranges 保持流顺序
	for i := range 80 {
		p := fmt.Sprintf("t/%03d", i)
		if i%9 == 0 {
			entries = append(entries, dir(p))
			continue
		}
		sz := rng.Int64N(3 * layout.MinChunkSize) // 含 0:混入不占流的空文件
		entries = append(entries, file(p, sz))
		if sz > 0 {
			arrayOrder[p] = len(arrayOrder)
			size[p] = sz
		}
	}
	const chunkSize = layout.MinChunkSize
	l := mustNew(t, entries, chunkSize)

	var wantStream int64
	for _, sz := range size {
		wantStream += sz
	}
	if l.StreamLen() != wantStream {
		t.Fatalf("StreamLen=%d, want %d", l.StreamLen(), wantStream)
	}

	n := l.NumChunks()
	fileChunks := map[string][]uint64{}
	for _, e := range entries {
		if e.IsDir || size[e.Path] == 0 {
			continue
		}
		set, err := l.ChunksFor([]string{e.Path})
		if err != nil {
			t.Fatalf("ChunksFor(%q): %v", e.Path, err)
		}
		cs := collectChunks(set)
		if !slices.IsSorted(cs) || len(slices.Compact(slices.Clone(cs))) != len(cs) {
			t.Fatalf("ChunksFor(%q)=%v is not sorted and deduplicated", e.Path, cs)
		}
		r, err := l.FileChunkRange(e.Path)
		if err != nil {
			t.Fatalf("FileChunkRange(%q): %v", e.Path, err)
		}
		if uint64(len(cs)) != r.End-r.First || cs[0] != r.First || cs[len(cs)-1] != r.End-1 {
			t.Fatalf("file %q: ChunksFor=%v differs from FileChunkRange=%v", e.Path, cs, r)
		}
		fileChunks[e.Path] = cs
	}

	for i := range n {
		ranges, err := l.ChunkToRanges(i)
		if err != nil {
			t.Fatalf("ChunkToRanges(%d): %v", i, err)
		}
		wantLen := min(chunkSize, l.StreamLen()-int64(i)*chunkSize)
		var got int64
		prevOrder := -1
		for k, r := range ranges {
			if r.N <= 0 || r.Off < 0 || r.Off+r.N > size[r.Path] {
				t.Fatalf("block %d range %+v exceeds file %q (size=%d)", i, r, r.Path, size[r.Path])
			}
			// 块内衔接:非首 range 必从文件头起,非末 range 必到文件尾止,
			// 且文件按流顺序出现——三者合起来即"恰好铺满、无洞无叠"。
			if k > 0 && r.Off != 0 {
				t.Fatalf("block %d non-first range %+v does not start at the file beginning", i, r)
			}
			if k < len(ranges)-1 && r.Off+r.N != size[r.Path] {
				t.Fatalf("block %d non-final range %+v does not end at the file boundary", i, r)
			}
			if ord := arrayOrder[r.Path]; ord <= prevOrder {
				t.Fatalf("block %d ranges are not in stream order: %v", i, ranges)
			} else {
				prevOrder = ord
			}
			got += r.N
			if !slices.Contains(fileChunks[r.Path], i) {
				t.Fatalf("inverse mismatch: block %d references %q, but ChunksFor(%q)=%v does not contain it",
					i, r.Path, r.Path, fileChunks[r.Path])
			}
		}
		if got != wantLen {
			t.Fatalf("block %d covers %d bytes, want %d", i, got, wantLen)
		}
	}

	for p, cs := range fileChunks {
		for _, c := range cs {
			ranges, err := l.ChunkToRanges(c)
			if err != nil {
				t.Fatalf("ChunkToRanges(%d): %v", c, err)
			}
			if !slices.ContainsFunc(ranges, func(r layout.FileRange) bool { return r.Path == p }) {
				t.Fatalf("inverse mismatch: ChunksFor(%q) contains block %d, but ranges=%v do not reference it", p, c, ranges)
			}
		}
	}

	var all []string
	for _, e := range entries {
		all = append(all, e.Path)
	}
	allSet, err := l.ChunksFor(all)
	if err != nil {
		t.Fatalf("select all: %v", err)
	}
	if allSet.Count() != n {
		t.Fatalf("select all covers %d blocks, want %d", allSet.Count(), n)
	}
	got := collectChunks(allSet)
	for i, c := range got {
		if c != uint64(i) {
			t.Fatalf("select-all item %d=%d; demand set is not [0,N)", i, c)
		}
	}
}
