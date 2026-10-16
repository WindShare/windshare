package v2endpoint

import (
	"context"
	"errors"
	"time"

	"github.com/windshare/windshare/internal/connectionliveness"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/relay/signaling/v2route"
)

var ErrLiveness = connectionliveness.ErrSilent

// LivenessTrace records why the relay closed a connection that stopped
// delivering accepted frames, including probes its client sends while idle.
type LivenessTrace struct {
	Connection v2route.ConnectionRef
	Silence    time.Duration
	Timeout    time.Duration
}
type LivenessTracer interface{ TraceLiveness(LivenessTrace) }
type LivenessTraceFunc func(LivenessTrace)

func (f LivenessTraceFunc) TraceLiveness(event LivenessTrace) {
	if f != nil {
		f(event)
	}
}

// The relay never originates probes: clients probe on a fixed cadence and the
// acknowledgements keep the reverse direction active. Only silence ends a peer.
func (s *Server) livenessLoop(ctx context.Context, peer *connection) error {
	silence, err := connectionliveness.Watch(ctx, peer.liveness, s.livenessTimeout)
	if !errors.Is(err, ErrLiveness) {
		return err
	}
	if s.livenessTracer != nil {
		s.livenessTracer.TraceLiveness(LivenessTrace{Connection: peer.ref, Silence: silence, Timeout: s.livenessTimeout})
	}
	peer.requestClose()
	return err
}

func (s *Server) answerConnectionProbe(source *connection, encoded []byte) error {
	probe, err := v2.ParseConnectionProbe(encoded)
	// Only the read loop touches lastProbe. Increasing nonces let one pending
	// acknowledgement stand for every earlier probe without ambiguity.
	if err != nil || probe.Nonce <= source.lastProbe {
		return ErrProtocol
	}
	source.lastProbe = probe.Nonce
	// Coalescing keeps the reader free of write waits and bounds a probe flood
	// to one pending acknowledgement, which bypasses session forwarding.
	source.probeAck.Store(probe.Nonce)
	select {
	case source.wake <- struct{}{}:
	default:
	}
	return nil
}

func (peer *connection) takeProbeAck() ([]byte, bool) {
	nonce := peer.probeAck.Swap(0)
	if nonce == 0 {
		return nil, false
	}
	encoded, _ := v2.ConnectionProbeAck{Nonce: nonce}.MarshalBinary()
	return encoded, true
}
