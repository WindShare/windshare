package session

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// 线常量与帧类型字节是协议本体(M0 钉死),值变更即破坏跨语言互通,钉死防误改。
func TestWireConstants(t *testing.T) {
	if MaxFrameSize != 64*1024 {
		t.Errorf("MaxFrameSize = %d, want 64 KiB", MaxFrameSize)
	}
	if InFlightWindow != 8 {
		t.Errorf("InFlightWindow = %d, want 8", InFlightWindow)
	}
	if FrameRequest != 0x01 || FrameBlock != 0x02 || FrameError != 0x03 {
		t.Errorf("frame type bytes = %#02x/%#02x/%#02x, want 0x01/0x02/0x03", FrameRequest, FrameBlock, FrameError)
	}
	if FlagLast != 0x01 {
		t.Errorf("FlagLast = %#02x, want bit0", FlagLast)
	}
	if MaxBlockPayload != MaxFrameSize-18 {
		t.Errorf("MaxBlockPayload = %d, want %d", MaxBlockPayload, MaxFrameSize-18)
	}
	if MaxRequestIndices != (MaxFrameSize-5)/8 {
		t.Errorf("MaxRequestIndices = %d", MaxRequestIndices)
	}
}

// 金标字节:硬编码期望帧,供 TS 端逐字节对拍参考(正式向量由 T1.7 入库)。
func TestGoldenFrames(t *testing.T) {
	tests := []struct {
		name    string
		build   func() (Frame, error)
		wantHex string
	}{
		{
			"REQUEST 两块号",
			func() (Frame, error) { return EncodeRequest([]uint64{0x0807060504030201, 2}) },
			"0102000000" + "0102030405060708" + "0200000000000000",
		},
		{
			"BLOCK 末帧",
			func() (Frame, error) {
				return EncodeBlock(Block{Index: 3, Seq: 1, Last: true, Payload: []byte{0xAA, 0xBB, 0xCC}})
			},
			"02" + "0300000000000000" + "01000000" + "01" + "03000000" + "aabbcc",
		},
		{
			"BLOCK 非末帧",
			func() (Frame, error) {
				return EncodeBlock(Block{Index: 0x1122334455667788, Seq: 0, Payload: []byte{0x00}})
			},
			"02" + "8877665544332211" + "00000000" + "00" + "01000000" + "00",
		},
		{
			"ERROR 读块失败",
			func() (Frame, error) { return EncodeError(ErrCodeBlockRead, "drift") },
			"03" + "0200" + "0500" + "6472696674",
		},
		{
			"ERROR 空消息",
			func() (Frame, error) { return EncodeError(ErrCodeBadRequest, "") },
			"03" + "0100" + "0000",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := tt.build()
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if got := hex.EncodeToString(f); got != tt.wantHex {
				t.Errorf("byte mismatch:\n got  %s\n want %s", got, tt.wantHex)
			}
		})
	}
}

func TestRequestRoundTrip(t *testing.T) {
	want := []uint64{0, 1, 42, 1 << 53, ^uint64(0)}
	f, err := EncodeRequest(want)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	msg, err := Decode(f)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	req, ok := msg.(*Request)
	if !ok {
		t.Fatalf("Decode returned %T", msg)
	}
	if len(req.Indices) != len(want) {
		t.Fatalf("index count %d, want %d", len(req.Indices), len(want))
	}
	for i := range want {
		if req.Indices[i] != want[i] {
			t.Errorf("Indices[%d] = %d, want %d", i, req.Indices[i], want[i])
		}
	}
}

func TestBlockRoundTrip(t *testing.T) {
	for _, last := range []bool{true, false} {
		want := Block{Index: 7, Seq: 3, Last: last, Payload: []byte("payload-字节")}
		f, err := EncodeBlock(want)
		if err != nil {
			t.Fatalf("EncodeBlock: %v", err)
		}
		msg, err := Decode(f)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		got, ok := msg.(*Block)
		if !ok {
			t.Fatalf("Decode returned %T", msg)
		}
		if got.Index != want.Index || got.Seq != want.Seq || got.Last != want.Last || !bytes.Equal(got.Payload, want.Payload) {
			t.Errorf("round-trip mismatch: %+v vs %+v", got, want)
		}
	}
}

func TestErrorRoundTrip(t *testing.T) {
	for _, msg := range []string{"", "漂移中止", strings.Repeat("x", 1000)} {
		f, err := EncodeError(ErrCodeSeal, msg)
		if err != nil {
			t.Fatalf("EncodeError: %v", err)
		}
		decoded, err := Decode(f)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		e, ok := decoded.(*Error)
		if !ok {
			t.Fatalf("Decode returned %T", decoded)
		}
		if e.Code != ErrCodeSeal || e.Msg != msg {
			t.Errorf("round-trip mismatch: %+v", e)
		}
	}
}

// Decode 必须快照 payload:传输层可能复用帧缓冲。
func TestDecodeCopiesPayload(t *testing.T) {
	f, _ := EncodeBlock(Block{Index: 1, Seq: 0, Last: true, Payload: []byte{1, 2, 3}})
	msg, err := Decode(f)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	b := msg.(*Block)
	f[blockHeaderLen] = 0xFF
	if b.Payload[0] != 1 {
		t.Error("payload shares its backing array with the input buffer")
	}
}

func TestEncodeRejects(t *testing.T) {
	if _, err := EncodeRequest(nil); !errors.Is(err, ErrEmptyRequest) {
		t.Errorf("empty REQUEST: %v", err)
	}
	if _, err := EncodeRequest(make([]uint64, MaxRequestIndices+1)); !errors.Is(err, ErrFrameOversize) {
		t.Errorf("oversize REQUEST: %v", err)
	}
	if _, err := EncodeBlock(Block{Index: 1}); !errors.Is(err, ErrEmptyPayload) {
		t.Errorf("empty payload: %v", err)
	}
	if _, err := EncodeBlock(Block{Index: 1, Payload: make([]byte, MaxBlockPayload+1)}); !errors.Is(err, ErrFrameOversize) {
		t.Errorf("oversize payload: %v", err)
	}
	if _, err := EncodeError(0, strings.Repeat("x", MaxErrorMsgBytes+1)); !errors.Is(err, ErrFrameOversize) {
		t.Errorf("oversize ERROR message: %v", err)
	}
	if _, err := EncodeError(0, string([]byte{0xff})); !errors.Is(err, ErrInvalidUTF8) {
		t.Errorf("invalid UTF-8 ERROR msg: %v", err)
	}
	// 载荷上限恰好可编码。
	if _, err := EncodeBlock(Block{Index: 1, Last: true, Payload: make([]byte, MaxBlockPayload)}); err != nil {
		t.Errorf("maximum-size BLOCK should encode: %v", err)
	}
}

func TestDecodeRejects(t *testing.T) {
	le32 := func(v uint32) []byte { return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)} }
	validBlock, _ := EncodeBlock(Block{Index: 1, Seq: 0, Last: true, Payload: []byte{9}})

	tests := []struct {
		name    string
		frame   []byte
		wantErr error
	}{
		{"空帧", nil, ErrFrameMalformed},
		{"未知类型", []byte{0x7F, 0, 0}, ErrUnknownFrameType},
		{"整帧超限", make([]byte, MaxFrameSize+1), ErrFrameOversize},
		{"REQUEST 头残缺", []byte{0x01, 0x01}, ErrFrameMalformed},
		{"REQUEST n=0", append([]byte{0x01}, le32(0)...), ErrEmptyRequest},
		{"REQUEST 长度短于声明", append(append([]byte{0x01}, le32(2)...), make([]byte, 8)...), ErrFrameMalformed},
		{"REQUEST 尾部多余", append(append([]byte{0x01}, le32(1)...), make([]byte, 9)...), ErrFrameMalformed},
		{"BLOCK 头残缺", []byte{0x02, 1, 2, 3}, ErrFrameMalformed},
		{"BLOCK 未知 flags", func() []byte {
			f := bytes.Clone(validBlock)
			f[13] = 0x02
			return f
		}(), ErrUnknownFlags},
		{"BLOCK len=0", func() []byte {
			f := bytes.Clone(validBlock[:blockHeaderLen])
			copy(f[14:], le32(0))
			return f
		}(), ErrEmptyPayload},
		{"BLOCK 长度与声明不符", func() []byte {
			f := bytes.Clone(validBlock)
			copy(f[14:], le32(5))
			return f
		}(), ErrFrameMalformed},
		{"ERROR 头残缺", []byte{0x03, 0}, ErrFrameMalformed},
		{"ERROR 长度与声明不符", []byte{0x03, 0, 0, 9, 0, 'x'}, ErrFrameMalformed},
		{"ERROR 非 UTF-8", []byte{0x03, 0, 0, 1, 0, 0xff}, ErrInvalidUTF8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Decode(tt.frame); !errors.Is(err, tt.wantErr) {
				t.Errorf("Decode = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestFatalCode(t *testing.T) {
	if FatalCode(ErrCodeBadRequest) {
		t.Error("BadRequest should be channel-scoped")
	}
	if !FatalCode(ErrCodeBlockRead) || !FatalCode(ErrCodeSeal) {
		t.Error("BlockRead and Seal should be share-scoped")
	}
}

func TestSplitBlockCT(t *testing.T) {
	blockCT := make([]byte, 25)
	for i := range blockCT {
		blockCT[i] = byte(i)
	}
	tests := []struct {
		name       string
		maxPayload int
		wantFrames int
	}{
		{"整除", 5, 5},
		{"有余数", 10, 3},
		{"单帧", 25, 1},
		{"单帧富余", 100, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frames, err := SplitBlockCT(42, blockCT, tt.maxPayload)
			if err != nil {
				t.Fatalf("SplitBlockCT: %v", err)
			}
			if len(frames) != tt.wantFrames {
				t.Fatalf("frame count %d, want %d", len(frames), tt.wantFrames)
			}
			var joined []byte
			for i, f := range frames {
				msg, err := Decode(f)
				if err != nil {
					t.Fatalf("frame %d Decode: %v", i, err)
				}
				b := msg.(*Block)
				if b.Index != 42 || b.Seq != uint32(i) {
					t.Errorf("frame %d: index=%d seq=%d", i, b.Index, b.Seq)
				}
				if b.Last != (i == len(frames)-1) {
					t.Errorf("frame %d last=%v", i, b.Last)
				}
				joined = append(joined, b.Payload...)
			}
			if !bytes.Equal(joined, blockCT) {
				t.Error("reassembled result differs from the original block ciphertext")
			}
			// nonce(密文头 12 字节)必须整体落在首帧,接收侧才能先拿 nonce。
			first := frames[0]
			if tt.maxPayload >= 12 && !bytes.Equal(first[blockHeaderLen:blockHeaderLen+12], blockCT[:12]) {
				t.Error("first frame does not contain the complete nonce prefix")
			}
		})
	}

	if _, err := SplitBlockCT(0, nil, 8); !errors.Is(err, ErrEmptyPayload) {
		t.Errorf("empty block ciphertext: %v", err)
	}
	if _, err := SplitBlockCT(0, blockCT, 0); !errors.Is(err, ErrFrameOversize) {
		t.Errorf("maxPayload=0: %v", err)
	}
	if _, err := SplitBlockCT(0, blockCT, MaxBlockPayload+1); !errors.Is(err, ErrFrameOversize) {
		t.Errorf("maxPayload out of range: %v", err)
	}
}
