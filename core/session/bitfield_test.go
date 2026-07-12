package session

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/layout"
)

func TestBitfieldBasics(t *testing.T) {
	b := NewBitfield(10)
	if b.Len() != 10 || b.Count() != 0 {
		t.Fatalf("zero value: Len=%d Count=%d", b.Len(), b.Count())
	}
	b.Set(0)
	b.Set(3)
	b.Set(9)
	b.Set(3) // 重复置位幂等
	if b.Count() != 3 {
		t.Errorf("Count = %d, want 3", b.Count())
	}
	for i := range uint64(10) {
		want := i == 0 || i == 3 || i == 9
		if b.Get(i) != want {
			t.Errorf("Get(%d) = %v", i, b.Get(i))
		}
	}
	if b.Get(10) || b.Get(1<<40) {
		t.Error("out-of-range Get should return false")
	}
}

func TestBitfieldSetOutOfRangePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("out-of-range Set should panic")
		}
	}()
	NewBitfield(8).Set(8)
}

func TestBitfieldConstructionEnforcesDenseStateBound(t *testing.T) {
	atLimit := NewBitfield(layout.MaxChunkCount)
	if atLimit.Len() != layout.MaxChunkCount {
		t.Fatalf("Len = %d, want %d", atLimit.Len(), layout.MaxChunkCount)
	}

	defer func() {
		recovered := recover()
		err, ok := recovered.(error)
		if !ok || !errors.Is(err, layout.ErrTooManyChunks) {
			t.Fatalf("NewBitfield above limit panic = %v, want ErrTooManyChunks", recovered)
		}
	}()
	NewBitfield(layout.MaxChunkCount + 1)
}

// 序列化金标:位 i 在字节 i/8 的第 i%8 位(LSB 先),头部 u64 小端位长。
func TestBitfieldMarshalGolden(t *testing.T) {
	b := NewBitfield(10)
	b.Set(0)
	b.Set(3)
	b.Set(9)
	raw, err := b.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	want := "0a00000000000000" + "0902"
	if got := hex.EncodeToString(raw); got != want {
		t.Errorf("byte mismatch:\n got  %s\n want %s", got, want)
	}
}

func TestBitfieldRoundTrip(t *testing.T) {
	for _, n := range []uint64{0, 1, 7, 8, 9, 64, 1000} {
		b := NewBitfield(n)
		for i := uint64(0); i < n; i += 3 {
			b.Set(i)
		}
		raw, err := b.MarshalBinary()
		if err != nil {
			t.Fatalf("MarshalBinary(n=%d): %v", n, err)
		}
		var got Bitfield
		if err := got.UnmarshalBinary(raw); err != nil {
			t.Fatalf("UnmarshalBinary(n=%d): %v", n, err)
		}
		if got.Len() != n || got.Count() != b.Count() {
			t.Errorf("n=%d round-trip mismatch: Len=%d Count=%d", n, got.Len(), got.Count())
		}
		for i := range n {
			if got.Get(i) != b.Get(i) {
				t.Errorf("n=%d bit %d mismatch", n, i)
			}
		}
	}
}

// 反序列化后与源缓冲脱钩:journal 缓冲随后可能被复用。
func TestBitfieldUnmarshalSnapshots(t *testing.T) {
	src := NewBitfield(8)
	src.Set(0)
	raw, _ := src.MarshalBinary()
	var got Bitfield
	if err := got.UnmarshalBinary(raw); err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}
	raw[8] = 0xFF
	if got.Get(1) {
		t.Error("Bitfield shares its backing array with the input buffer")
	}
}

func TestBitfieldUnmarshalRejects(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"头残缺", []byte{1, 2, 3}},
		{"位组过短", []byte{9, 0, 0, 0, 0, 0, 0, 0, 0xFF}},
		{"位组过长", []byte{8, 0, 0, 0, 0, 0, 0, 0, 0xFF, 0x00}},
		{"padding 非零", []byte{4, 0, 0, 0, 0, 0, 0, 0, 0xF0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b Bitfield
			if err := b.UnmarshalBinary(tt.data); !errors.Is(err, ErrBitfieldEncoding) {
				t.Errorf("UnmarshalBinary = %v, want ErrBitfieldEncoding", err)
			}
		})
	}
}

func TestBitfieldUnmarshalEnforcesDenseStateBoundBeforeLength(t *testing.T) {
	exact := make([]byte, 8+int(layout.MaxChunkStateBytes))
	binary.LittleEndian.PutUint64(exact, layout.MaxChunkCount)
	var atLimit Bitfield
	if err := atLimit.UnmarshalBinary(exact); err != nil {
		t.Fatalf("UnmarshalBinary at MaxChunkCount: %v", err)
	}
	if atLimit.Len() != layout.MaxChunkCount {
		t.Fatalf("Len = %d, want %d", atLimit.Len(), layout.MaxChunkCount)
	}

	over := make([]byte, 8)
	binary.LittleEndian.PutUint64(over, layout.MaxChunkCount+1)
	unchanged := NewBitfield(1)
	unchanged.Set(0)
	err := unchanged.UnmarshalBinary(over)
	if !errors.Is(err, layout.ErrTooManyChunks) || !errors.Is(err, ErrBitfieldEncoding) {
		t.Fatalf("UnmarshalBinary above limit = %v, want encoding and chunk-count errors", err)
	}
	if unchanged.Len() != 1 || !unchanged.Get(0) {
		t.Fatal("failed decode mutated the receiver")
	}
}

func TestBitfieldRangeQueries(t *testing.T) {
	have := NewBitfield(20)
	for _, index := range []uint64{0, 1, 7, 8, 9, 15, 19} {
		have.Set(index)
	}
	if got := have.countRange(1, 16); got != 5 {
		t.Fatalf("countRange(1, 16) = %d, want 5", got)
	}
	if got, ok := have.nextClear(0, 20); !ok || got != 2 {
		t.Fatalf("nextClear(0, 20) = (%d, %v), want (2, true)", got, ok)
	}
	for index := uint64(2); index < 19; index++ {
		have.Set(index)
	}
	if got, ok := have.nextClear(0, 20); ok || got != 0 {
		t.Fatalf("nextClear on complete range = (%d, %v), want (0, false)", got, ok)
	}
}
