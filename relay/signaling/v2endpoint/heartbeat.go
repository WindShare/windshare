package v2endpoint

import (
	"context"
	"errors"

	"github.com/windshare/windshare/internal/websocketheartbeat"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/relay/signaling/v2route"
)

var ErrHeartbeat = websocketheartbeat.ErrFailed

type HeartbeatConfig = websocketheartbeat.Config

type HeartbeatTrace struct {
	Connection v2route.ConnectionRef
	websocketheartbeat.Event
}
type HeartbeatTracer interface{ TraceHeartbeat(HeartbeatTrace) }
type HeartbeatTraceFunc func(HeartbeatTrace)

func (f HeartbeatTraceFunc) TraceHeartbeat(event HeartbeatTrace) {
	if f != nil {
		f(event)
	}
}

func (s *Server) heartbeatLoop(ctx context.Context, peer *connection) error {
	err := websocketheartbeat.Run(ctx, peer.socket, s.heartbeat, func(event websocketheartbeat.Event) {
		if s.heartbeatTracer != nil {
			s.heartbeatTracer.TraceHeartbeat(HeartbeatTrace{Connection: peer.ref, Event: event})
		}
	})
	if errors.Is(err, websocketheartbeat.ErrFailed) {
		peer.requestClose()
	}
	return err
}

func (s *Server) answerConnectionProbe(ctx context.Context, source *connection, encoded []byte) error {
	probe, err := v2.ParseConnectionProbe(encoded)
	if err != nil {
		return ErrProtocol
	}
	ack, _ := v2.ConnectionProbeAck(probe).MarshalBinary()
	// Control writes bypass session routing and its forward queues. A stalled
	// destination therefore cannot prevent this connection proving its liveness.
	heartbeat, _ := s.heartbeat.Normalize()
	responseContext, cancel := context.WithTimeout(ctx, heartbeat.Timeout)
	defer cancel()
	return source.sendControl(responseContext, ack)
}
