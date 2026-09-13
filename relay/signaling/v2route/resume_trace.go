package v2route

import (
	"errors"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

type ResumePhase string

const (
	ResumeCredentialPhase ResumePhase = "credential"
	ResumeCommitPhase     ResumePhase = "commit"
)

type ResumeTrace struct {
	ShareID                 v2.ShareID
	ShareInstance           v2.ShareInstance
	Phase                   ResumePhase
	Outcome                 string
	ExpectedGeneration      uint64
	CurrentGeneration       uint64
	ExpectedOwnerGeneration uint64
	NewOwnerGeneration      uint64
	RetiredSessions         int
	Err                     error
}

type ResumeTracer interface{ TraceResume(ResumeTrace) }
type ResumeTraceFunc func(ResumeTrace)

func (f ResumeTraceFunc) TraceResume(event ResumeTrace) {
	if f != nil {
		f(event)
	}
}

func (r *Registry) traceResume(event ResumeTrace, err error) {
	if r.resumeTracer == nil {
		return
	}
	event.Err = err
	switch {
	case err == nil:
		event.Outcome = "accepted"
	case errors.Is(err, ErrNotFound):
		event.Outcome = "absent"
	case errors.Is(err, ErrStarting):
		event.Outcome = "starting"
	case errors.Is(err, ErrResumeStale):
		event.Outcome = "stale"
	case errors.Is(err, ErrStopped):
		event.Outcome = "stopped"
	case errors.Is(err, ErrStopping):
		event.Outcome = "stopping"
	case errors.Is(err, ErrResume):
		event.Outcome = "invalid_credential"
	default:
		event.Outcome = "failed"
	}
	// Diagnostics run after ownership locks are released, including rejected
	// commits, so tracing cannot block routing or deadlock a reentrant observer.
	r.resumeTracer.TraceResume(event)
}
