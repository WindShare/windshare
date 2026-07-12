package layout_test

import (
	"errors"
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"

	"github.com/windshare/windshare/core/layout"
)

func TestChunkSetNormalizesAndSnapshots(t *testing.T) {
	input := []layout.ChunkRange{
		{First: 5, End: 8},
		{First: 1, End: 3},
		{First: 3, End: 5},
		{First: 10, End: 10},
	}
	set, err := layout.NewChunkSet(input...)
	if err != nil {
		t.Fatalf("NewChunkSet: %v", err)
	}
	input[0] = layout.ChunkRange{First: 100, End: 101}

	wantRanges := []layout.ChunkRange{{First: 1, End: 8}}
	if got := set.Ranges(); !slices.Equal(got, wantRanges) {
		t.Fatalf("ranges=%v, want %v", got, wantRanges)
	}
	if set.Count() != 7 || !set.Contains(1) || !set.Contains(7) || set.Contains(8) {
		t.Fatalf("count/contains mismatch: count=%d ranges=%v", set.Count(), set.Ranges())
	}
	if got := slices.Collect(set.Iter()); !slices.Equal(got, []uint64{1, 2, 3, 4, 5, 6, 7}) {
		t.Fatalf("iteration=%v", got)
	}

	exposed := set.Ranges()
	exposed[0].End = 2
	if set.Count() != 7 || set.Ranges()[0].End != 8 {
		t.Fatal("Ranges exposed mutable backing storage")
	}
}

func TestChunkSetRejectsReversedRange(t *testing.T) {
	if _, err := layout.NewChunkSet(layout.ChunkRange{First: 3, End: 2}); !errors.Is(err, layout.ErrInvalidChunkRange) {
		t.Fatalf("err=%v, want ErrInvalidChunkRange", err)
	}
}

func TestChunkSetUnionAndSubtract(t *testing.T) {
	left, err := layout.NewChunkSet(
		layout.ChunkRange{First: 1, End: 3},
		layout.ChunkRange{First: 7, End: 9},
	)
	if err != nil {
		t.Fatal(err)
	}
	right, err := layout.NewChunkSet(layout.ChunkRange{First: 2, End: 8})
	if err != nil {
		t.Fatal(err)
	}
	union := left.Union(right)
	if want := []layout.ChunkRange{{First: 1, End: 9}}; !slices.Equal(union.Ranges(), want) {
		t.Fatalf("union=%v, want %v", union.Ranges(), want)
	}

	full := layout.FullChunkSet(20)
	have, err := layout.NewChunkSet(
		layout.ChunkRange{First: 2, End: 5},
		layout.ChunkRange{First: 7, End: 12},
		layout.ChunkRange{First: 18, End: 20},
	)
	if err != nil {
		t.Fatal(err)
	}
	missing := full.Subtract(have)
	want := []layout.ChunkRange{
		{First: 0, End: 2},
		{First: 5, End: 7},
		{First: 12, End: 18},
	}
	if !slices.Equal(missing.Ranges(), want) || missing.Count() != 10 {
		t.Fatalf("subtract=%v count=%d, want %v/10", missing.Ranges(), missing.Count(), want)
	}
}

func TestChunkSetSetAlgebraProperty(t *testing.T) {
	rng := rand.New(rand.NewPCG(20260710, 1))
	for trial := range 500 {
		left, leftBits := randomSet(t, rng)
		right, rightBits := randomSet(t, rng)

		assertSetMatchesBits(t, fmtTrial(trial, "left"), left, leftBits)
		assertSetMatchesBits(t, fmtTrial(trial, "right"), right, rightBits)

		unionBits := make([]bool, len(leftBits))
		subtractBits := make([]bool, len(leftBits))
		for i := range leftBits {
			unionBits[i] = leftBits[i] || rightBits[i]
			subtractBits[i] = leftBits[i] && !rightBits[i]
		}
		assertSetMatchesBits(t, fmtTrial(trial, "union"), left.Union(right), unionBits)
		assertSetMatchesBits(t, fmtTrial(trial, "subtract"), left.Subtract(right), subtractBits)
	}
}

func randomSet(t *testing.T, rng *rand.Rand) (layout.ChunkSet, []bool) {
	t.Helper()
	const universe = 64
	bits := make([]bool, universe)
	ranges := make([]layout.ChunkRange, 0, 12)
	for range rng.IntN(12) {
		first, end := rng.IntN(universe+1), rng.IntN(universe+1)
		if first > end {
			first, end = end, first
		}
		ranges = append(ranges, layout.ChunkRange{First: uint64(first), End: uint64(end)})
		for i := first; i < end; i++ {
			bits[i] = true
		}
	}
	set, err := layout.NewChunkSet(ranges...)
	if err != nil {
		t.Fatalf("NewChunkSet: %v", err)
	}
	return set, bits
}

func assertSetMatchesBits(t *testing.T, label string, set layout.ChunkSet, bits []bool) {
	t.Helper()
	var want []uint64
	for i, present := range bits {
		if present {
			want = append(want, uint64(i))
		}
		if set.Contains(uint64(i)) != present {
			t.Fatalf("%s Contains(%d)=%t, want %t", label, i, set.Contains(uint64(i)), present)
		}
	}
	if got := slices.Collect(set.Iter()); !slices.Equal(got, want) {
		t.Fatalf("%s iteration=%v, want %v", label, got, want)
	}
	if set.Count() != uint64(len(want)) {
		t.Fatalf("%s count=%d, want %d", label, set.Count(), len(want))
	}
}

func fmtTrial(trial int, operation string) string {
	return operation + " trial " + strconv.Itoa(trial)
}

func TestChunkSetKeepsFullShareCompact(t *testing.T) {
	full := layout.FullChunkSet(layout.MaxChunkCount)
	if full.Count() != layout.MaxChunkCount {
		t.Fatalf("count=%d", full.Count())
	}
	if got := full.Ranges(); !slices.Equal(got, []layout.ChunkRange{{End: layout.MaxChunkCount}}) {
		t.Fatalf("full share expanded or malformed: %v", got)
	}
	cut, err := layout.NewChunkSet(layout.ChunkRange{First: 10, End: layout.MaxChunkCount - 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := full.Subtract(cut).Ranges(); !slices.Equal(got, []layout.ChunkRange{
		{First: 0, End: 10},
		{First: layout.MaxChunkCount - 10, End: layout.MaxChunkCount},
	}) {
		t.Fatalf("compact subtraction=%v", got)
	}
}
