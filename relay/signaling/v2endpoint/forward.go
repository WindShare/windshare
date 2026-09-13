package v2endpoint

import (
	"context"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/relay/signaling/v2route"
)

type ForwardStage string

const (
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
	if len(encoded) >= 4 && string(encoded[:4]) == v2.ConnectionProbeMagic {
		return s.answerConnectionProbe(ctx, source, encoded)
	}
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
	err = s.forwardToDestination(source, destination, route.RelaySessionID, encoded)
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
	source, destination *connection,
	sessionID v2.RelaySessionID, encoded []byte,
) error {
	if destination == nil {
		return ErrConnection
	}
	return s.forwardCredited(source, destination, sessionID, encoded)
}

func (s *Server) forwardCredited(source, destination *connection, sessionID v2.RelaySessionID, encoded []byte) error {
	// Neither role may pause the socket reader behind productive data pressure:
	// probes and native Ping/Pong need that reader even during an idle transfer.
	// Source credit reserves bounded destination storage before each write.
	trace, permitted := source.consumeForwardCredit(sessionID, len(encoded))
	trace.Source, trace.Destination = source.ref, destination.ref
	if trace.Stage != "" {
		s.traceForward(trace, trace.Stage)
	}
	if !permitted {
		s.traceForward(trace, ForwardCreditViolation)
		return ErrProtocol
	}
	if accepted, _ := destination.tryForward(sessionID, encoded); !accepted {
		s.traceForward(trace, ForwardDestinationClosed)
		return ErrConnection
	}
	return nil
}

func (s *Server) traceForward(event ForwardTrace, stage ForwardStage) {
	if s.forwardTracer != nil {
		event.Stage = stage
		s.forwardTracer.TraceForward(event)
	}
}
