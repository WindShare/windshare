package session

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// recvMsg 从通道端收一条帧并解码,超时 fatal。
func recvMsg(t *testing.T, ch *memChannel, timeout time.Duration) Message {
	t.Helper()
	select {
	case f, ok := <-ch.Recv():
		if !ok {
			t.Fatal("inbound stream is closed")
		}
		msg, err := Decode(f)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		return msg
	case <-time.After(timeout):
		t.Fatal("timed out waiting for frame")
		return nil
	}
}

// collectBlock 持续收帧直到某块凑齐,返回重组的 blockCT。
func collectBlock(t *testing.T, ch *memChannel, index uint64, maxBytes int64) []byte {
	t.Helper()
	a := newReassembly()
	for {
		msg := recvMsg(t, ch, 2*time.Second)
		b, ok := msg.(*Block)
		if !ok {
			t.Fatalf("expected BLOCK, got %T", msg)
		}
		if b.Index != index {
			t.Fatalf("expected block %d, got %d", index, b.Index)
		}
		blockCT, complete, err := a.add(b, maxBytes)
		if err != nil {
			t.Fatalf("reassemble: %v", err)
		}
		if complete {
			return blockCT
		}
	}
}

func startSendSession(t *testing.T, ch FrameChannel, store BlockStore, sealer Sealer) (*SendSession, <-chan error) {
	t.Helper()
	ss, err := NewSendSession(ch, store, sealer)
	if err != nil {
		t.Fatalf("NewSendSession: %v", err)
	}
	done := mustRun(ss, context.Background())
	t.Cleanup(func() { _ = ss.Close() })
	return ss, done
}

func TestSendSessionServesRequest(t *testing.T) {
	checkNoLeak(t)
	codec := &fakeCodec{}
	store := newMemStore(3, 100)
	senderEnd, receiverEnd := newPipe(64)
	_, done := startSendSession(t, senderEnd, store, codec)

	req, _ := EncodeRequest([]uint64{0, 2})
	if err := receiverEnd.Send(context.Background(), req); err != nil {
		t.Fatalf("Send: %v", err)
	}
	for _, idx := range []uint64{0, 2} {
		blockCT := collectBlock(t, receiverEnd, idx, 1<<20)
		plaintext, err := codec.Open(idx, blockCT)
		if err != nil {
			t.Fatalf("Open(%d): %v", idx, err)
		}
		if !bytes.Equal(plaintext, store.blocks[idx]) {
			t.Errorf("block %d content mismatch", idx)
		}
	}
	// 对端收工:关通道,Run 应以 nil 退出。
	_ = receiverEnd.Close()
	if err := waitErr(t, done, 2*time.Second); err != nil {
		t.Errorf("Run = %v, want nil", err)
	}
}

// 大块按 MaxBlockPayload 切多帧,seq 递增且仅末帧置 last。
func TestSendSessionSplitsLargeBlock(t *testing.T) {
	codec := &fakeCodec{}
	store := newMemStore(1, MaxBlockPayload+1000)
	senderEnd, receiverEnd := newPipe(64)
	startSendSession(t, senderEnd, store, codec)

	req, _ := EncodeRequest([]uint64{0})
	if err := receiverEnd.Send(context.Background(), req); err != nil {
		t.Fatalf("Send: %v", err)
	}
	first := recvMsg(t, receiverEnd, 2*time.Second).(*Block)
	second := recvMsg(t, receiverEnd, 2*time.Second).(*Block)
	if first.Seq != 0 || first.Last || len(first.Payload) != MaxBlockPayload {
		t.Errorf("first frame: seq=%d last=%v len=%d", first.Seq, first.Last, len(first.Payload))
	}
	if second.Seq != 1 || !second.Last {
		t.Errorf("final frame: seq=%d last=%v", second.Seq, second.Last)
	}
	blockCT := append(append([]byte(nil), first.Payload...), second.Payload...)
	if plaintext, err := codec.Open(0, blockCT); err != nil || !bytes.Equal(plaintext, store.blocks[0]) {
		t.Errorf("reassembled decrypt failed: %v", err)
	}
}

func TestSendSessionStoreErrorBecomesErrorFrame(t *testing.T) {
	codec := &fakeCodec{}
	store := newMemStore(3, 50)
	drift := errors.New("source snapshot drift; share aborted")
	store.errAt[1] = drift
	senderEnd, receiverEnd := newPipe(64)
	_, done := startSendSession(t, senderEnd, store, codec)

	req, _ := EncodeRequest([]uint64{0, 1})
	_ = receiverEnd.Send(context.Background(), req)
	collectBlock(t, receiverEnd, 0, 1<<20) // 块 0 正常供出
	msg := recvMsg(t, receiverEnd, 2*time.Second)
	e, ok := msg.(*Error)
	if !ok || e.Code != ErrCodeBlockRead {
		t.Fatalf("expected ERROR(BlockRead), got %#v", msg)
	}
	if err := waitErr(t, done, 2*time.Second); !errors.Is(err, drift) {
		t.Errorf("Run = %v, want drift error", err)
	}
	select {
	case _, ok := <-receiverEnd.Recv():
		if ok {
			t.Fatal("terminal must be the final inbound frame")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Recv did not close after terminal delivery")
	}
}

func TestSendSessionReportsTerminalDeliveryFailure(t *testing.T) {
	store := newMemStore(1, 8)
	drift := errors.New("source snapshot drift")
	store.errAt[0] = drift
	senderEnd, receiverEnd := newPipe(4)
	senderEnd.failNextSend.Store(true)
	_, done := startSendSession(t, senderEnd, store, &fakeCodec{})

	req, _ := EncodeRequest([]uint64{0})
	if err := receiverEnd.Send(context.Background(), req); err != nil {
		t.Fatalf("send request: %v", err)
	}
	err := waitErr(t, done, 2*time.Second)
	if !errors.Is(err, drift) || !errors.Is(err, ErrTerminalDelivery) {
		t.Fatalf("Run = %v, want drift and ErrTerminalDelivery", err)
	}
}

// 超长错误消息在转 ERROR 帧时截断,通知仍可送达。
func TestSendSessionTruncatesLongErrorMsg(t *testing.T) {
	store := newMemStore(1, 10)
	store.errAt[0] = errors.New(strings.Repeat("漂", MaxErrorMsgBytes))
	senderEnd, receiverEnd := newPipe(4)
	startSendSession(t, senderEnd, store, &fakeCodec{})

	req, _ := EncodeRequest([]uint64{0})
	_ = receiverEnd.Send(context.Background(), req)
	msg := recvMsg(t, receiverEnd, 2*time.Second)
	e, ok := msg.(*Error)
	if !ok || e.Code != ErrCodeBlockRead {
		t.Fatalf("expected ERROR(BlockRead), got %#v", msg)
	}
	if len(e.Msg) > MaxErrorMsgBytes || !utf8.ValidString(e.Msg) {
		t.Errorf("terminal message len/UTF-8 = %d/%t", len(e.Msg), utf8.ValidString(e.Msg))
	}
	if want := MaxErrorMsgBytes - MaxErrorMsgBytes%len("漂"); len(e.Msg) != want {
		t.Errorf("terminal message length = %d, want rune boundary %d", len(e.Msg), want)
	}
}

func TestSendSessionNormalizesInvalidUTF8BeforeBoundaryTruncation(t *testing.T) {
	const normalizedHead = "bad\uFFFD-"
	filler := strings.Repeat("x", MaxErrorMsgBytes-2-len(normalizedHead))
	want := normalizedHead + filler
	raw := "bad" + string([]byte{0xff}) + "-" + filler + string([]byte{0xfe}) + "tail"
	sourceErr := errors.New(raw)

	store := newMemStore(1, 10)
	store.errAt[0] = sourceErr
	senderEnd, receiverEnd := newPipe(4)
	_, done := startSendSession(t, senderEnd, store, &fakeCodec{})

	req, _ := EncodeRequest([]uint64{0})
	if err := receiverEnd.Send(context.Background(), req); err != nil {
		t.Fatalf("send request: %v", err)
	}
	msg := recvMsg(t, receiverEnd, 2*time.Second)
	e, ok := msg.(*Error)
	if !ok || e.Code != ErrCodeBlockRead {
		t.Fatalf("expected ERROR(BlockRead), got %#v", msg)
	}
	if e.Msg != want || !utf8.ValidString(e.Msg) {
		t.Fatalf("normalized terminal message mismatch: len=%d valid=%t", len(e.Msg), utf8.ValidString(e.Msg))
	}
	if err := waitErr(t, done, 2*time.Second); !errors.Is(err, sourceErr) {
		t.Fatalf("Run = %v, want original source error", err)
	}
}

func TestSendSessionSealErrorBecomesErrorFrame(t *testing.T) {
	codec := &fakeCodec{sealErr: errors.New("per-segment seal limit reached")}
	store := newMemStore(1, 50)
	senderEnd, receiverEnd := newPipe(64)
	_, done := startSendSession(t, senderEnd, store, codec)

	req, _ := EncodeRequest([]uint64{0})
	_ = receiverEnd.Send(context.Background(), req)
	msg := recvMsg(t, receiverEnd, 2*time.Second)
	if e, ok := msg.(*Error); !ok || e.Code != ErrCodeSeal {
		t.Fatalf("expected ERROR(Seal), got %#v", msg)
	}
	if err := waitErr(t, done, 2*time.Second); !errors.Is(err, codec.sealErr) {
		t.Errorf("Run = %v", err)
	}
}

func TestSendSessionRejectsPeerViolations(t *testing.T) {
	tests := []struct {
		name  string
		frame func() Frame
	}{
		{"块号越界", func() Frame {
			f, _ := EncodeRequest([]uint64{99})
			return f
		}},
		{"畸形帧", func() Frame { return Frame{0x7F, 1, 2} }},
		{"错位帧型(BLOCK)", func() Frame {
			f, _ := EncodeBlock(Block{Index: 0, Seq: 0, Last: true, Payload: []byte{1}})
			return f
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			senderEnd, receiverEnd := newPipe(64)
			_, done := startSendSession(t, senderEnd, newMemStore(2, 10), &fakeCodec{})
			_ = receiverEnd.Send(context.Background(), tt.frame())
			msg := recvMsg(t, receiverEnd, 2*time.Second)
			if e, ok := msg.(*Error); !ok || e.Code != ErrCodeBadRequest {
				t.Fatalf("expected ERROR(BadRequest), got %#v", msg)
			}
			if err := waitErr(t, done, 2*time.Second); !errors.Is(err, ErrPeerViolation) {
				t.Errorf("Run = %v, want ErrPeerViolation", err)
			}
		})
	}
}

// 收到对端 ERROR:原样上抛终止。
func TestSendSessionStopsOnPeerError(t *testing.T) {
	senderEnd, receiverEnd := newPipe(64)
	_, done := startSendSession(t, senderEnd, newMemStore(1, 10), &fakeCodec{})
	f, _ := EncodeError(ErrCodeBadRequest, "receiver 放弃")
	_ = receiverEnd.Send(context.Background(), f)
	err := waitErr(t, done, 2*time.Second)
	var e *Error
	if !errors.As(err, &e) || e.Code != ErrCodeBadRequest {
		t.Errorf("Run = %v, want *Error(BadRequest)", err)
	}
}

func TestSendSessionLifecycle(t *testing.T) {
	checkNoLeak(t)
	t.Run("ctx 取消", func(t *testing.T) {
		senderEnd, _ := newPipe(4)
		ss, _ := NewSendSession(senderEnd, newMemStore(1, 10), &fakeCodec{})
		ctx, cancel := context.WithCancel(context.Background())
		done := mustRun(ss, ctx)
		cancel()
		if err := waitErr(t, done, 2*time.Second); !errors.Is(err, context.Canceled) {
			t.Errorf("Run = %v", err)
		}
	})
	t.Run("Close 幂等", func(t *testing.T) {
		senderEnd, _ := newPipe(4)
		ss, _ := NewSendSession(senderEnd, newMemStore(1, 10), &fakeCodec{})
		done := mustRun(ss, context.Background())
		if err := ss.Close(); err != nil {
			t.Errorf("Close 1: %v", err)
		}
		if err := ss.Close(); err != nil {
			t.Errorf("Close 2: %v", err)
		}
		if err := waitErr(t, done, 2*time.Second); !errors.Is(err, ErrSessionClosed) {
			t.Errorf("Run = %v", err)
		}
	})
	t.Run("Run 只跑一次", func(t *testing.T) {
		senderEnd, _ := newPipe(4)
		ss, _ := NewSendSession(senderEnd, newMemStore(1, 10), &fakeCodec{})
		_ = ss.Close()
		done := mustRun(ss, context.Background())
		_ = waitErr(t, done, 2*time.Second)
		if err := ss.Run(context.Background()); !errors.Is(err, ErrSessionReused) {
			t.Errorf("second Run = %v", err)
		}
	})
}

func TestNewSendSessionValidation(t *testing.T) {
	ch, _ := newPipe(1)
	store := newMemStore(1, 1)
	codec := &fakeCodec{}
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"nil ch", func() error { _, err := NewSendSession(nil, store, codec); return err }()},
		{"nil store", func() error { _, err := NewSendSession(ch, nil, codec); return err }()},
		{"nil sealer", func() error { _, err := NewSendSession(ch, store, nil); return err }()},
	} {
		if !errors.Is(tt.err, ErrNilDependency) {
			t.Errorf("%s: %v", tt.name, tt.err)
		}
	}
}
