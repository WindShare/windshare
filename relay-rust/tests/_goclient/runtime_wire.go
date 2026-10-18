package main

import (
	"context"
	"net/http"
	"sync"

	"github.com/coder/websocket"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

type runtimeCounts struct {
	received v2.DeliveryConsumed
	consumed v2.DeliveryConsumed
	notice   *v2.SettlementNotice
	settled  *v2.SettlementAck
}

// Observe the real client's socket without driving its grants, feedback or
// settlement. The test must fail if the production runtime strands capacity.
type runtimeWire struct {
	mu      sync.Mutex
	flows   map[v2.RelaySessionID]runtimeCounts
	changed chan struct{}
}

func newRuntimeWire() *runtimeWire {
	return &runtimeWire{flows: make(map[v2.RelaySessionID]runtimeCounts), changed: make(chan struct{}, 1)}
}

type observedSocket struct {
	*websocket.Conn
	wire *runtimeWire
}

func (w *runtimeWire) dial(ctx context.Context, target string, header http.Header) (relayv2.BinarySocket, error) {
	socket, _, err := websocket.Dial(ctx, target, &websocket.DialOptions{HTTPHeader: header}) //nolint:bodyclose
	if err != nil {
		return nil, err
	}
	return &observedSocket{Conn: socket, wire: w}, nil
}

func (s *observedSocket) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	kind, frame, err := s.Conn.Read(ctx)
	if err == nil {
		s.wire.observe(frame, false)
	}
	return kind, frame, err
}

func (s *observedSocket) Write(ctx context.Context, kind websocket.MessageType, frame []byte) error {
	err := s.Conn.Write(ctx, kind, frame)
	if err == nil {
		s.wire.observe(frame, true)
	}
	return err
}

func (w *runtimeWire) observe(frame []byte, outbound bool) {
	if len(frame) < 4 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if outbound {
		switch string(frame[:4]) {
		case v2.DeliveryConsumedMagic:
			feedback, err := v2.ParseDeliveryConsumed(frame)
			check(err)
			state := w.flows[feedback.RelaySessionID]
			require(feedback.TotalFrames >= state.consumed.TotalFrames &&
				feedback.TotalEncodedBytes >= state.consumed.TotalEncodedBytes, "runtime consumption regressed")
			state.consumed = feedback
			w.flows[feedback.RelaySessionID] = state
		case v2.SettlementAckMagic:
			ack, err := v2.ParseSettlementAck(frame)
			check(err)
			state := w.flows[ack.RelaySessionID]
			require(state.notice != nil && ack.TotalFrames == state.notice.TotalFrames &&
				ack.TotalEncodedBytes == state.notice.TotalEncodedBytes, "runtime acknowledged the wrong frozen prefix")
			state.settled = &ack
			w.flows[ack.RelaySessionID] = state
		default:
			return
		}
	} else {
		switch string(frame[:4]) {
		case "WS2O":
			route, err := v2.ParseOpaqueRoute(frame)
			check(err)
			state := w.flows[route.RelaySessionID]
			state.received.TotalFrames++
			state.received.TotalEncodedBytes += uint64(len(frame))
			w.flows[route.RelaySessionID] = state
		case v2.SettlementNoticeMagic:
			notice, err := v2.ParseSettlementNotice(frame)
			check(err)
			state := w.flows[notice.RelaySessionID]
			require(notice.TotalFrames == state.received.TotalFrames &&
				notice.TotalEncodedBytes == state.received.TotalEncodedBytes, "notice overtook submitted envelopes")
			state.notice = &notice
			w.flows[notice.RelaySessionID] = state
		default:
			return
		}
	}
	select {
	case w.changed <- struct{}{}:
	default:
	}
}

func (w *runtimeWire) state(id v2.RelaySessionID) runtimeCounts {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flows[id]
}

func (w *runtimeWire) until(ctx context.Context, id v2.RelaySessionID, ready func(runtimeCounts) bool) {
	for !ready(w.state(id)) {
		select {
		case <-w.changed:
		case <-ctx.Done():
			panic("runtime delivery observation timed out: " + ctx.Err().Error())
		}
	}
}
