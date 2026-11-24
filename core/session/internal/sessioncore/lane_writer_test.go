package sessioncore

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type testPassThroughSealer struct {
	seq atomic.Uint64
}

func (s *testPassThroughSealer) Seal(plaintext []byte) (protocolsession.SealedEnvelope, error) {
	n := s.seq.Add(1)
	return protocolsession.SealedEnvelope{Sequence: n, Frame: append([]byte(nil), plaintext...)}, nil
}

func newTestLaneWriterMessage(t *testing.T, kind protocolsession.MessageKind, opByte byte, body []byte) protocolsession.Message {
	t.Helper()
	if kind == protocolsession.MessageBlockFragment {
		fragments, err := contentflow.FragmentRecord(protocolsession.OperationID(opByte), body)
		if err != nil {
			t.Fatal(err)
		}
		return fragments[0]
	}
	var idPtr *protocolsession.OperationID
	if kind != protocolsession.MessageSessionTerminal && !kind.IsSessionControl() {
		id := protocolsession.OperationID(opByte)
		idPtr = &id
	}
	msg, err := protocolsession.NewMessage(kind, idPtr, body)
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func TestLaneWriterPriorityBurstFairnessAndTerminalPreemption(t *testing.T) {
	base, peer := newMemoryChannelPair()
	defer base.Close()
	defer peer.Close()

	writer, err := newLaneWriter(base, &testPassThroughSealer{})
	if err != nil {
		t.Fatal(err)
	}

	// Queue > MaximumControlBurst control frames and 2 data frames before starting
	// the writer so scheduling order is deterministic.
	for i := range protocolsession.MaximumControlBurst + 2 {
		msg := newTestLaneWriterMessage(t, protocolsession.MessageListChildren, byte(i+1), []byte{0xa0})
		if err := writer.EnqueueControl(msg, nil); err != nil {
			t.Fatalf("enqueue control %d: %v", i, err)
		}
	}
	for i := range 2 {
		msg := newTestLaneWriterMessage(t, protocolsession.MessageBlockFragment, byte(50+i), []byte{0xa0})
		if _, err := writer.enqueue(writeClassData, msg, nil); err != nil {
			t.Fatalf("enqueue data %d: %v", i, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- writer.Run(ctx) }()

	received := make([]protocolsession.MessageKind, 0, protocolsession.MaximumControlBurst+4)
	for range protocolsession.MaximumControlBurst + 4 {
		select {
		case frame := <-peer.Recv():
			msg, decErr := protocolsession.DecodeMessage(frame)
			if decErr != nil {
				t.Fatal(decErr)
			}
			received = append(received, msg.Kind())
		case <-time.After(time.Second):
			t.Fatal("timed out draining scheduled frames")
		}
	}
	// First MaximumControlBurst must be control, followed immediately by a data frame
	// for burst fairness.
	for i := range protocolsession.MaximumControlBurst {
		if received[i] != protocolsession.MessageListChildren {
			t.Fatalf("slot %d = kind %d, want control", i, received[i])
		}
	}
	if received[protocolsession.MaximumControlBurst] != protocolsession.MessageBlockFragment {
		t.Fatalf("burst yield slot = kind %d, want data", received[protocolsession.MaximumControlBurst])
	}
	cancel()
	<-done
}

func TestLaneWriterCancelQueuedVsInFlightPreservesAEADMonotonicity(t *testing.T) {
	base, peer := newMemoryChannelPair()
	defer base.Close()
	defer peer.Close()
	blocking := &blockingRuntimeLaneChannel{
		memoryChannel: base, started: make(chan struct{}), release: make(chan struct{}),
	}
	sealer := &testPassThroughSealer{}
	writer, err := newLaneWriter(blocking, sealer)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- writer.Run(ctx) }()

	// First send enters transport and blocks.
	var firstDone atomic.Int32
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstErrCh := make(chan error, 1)
	go func() {
		firstErrCh <- writer.SendControl(
			firstCtx,
			newTestLaneWriterMessage(t, protocolsession.MessageListChildren, 1, []byte{0xa0}),
			func(deliveredOrUncertain bool) {
				if deliveredOrUncertain {
					firstDone.Store(1)
				} else {
					firstDone.Store(-1)
				}
			},
		)
	}()
	<-blocking.started

	// Second send sits queued behind the in-flight send; canceling it must withdraw
	// it before sealing (NotSent == true, onDone(false)).
	var secondDone atomic.Int32
	secondCtx, cancelSecond := context.WithCancel(context.Background())
	secondErrCh := make(chan error, 1)
	go func() {
		secondErrCh <- writer.SendControl(
			secondCtx,
			newTestLaneWriterMessage(t, protocolsession.MessageListChildren, 2, []byte{0xa0}),
			func(deliveredOrUncertain bool) {
				if deliveredOrUncertain {
					secondDone.Store(1)
				} else {
					secondDone.Store(-1)
				}
			},
		)
	}()
	// Wait until second entry is queued.
	for {
		writer.mu.Lock()
		queued := len(writer.control)
		writer.mu.Unlock()
		if queued == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancelSecond()
	secondErr := <-secondErrCh
	if !errors.Is(secondErr, context.Canceled) || !protocolsession.IsNotSent(secondErr) {
		t.Fatalf("queued cancel err=%v notSent=%v", secondErr, protocolsession.IsNotSent(secondErr))
	}
	if secondDone.Load() != -1 {
		t.Fatalf("queued cancel onDone=%d, want -1 (false)", secondDone.Load())
	}

	// Cancel the in-flight send: caller wakes with unmarked context.Canceled
	// (NotSent == false), while physical send still completes on lane lifetime.
	cancelFirst()
	firstErr := <-firstErrCh
	if !errors.Is(firstErr, context.Canceled) || protocolsession.IsNotSent(firstErr) {
		t.Fatalf("in-flight cancel err=%v notSent=%v", firstErr, protocolsession.IsNotSent(firstErr))
	}
	close(blocking.release)
	select {
	case <-peer.Recv():
	case <-time.After(time.Second):
		t.Fatal("in-flight sealed frame never finished on lane lifetime")
	}
	if sealer.seq.Load() != 1 {
		t.Fatalf("sealer sequences=%d, want 1 (withdrawn frame must not seal)", sealer.seq.Load())
	}
	cancel()
	<-runDone
	if firstDone.Load() != 1 {
		t.Fatalf("in-flight completion onDone=%d, want 1 (true)", firstDone.Load())
	}
}

func TestLaneWriterTerminalPreemptsQueuedAndRejectsSubsequentSends(t *testing.T) {
	base, peer := newMemoryChannelPair()
	defer base.Close()
	defer peer.Close()
	writer, err := newLaneWriter(base, &testPassThroughSealer{})
	if err != nil {
		t.Fatal(err)
	}
	var drained atomic.Int32
	msg := newTestLaneWriterMessage(t, protocolsession.MessageListChildren, 1, []byte{0xa0})
	if err := writer.EnqueueControl(msg, func(delivered bool) {
		if !delivered {
			drained.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	dataMsg := newTestLaneWriterMessage(t, protocolsession.MessageBlockFragment, 2, []byte{0xa0})
	if _, err := writer.enqueue(writeClassData, dataMsg, func(delivered bool) {
		if !delivered {
			drained.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if writer.DeliveringTerminal() {
		t.Fatal("writer reported DeliveringTerminal before terminal enqueue")
	}
	termMsg := newTestLaneWriterMessage(t, protocolsession.MessageSessionTerminal, 0, []byte{0xa0})
	termSend, err := writer.EnqueueTerminal(termMsg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if drained.Load() != 2 {
		t.Fatalf("terminal drained %d callbacks, want 2", drained.Load())
	}
	if writer.Accepting() || !writer.Active() || !writer.DeliveringTerminal() {
		t.Fatalf(
			"post-enqueue writer states accepting=%t active=%t terminal=%t, want false/true/true",
			writer.Accepting(), writer.Active(), writer.DeliveringTerminal(),
		)
	}
	if err := writer.SendControl(context.Background(), msg, nil); !errors.Is(err, protocolsession.ErrWriterTerminal) || !protocolsession.IsNotSent(err) {
		t.Fatalf("post-terminal control err=%v", err)
	}
	if err := writer.SendData(context.Background(), msg, nil); !errors.Is(err, protocolsession.ErrMessageClass) {
		t.Fatalf("wrong class err=%v", err)
	}
	runDone := make(chan error, 1)
	go func() { runDone <- writer.Run(context.Background()) }()
	res := termSend.Await(context.Background())
	if !res.settled || res.err != nil || res.disposition != framechannel.SendAccepted {
		t.Fatalf("terminal await=%+v", res)
	}
	if err := <-runDone; err != nil {
		t.Fatalf("writer run after terminal=%v", err)
	}
	if writer.Accepting() || writer.Active() || writer.DeliveringTerminal() {
		t.Fatalf(
			"stopped writer states accepting=%t active=%t terminal=%t, want false/false/false",
			writer.Accepting(), writer.Active(), writer.DeliveringTerminal(),
		)
	}
	// Only the terminal frame must have crossed the wire.
	select {
	case frame := <-peer.Recv():
		decoded, decErr := protocolsession.DecodeMessage(frame)
		if decErr != nil || decoded.Kind() != protocolsession.MessageSessionTerminal {
			t.Fatalf("received frame=%+v err=%v", decoded, decErr)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal frame missing")
	}
	select {
	case extra := <-peer.Recv():
		t.Fatalf("preempted frame leaked onto wire: %x", extra)
	default:
	}
	// Verify concurrent Run rejection.
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := writer.Run(context.Background()); !errors.Is(err, protocolsession.ErrWriterStopped) {
			t.Errorf("second Run err=%v", err)
		}
	})
	wg.Wait()
}
