package v2endpoint

import (
	"context"
	"errors"
	"time"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/relay/signaling/v2route"
)

const (
	connectionAdmissionTimeout = 10 * time.Second
	admissionFirstFrame        = "first_frame"
	admissionStopProof         = "stop_proof"
	admissionRegistration      = "registration"
	admissionStarted           = "started"
	admissionExpired           = "expired"
	admissionFailed            = "failed"
	admissionCompleted         = "completed"
)

type AdmissionTrace struct {
	Connection v2route.ConnectionRef
	SessionID  v2.RelaySessionID
	Phase      string
	Outcome    string
}

type AdmissionTracer interface{ TraceAdmission(AdmissionTrace) }
type AdmissionTraceFunc func(AdmissionTrace)

func (f AdmissionTraceFunc) TraceAdmission(event AdmissionTrace) {
	if f != nil {
		f(event)
	}
}

func (s *Server) traceAdmission(peer *connection, id v2.RelaySessionID, phase, outcome string) {
	if s.admissionTracer != nil {
		s.admissionTracer.TraceAdmission(AdmissionTrace{
			Connection: peer.ref, SessionID: id, Phase: phase, Outcome: outcome,
		})
	}
}

func admissionOutcome(ctx context.Context, err error) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return admissionExpired
	}
	if err != nil {
		return admissionFailed
	}
	return admissionCompleted
}

func (s *Server) watchAdmission(ctx context.Context, peer *connection, id v2.RelaySessionID) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	timer := time.NewTimer(v2route.SessionAdmissionTimeout)
	s.traceAdmission(peer, id, string(v2route.SessionAwaitingReceiver), admissionStarted)
	go func() {
		defer close(done)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-peer.admitted:
		case <-timer.C:
			retirement, phase, expired := s.registry.ExpireAdmission(id, peer.ref)
			if expired {
				s.traceAdmission(peer, id, string(phase), admissionExpired)
				s.applySessionRetirement(retirement)
				peer.requestClose()
			}
		}
	}()
	return func() { cancel(); <-done }
}

func (s *Server) admitSession(source *connection, encoded []byte) error {
	if source.roleValue() != roleSender {
		return ErrProtocol
	}
	frame, err := v2.ParseSessionAdmitted(encoded)
	if err != nil {
		return ErrProtocol
	}
	resolution, err := s.registry.ResolveSession(frame.RelaySessionID, source.ref)
	if err != nil {
		return ErrProtocol
	}
	if resolution.Disposition == v2route.SessionRetired {
		return nil
	}
	err = s.registry.AdmitSession(frame.RelaySessionID, source.ref)
	switch {
	case errors.Is(err, v2route.ErrSessionEnded):
		return nil
	case errors.Is(err, v2route.ErrAdmissionExpired):
		s.retireExpiredAdmission(source, frame.RelaySessionID)
		return nil
	case err != nil:
		return ErrProtocol
	}
	if receiver, _, _ := s.connections.resolve(resolution.Destination); receiver != nil {
		receiver.admitOnce.Do(func() {
			s.traceAdmission(receiver, frame.RelaySessionID, string(v2route.SessionActive), admissionCompleted)
			close(receiver.admitted)
		})
	}
	return nil
}

func (s *Server) retireExpiredAdmission(source *connection, id v2.RelaySessionID) {
	retirement, ended := s.endSession(id, source.ref)
	if !ended {
		return
	}
	receiver, _, _ := s.connections.resolve(retirement.Receiver)
	if receiver == nil {
		return
	}
	s.traceAdmission(receiver, id, string(v2route.SessionAwaitingSender), admissionExpired)
	receiver.requestClose()
}
