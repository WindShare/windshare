package sessioncore

import (
	"context"
	"errors"

	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
	"time"
)

func (client *Requests) newCall(
	ctx context.Context,
	id protocolsession.OperationID,
	kind protocolsession.MessageKind,
) *Call {
	traceEnabled := client.runtime.protocolOperationTracingEnabled()
	if !traceEnabled {
		return newOperationCall(id, kind, time.Time{}, 0, false, false)
	}
	started := client.runtime.now()
	deadlineMillis, hasDeadline := remainingDeadlineMillis(ctx, started)
	return newOperationCall(id, kind, started, deadlineMillis, hasDeadline, true)
}

func newOperationCall(
	id protocolsession.OperationID,
	kind protocolsession.MessageKind,
	started time.Time,
	deadlineMillis uint64,
	hasDeadline bool,
	traceEnabled bool,
) *Call {
	return &Call{
		id: id, requestKind: kind, traceEnabled: traceEnabled, traceStarted: started,
		traceDeadlineMillis: deadlineMillis, traceHasDeadline: hasDeadline,
		messages: make(chan operationResponse, operationResponseFrames), done: make(chan struct{}),
	}
}

func (call *Call) setProtocolTraceLane(lane sessioncontract.LaneIdentity, usable uint32) {
	if call == nil {
		return
	}
	call.laneMu.Lock()
	call.lane = lane
	call.laneMu.Unlock()
	if !call.traceEnabled {
		return
	}
	call.stateMu.Lock()
	call.traceUsableAtSelection = usable
	call.stateMu.Unlock()
}

func (call *Call) recordProtocolTraceSend(completion protocolsession.SendCompletion) {
	if call == nil || !call.traceEnabled {
		return
	}
	call.stateMu.Lock()
	call.traceHasSend = true
	call.traceSendSettled = completion.Settled
	call.traceSendAdmitted = completion.Admitted
	call.traceSendOutcome = completion.Outcome
	call.stateMu.Unlock()
}

func (call *Call) recordProtocolTraceFailure(err error) {
	if call == nil || !call.traceEnabled || err == nil {
		return
	}
	if timeout, ok := errors.AsType[*contentflow.BlockWaitTimeout](err); ok {
		call.stateMu.Lock()
		call.traceBlockWait = *timeout
		call.stateMu.Unlock()
	}
	call.recordProtocolTraceCause(protocolOperationCause(err))
}

func (call *Call) recordProtocolTraceCleanup(err error) {
	if call == nil || !call.traceEnabled || err == nil {
		return
	}
	cause := protocolOperationCause(err)
	// Cleanup is synchronous and owns no cancelable wait. A lifecycle-shaped
	// error here is a failed transition, not evidence of an ordinary wakeup.
	if cause != sessionobs.ProtocolOperationCauseNone && ProtocolOperationLifecycleCause(cause) {
		cause = sessionobs.ProtocolOperationCauseProtocolFailure
	}
	call.recordProtocolTraceCause(cause)
}

func (call *Call) recordProtocolTraceCause(cause sessionobs.ProtocolOperationCause) {
	if call == nil || !call.traceEnabled {
		return
	}
	if cause == sessionobs.ProtocolOperationCauseNone {
		return
	}
	call.stateMu.Lock()
	call.traceCause = MergeProtocolOperationCause(call.traceCause, cause)
	call.stateMu.Unlock()
}

type OperationOutcome struct {
	Stage sessionobs.ProtocolOperationStage
	Cause sessionobs.ProtocolOperationCause
}

// An owner holds terminal observation until its receive and cleanup results are
// known. Closing the RPC sink still wakes waiters immediately, but cannot publish
// a partial outcome that would hide a later joined fault.
type TerminalOwner struct{ call *Call }

func (call *Call) claimProtocolTermination() *TerminalOwner {
	if call == nil || !call.traceEnabled {
		return nil
	}
	call.stateMu.Lock()
	defer call.stateMu.Unlock()
	return call.claimProtocolTerminationLocked()
}

func (call *Call) claimProtocolTerminationLocked() *TerminalOwner {
	if !call.traceEnabled {
		return nil
	}
	owner := &TerminalOwner{call: call}
	call.traceOwner = owner
	return owner
}

func (owner *TerminalOwner) finish(runtime *Runtime, outcome OperationOutcome) {
	if owner == nil || runtime == nil {
		return
	}
	if event, ok := owner.call.protocolOperationTerminationTrace(runtime.now(), owner, outcome); ok {
		runtime.traceProtocolOperation(event)
	}
}

func (call *Call) protocolOperationTrace(now time.Time) (sessionobs.ProtocolOperationObservation, bool) {
	return call.protocolOperationTerminationTrace(now, nil, OperationOutcome{})
}

func (call *Call) protocolOperationTerminationTrace(now time.Time, owner *TerminalOwner, outcome OperationOutcome) (sessionobs.ProtocolOperationObservation, bool) {
	if call == nil || !call.traceEnabled {
		return sessionobs.ProtocolOperationObservation{}, false
	}
	// continuationLane takes laneMu before stateMu, so trace snapshotting follows
	// the same order and cannot invert locks during concurrent shutdown.
	call.laneMu.Lock()
	lane := call.lane
	call.stateMu.Lock()
	if call.traceEmitted || call.traceOwner != owner {
		call.stateMu.Unlock()
		call.laneMu.Unlock()
		return sessionobs.ProtocolOperationObservation{}, false
	}
	call.traceEmitted = true
	stage, cause := sessionobs.ProtocolOperationReceiverEnded, call.traceCause
	switch {
	case owner != nil:
		stage, cause = outcome.Stage, MergeProtocolOperationCause(cause, outcome.Cause)
		// Supersession owns a canceled block wait, not missing RPC authority.
		// Peer owners can separately prove an ordinary operation-closed wakeup.
		if outcome.Cause == sessionobs.ProtocolOperationCauseSuperseded && cause == sessionobs.ProtocolOperationCauseOperationClosed {
			stage = sessionobs.ProtocolOperationReceiverFailed
		}
		if stage == sessionobs.ProtocolOperationReceiverEnded && ProtocolOperationLifecycleCause(cause) && outcome.Cause != sessionobs.ProtocolOperationCauseNone {
			cause = outcome.Cause
		}
		if (stage == sessionobs.ProtocolOperationReceiverCompleted && cause != sessionobs.ProtocolOperationCauseNone) ||
			(stage == sessionobs.ProtocolOperationReceiverEnded && !ProtocolOperationLifecycleCause(cause)) {
			stage = sessionobs.ProtocolOperationReceiverFailed
		}
		if stage == sessionobs.ProtocolOperationReceiverFailed && cause == sessionobs.ProtocolOperationCauseNone {
			cause = sessionobs.ProtocolOperationCauseProtocolFailure
		}
	case cause != sessionobs.ProtocolOperationCauseNone:
		stage = sessionobs.ProtocolOperationReceiverFailed
	case call.traceHasFinalResponse:
		stage = sessionobs.ProtocolOperationReceiverCompleted
	}
	event := sessionobs.ProtocolOperationObservation{
		Stage: stage, OperationID: call.id, RequestKind: call.requestKind,
		ResponseKind: call.traceResponseKind, HasResponse: call.traceHasResponse,
		Lane: lane, HasLane: lane.Valid(true), HasSend: call.traceHasSend,
		SendSettled: call.traceSendSettled, SendAdmitted: call.traceSendAdmitted,
		SendOutcome: call.traceSendOutcome, ResponseCount: call.traceResponseCount,
		DeadlineRemainingMillis: call.traceDeadlineMillis, HasDeadline: call.traceHasDeadline,
		OperationElapsedMillis: durationMillis(now.Sub(call.traceStarted)),
		UsableLanesAtSelection: call.traceUsableAtSelection,
		Cause:                  cause,
		RequestScheduling:      call.requests.Estimate(),
		BlockWait:              call.traceBlockWait,
	}
	call.stateMu.Unlock()
	call.laneMu.Unlock()
	return event, true
}

func (runtime *Runtime) observationContext(operationID protocolsession.OperationID, kind protocolsession.MessageKind) sessionobs.ProtocolObservationContext {
	return sessionobs.ProtocolObservationContext{Correlation: sessionobs.ProtocolObservationCorrelation{Role: runtime.role, ProtocolSessionID: runtime.sessionID, OperationID: operationID, RequestKind: kind}, ObservedAt: runtime.now()}
}

func (runtime *Runtime) traceProtocolOperation(event sessionobs.ProtocolOperationObservation) {
	if !runtime.protocolOperationTracingEnabled() || !retainProtocolOperationObservation(event) {
		return
	}
	event = sessionobs.NewProtocolOperationObservation(runtime.observationContext(event.OperationID, event.RequestKind), event)
	if runtime.lanes != nil {
		event.UsableLanesAtSettlement = runtime.lanes.usableCount()
	}
	runtime.protocolObservations.TryPublish(event)
}

func (runtime *Runtime) protocolOperationTracingEnabled() bool {
	return runtime != nil && !runtime.protocolObservations.IsZero()
}

func retainProtocolOperationObservation(event sessionobs.ProtocolOperationObservation) bool {
	if event.Stage == sessionobs.ProtocolOperationReceiverWaitingActiveCapacity || event.Stage == sessionobs.ProtocolOperationReceiverWaitingRetainedCapacity || event.Stage == sessionobs.ProtocolOperationReceiverAdmissionReady {
		return true
	}
	if event.Cause != sessionobs.ProtocolOperationCauseNone ||
		(event.HasSend && (!event.SendSettled || !event.SendAdmitted || event.SendOutcome != protocolsession.SendOutcomeTransportConfirmed)) ||
		(event.HasResponse && event.ResponseKind == protocolsession.MessageOperationError) {
		return true
	}
	return event.RequestKind != protocolsession.MessageRequestBlocks
}

func remainingDeadlineMillis(ctx context.Context, now time.Time) (uint64, bool) {
	if ctx == nil {
		return 0, false
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0, false
	}
	remaining := deadline.Sub(now)
	if remaining <= 0 {
		return 0, true
	}
	// Round up so a newly-created 30 second budget remains recognizable as
	// 30000ms instead of becoming 29999ms due to sub-millisecond setup work.
	millis := remaining / time.Millisecond
	if remaining%time.Millisecond != 0 {
		millis++
	}
	return uint64(millis), true
}

func durationMillis(value time.Duration) uint64 {
	if value <= 0 {
		return 0
	}
	millis := value / time.Millisecond
	return uint64(millis)
}

func (client *Requests) waitRequestCapacity(ctx context.Context, call *Call) error {
	var observer func(protocolsession.OperationCapacityWaitReason)
	waited := false
	if call.traceEnabled {
		observer = func(reason protocolsession.OperationCapacityWaitReason) {
			waited = true
			stage := sessionobs.ProtocolOperationReceiverWaitingRetainedCapacity
			if reason == protocolsession.OperationWaitingActiveCapacity {
				stage = sessionobs.ProtocolOperationReceiverWaitingActiveCapacity
			}
			client.runtime.traceProtocolOperation(sessionobs.ProtocolOperationObservation{
				Stage: stage, OperationID: call.id, RequestKind: call.requestKind,
				OperationElapsedMillis: durationMillis(client.runtime.now().Sub(call.traceStarted)),
			})
		}
	}
	err := client.runtime.operations.WaitForCapacity(ctx, observer)
	if waited && err == nil {
		client.runtime.traceProtocolOperation(sessionobs.ProtocolOperationObservation{
			Stage: sessionobs.ProtocolOperationReceiverAdmissionReady, OperationID: call.id, RequestKind: call.requestKind,
			OperationElapsedMillis: durationMillis(client.runtime.now().Sub(call.traceStarted)),
		})
	}
	return err
}
