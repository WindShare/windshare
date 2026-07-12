package session

import (
	"bytes"
	"testing"
)

func frameOf(t *testing.T, index uint64, seq uint32, last bool, payload []byte) *Block {
	t.Helper()
	return &Block{Index: index, Seq: seq, Last: last, Payload: payload}
}

func TestReassemblyOutOfOrder(t *testing.T) {
	a := newReassembly()
	// 乱序到达:2(last) → 0 → 1。
	if _, complete, err := a.add(frameOf(t, 5, 2, true, []byte("cc")), 100); err != nil || complete {
		t.Fatalf("seq2: complete=%v err=%v", complete, err)
	}
	if _, complete, err := a.add(frameOf(t, 5, 0, false, []byte("aa")), 100); err != nil || complete {
		t.Fatalf("seq0: complete=%v err=%v", complete, err)
	}
	blockCT, complete, err := a.add(frameOf(t, 5, 1, false, []byte("bb")), 100)
	if err != nil || !complete {
		t.Fatalf("seq1: complete=%v err=%v", complete, err)
	}
	if !bytes.Equal(blockCT, []byte("aabbcc")) {
		t.Errorf("reassembled result %q", blockCT)
	}
}

func TestReassemblySingleFrame(t *testing.T) {
	a := newReassembly()
	blockCT, complete, err := a.add(frameOf(t, 1, 0, true, []byte("only")), 100)
	if err != nil || !complete || !bytes.Equal(blockCT, []byte("only")) {
		t.Fatalf("single-frame block: %q complete=%v err=%v", blockCT, complete, err)
	}
}

func TestReassemblyViolations(t *testing.T) {
	t.Run("重复 seq", func(t *testing.T) {
		a := newReassembly()
		a.add(frameOf(t, 1, 0, false, []byte("x")), 100)
		if _, _, err := a.add(frameOf(t, 1, 0, false, []byte("y")), 100); err == nil {
			t.Error("duplicate seq should be rejected")
		}
	})
	t.Run("seq 越过末帧", func(t *testing.T) {
		a := newReassembly()
		a.add(frameOf(t, 1, 1, true, []byte("x")), 100)
		if _, _, err := a.add(frameOf(t, 1, 2, false, []byte("y")), 100); err == nil {
			t.Error("seq after final frame should be rejected")
		}
	})
	t.Run("双 last", func(t *testing.T) {
		a := newReassembly()
		a.add(frameOf(t, 1, 2, true, []byte("x")), 100)
		if _, _, err := a.add(frameOf(t, 1, 1, true, []byte("y")), 100); err == nil {
			t.Error("second final frame should be rejected")
		}
	})
	t.Run("已收帧越过 last", func(t *testing.T) {
		a := newReassembly()
		a.add(frameOf(t, 1, 3, false, []byte("x")), 100)
		if _, _, err := a.add(frameOf(t, 1, 1, true, []byte("y")), 100); err == nil {
			t.Error("final seq before an already received seq should be rejected")
		}
	})
	t.Run("超内存预算", func(t *testing.T) {
		a := newReassembly()
		if _, _, err := a.add(frameOf(t, 1, 0, false, make([]byte, 60)), 100); err != nil {
			t.Fatalf("first frame: %v", err)
		}
		if _, _, err := a.add(frameOf(t, 1, 1, false, make([]byte, 60)), 100); err == nil {
			t.Error("aggregate size above maxBytes should be rejected")
		}
	})
}
