package v2endpoint

import (
	"context"
	"errors"
	"time"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/relay/signaling/v2route"
)

type ForwardStage string

const (
	ForwardQueueWait         ForwardStage = "queue_wait"
	ForwardQueueResumed      ForwardStage = "queue_resumed"
	ForwardQueueWaitExpired  ForwardStage = "queue_wait_expired"
	ForwardDestinationClosed ForwardStage = "destination_closed"
	ForwardCreditViolation   ForwardStage = "credit_violation"
	ForwardWindowConstrained ForwardStage = "window_constrained"
	ForwardWindowAvailable   ForwardStage = "window_available"
)

// ForwardTrace identifies pressure at the forwarding owner, before a generic
// session retirement hides which queue or participant caused it.
type ForwardTrace struct {
	Source, Destination v2route.ConnectionRef
	SessionID           v2.RelaySessionID
	Stage               ForwardStage
	Wait                time.Duration
	SessionFrames       int
	SessionBytes        int
	ConnectionFrames    int
	ConnectionBytes     int
	AvailableFrames     int
	AvailableBytes      int
}

type ForwardTracer interface{ TraceForward(ForwardTrace) }
type ForwardTraceFunc func(ForwardTrace)

func (f ForwardTraceFunc) TraceForward(event ForwardTrace) {
	if f != nil {
		f(event)
	}
}

func (s *Server) forwardLoop(ctx context.Context, source *connection) error {
	for {
		encoded, err := readBinary(ctx, source.socket)
		if err != nil {
			return err
		}
		if err := s.forwardFrame(ctx, source, encoded); err != nil {
			return err
		}
	}
}

func (s *Server) forwardFrame(ctx context.Context, source *connection, encoded []byte) error {
	if len(encoded) >= 4 && string(encoded[:4]) == v2.SessionAdmittedMagic {
		return s.admitSession(source, encoded)
	}
	route, err := v2.ParseOpaqueRoute(encoded)
	if err != nil {
		return ErrProtocol
	}
	resolution, err := s.registry.ResolveSession(route.RelaySessionID, source.ref)
	if err != nil {
		return ErrProtocol
	}
	if resolution.Disposition == v2route.SessionRetired {
		return nil
	}
	if resolution.Disposition != v2route.SessionForward || !resolution.Destination.Valid() {
		return ErrProtocol
	}
	if source.roleValue() == roleReceiver {
		if err := s.registry.ObserveReceiverFrame(route.RelaySessionID, source.ref); err != nil {
			return err
		}
	}
	destination, _, _ := s.connections.resolve(resolution.Destination)
	err = s.forwardToDestination(ctx, source, destination, route.RelaySessionID, encoded)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if retirement, ended := s.endSession(route.RelaySessionID, source.ref); ended {
		if receiver, _, _ := s.connections.resolve(retirement.Receiver); receiver != nil {
			receiver.requestClose()
		}
	}
	// A slow or vanished receiver cannot invalidate sibling sessions owned by
	// the same authenticated sender.
	if source.roleValue() == roleSender {
		return nil
	}
	return err
}

func (s *Server) forwardToDestination(
	ctx context.Context, source, destination *connection,
	sessionID v2.RelaySessionID, encoded []byte,
) error {
	if destination == nil {
		return ErrConnection
	}
	if source.roleValue() == roleSender {
		return s.forwardSender(source, destination, sessionID, encoded)
	}
	return s.forwardWithPressure(ctx, source, destination, sessionID, encoded)
}

func (s *Server) forwardSender(source, destination *connection, sessionID v2.RelaySessionID, encoded []byte) error {
	// Sender traffic is multiplexed. Waiting here would stop every sibling;
	// the sender scheduler must reserve this destination's credit first.
	trace, permitted := source.consumeSenderCredit(sessionID, len(encoded))
	trace.Source, trace.Destination = source.ref, destination.ref
	if trace.Stage != "" {
		s.traceForward(trace, trace.Stage)
	}
	if !permitted {
		s.traceForward(trace, ForwardCreditViolation)
		return ErrProtocol
	}
	if accepted, _, _ := destination.tryForward(sessionID, encoded); !accepted {
		s.traceForward(trace, ForwardDestinationClosed)
		return ErrConnection
	}
	return nil
}

func (s *Server) forwardWithPressure(
	ctx context.Context, source, destination *connection,
	sessionID v2.RelaySessionID, encoded []byte,
) error {
	var started time.Time
	var timer *time.Timer
	var deadline <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		accepted, changed, trace := destination.tryForward(sessionID, encoded)
		trace.Source, trace.Destination = source.ref, destination.ref
		trace.SessionID = sessionID
		if !started.IsZero() {
			trace.Wait = time.Since(started)
		}
		if accepted {
			if !started.IsZero() {
				s.traceForward(trace, ForwardQueueResumed)
			}
			return nil
		}
		if changed == nil {
			s.traceForward(trace, ForwardDestinationClosed)
			return ErrConnection
		}
		if started.IsZero() {
			started = time.Now()
			s.traceForward(trace, ForwardQueueWait)
			timeout := s.writeTimeout
			if timeout == 0 {
				timeout = defaultWriteTimeout
			}
			timer = time.NewTimer(timeout)
			deadline = timer.C
		}
		// Stop reading the source while the bounded destination queue is full.
		// This carries TCP backpressure upstream instead of mistaking a fast
		// localhost burst for a dead route and cancelling its P2P negotiation.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			trace.Wait = time.Since(started)
			s.traceForward(trace, ForwardQueueWaitExpired)
			return errors.Join(ErrForwardTimeout, context.DeadlineExceeded)
		case <-changed:
		}
	}
}

func (s *Server) traceForward(event ForwardTrace, stage ForwardStage) {
	if s.forwardTracer != nil {
		event.Stage = stage
		s.forwardTracer.TraceForward(event)
	}
}
