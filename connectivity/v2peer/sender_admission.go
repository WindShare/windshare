package v2peer

import (
	"context"

	"github.com/windshare/windshare/core/session/sessionruntime"
)

type attemptAdmissionPhase uint8

const (
	attemptAdmissionIdle attemptAdmissionPhase = iota
	attemptAdmissionPending
	attemptAdmissionResolved
	attemptAdmissionClosed
)

// beginAdmission and admissionAtTerminal form the arbitration boundary between
// the core lane registry and transport failures. Once core starts admission, a
// competing terminal must await its result: a successful return means the lane
// is already registered and therefore owns the public attempt terminal.
func (attempt *peerAttempt) beginAdmission(
	parent context.Context,
) (context.Context, bool) {
	attempt.admissionMu.Lock()
	defer attempt.admissionMu.Unlock()
	if attempt.admissionPhase != attemptAdmissionIdle {
		return nil, false
	}
	admissionContext, cancel := context.WithCancelCause(parent)
	attempt.admissionCancel = cancel
	attempt.admissionPhase = attemptAdmissionPending
	return admissionContext, true
}

func (attempt *peerAttempt) resolveAdmission(lane sessionruntime.LaneIdentity, err error) {
	attempt.admissionMu.Lock()
	if attempt.admissionPhase != attemptAdmissionPending {
		attempt.admissionMu.Unlock()
		return
	}
	attempt.admissionResult = attemptEvent{kind: attemptAdmission, lane: lane, err: err}
	attempt.admissionPhase = attemptAdmissionResolved
	cancel := attempt.admissionCancel
	attempt.admissionCancel = nil
	close(attempt.admissionDone)
	attempt.admissionMu.Unlock()
	if cancel != nil {
		cancel(err)
	}
}

func (attempt *peerAttempt) admissionAtTerminal(cause error) (attemptEvent, bool) {
	attempt.admissionMu.Lock()
	switch attempt.admissionPhase {
	case attemptAdmissionIdle:
		attempt.admissionPhase = attemptAdmissionClosed
		attempt.admissionMu.Unlock()
		return attemptEvent{}, false
	case attemptAdmissionClosed:
		attempt.admissionMu.Unlock()
		return attemptEvent{}, false
	case attemptAdmissionResolved:
		result := attempt.admissionResult
		attempt.admissionMu.Unlock()
		return result, true
	case attemptAdmissionPending:
		done := attempt.admissionDone
		cancel := attempt.admissionCancel
		attempt.admissionMu.Unlock()
		if cause == nil {
			cause = context.Canceled
		}
		// Cancellation asks the in-flight core admission to settle; it does not
		// pre-judge that settlement. A core success that races this cancellation
		// is still an already-registered lane and must remain admitted evidence.
		if cancel != nil {
			cancel(cause)
		}
		<-done
		attempt.admissionMu.Lock()
		result := attempt.admissionResult
		attempt.admissionMu.Unlock()
		return result, true
	default:
		attempt.admissionMu.Unlock()
		return attemptEvent{}, false
	}
}

func (execution *attemptExecution) settleAdmissionAtTerminal(cause error) (bool, error) {
	result, started := execution.attempt.admissionAtTerminal(cause)
	if !started {
		return false, nil
	}
	execution.attempt.recorder.complete(
		SenderAttemptDataChannelOpen,
		execution.candidateCounts(),
		nil,
		nil,
	)
	if err := execution.acceptAdmission(result); err != nil {
		return false, err
	}
	return true, nil
}
