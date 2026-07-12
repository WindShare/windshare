package relay

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/channeltest"
	"github.com/windshare/windshare/relay/forward"
	"github.com/windshare/windshare/relay/protocol"
)

type conformanceWriter struct {
	sid  protocol.SessionID
	sent chan channeltest.SentFrame

	mu      sync.Mutex
	blocked bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	relOnce sync.Once
}

func newConformanceWriter(sid protocol.SessionID) *conformanceWriter {
	return &conformanceWriter{
		sid:     sid,
		sent:    make(chan channeltest.SentFrame, 8),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (*conformanceWriter) WriteText([]byte) error { return nil }

func (w *conformanceWriter) WriteBinary(data []byte) error {
	w.mu.Lock()
	blocked := w.blocked
	w.mu.Unlock()
	if blocked {
		w.once.Do(func() {
			close(w.entered)
			<-w.release
		})
	}

	var (
		sid      protocol.SessionID
		frame    []byte
		terminal bool
		err      error
	)
	switch protocol.BinType(data) {
	case protocol.BinTypeForward:
		sid, frame, err = protocol.DecodeForwardFrame(data)
	case protocol.BinTypeTerminalForward:
		terminal = true
		sid, frame, err = protocol.DecodeTerminalForwardFrame(data)
	default:
		err = fmt.Errorf("unexpected binary envelope type 0x%02x", protocol.BinType(data))
	}
	if err != nil {
		return err
	}
	if sid != w.sid {
		return fmt.Errorf("envelope session = %s, want %s", sid, w.sid)
	}
	w.sent <- channeltest.SentFrame{
		Frame:    append(session.Frame(nil), frame...),
		Terminal: terminal,
	}
	return nil
}

func (w *conformanceWriter) block() { w.mu.Lock(); w.blocked = true; w.mu.Unlock() }

func (w *conformanceWriter) unblock() { w.relOnce.Do(func() { close(w.release) }) }

func TestRelayChannelConformance(t *testing.T) {
	channeltest.Run(t, func(tb testing.TB) channeltest.Fixture {
		tb.Helper()
		sid := protocol.SessionID{0x44, 0x31}
		writer := newConformanceWriter(sid)
		ctx, cancel := context.WithCancel(context.Background())
		link := &link{ctx: ctx, cancel: cancel}
		link.pump = forward.NewPump(writer, forward.Options{SessionQueueFrames: 1})
		channel := newChannel(sid, link)

		return channeltest.Fixture{
			Channel: channel,
			ReceiveSent: func(ctx context.Context) (channeltest.SentFrame, error) {
				select {
				case sent := <-writer.sent:
					return sent, nil
				case <-ctx.Done():
					return channeltest.SentFrame{}, ctx.Err()
				}
			},
			Deliver: func(frame session.Frame) error {
				switch channel.deliver(frame) {
				case ingressAccepted:
					return nil
				case ingressClosed:
					return ErrChannelClosed
				default:
					return ErrSessionIngressOverflow
				}
			},
			DeliverTerminal: func(frame session.Frame) error {
				switch channel.deliverTerminal(frame) {
				case ingressAccepted:
					return nil
				case ingressClosed:
					return ErrChannelClosed
				default:
					return ErrSessionIngressOverflow
				}
			},
			RemoteClose: func() error {
				channel.shut(nil)
				return nil
			},
			SaturateSends: func(tb testing.TB) {
				tb.Helper()
				writer.block()
				if err := channel.Send(context.Background(), session.Frame{1}); err != nil {
					tb.Fatalf("prime blocked writer: %v", err)
				}
				select {
				case <-writer.entered:
				case <-time.After(2 * time.Second):
					tb.Fatal("writer did not enter the gate")
				}
				if err := channel.Send(context.Background(), session.Frame{2}); err != nil {
					tb.Fatalf("fill bounded queue: %v", err)
				}
			},
			ReleaseSends: writer.unblock,
			Cleanup: func() {
				writer.unblock()
				channel.shut(nil)
				cancel()
				link.pump.Close()
				select {
				case <-link.pump.Done():
				case <-time.After(2 * time.Second):
					tb.Errorf("relay conformance pump did not stop")
				}
			},
		}
	})
}

func TestIngressOverflowErrorCategory(t *testing.T) {
	err := &SessionIngressOverflow{Kind: IngressSignals}
	if !errors.Is(err, ErrSessionIngressOverflow) {
		t.Fatalf("errors.Is(%v, ErrSessionIngressOverflow) = false", err)
	}
}
