package sessionruntime

import (
	"context"
	"errors"
	"time"

	"github.com/windshare/windshare/core/session/protocolsession"
)

// ProtocolOperationStage identifies the side and terminal observation boundary
// of one authenticated operation without exposing its request body.
type ProtocolOperationStage uint8

const (
	ProtocolOperationReceiverCompleted ProtocolOperationStage = iota + 1
	ProtocolOperationReceiverFailed
	ProtocolOperationReceiverEnded
	ProtocolOperationSenderRequestReceived
	ProtocolOperationSenderResponseSettled
)

// ProtocolOperationCause is deliberately closed and text-free. Raw transport
// errors can contain endpoints or provider details that do not belong in a
// privacy-safe user trace.
type ProtocolOperationCause uint8

const (
	ProtocolOperationCauseNone ProtocolOperationCause = iota
	ProtocolOperationCauseCanceled
	ProtocolOperationCauseDeadline
	ProtocolOperationCauseRuntimeClosed
	ProtocolOperationCauseLaneUnavailable
	ProtocolOperationCauseWriterStopped
	ProtocolOperationCauseOperationClosed
	ProtocolOperationCauseProtocolFailure
)

// ProtocolOperationTrace summarizes one RPC boundary. The operation identity is
// random correlation authority; request bodies, file identities, lease identities,
// catalog paths, and raw errors are intentionally absent.
type ProtocolOperationTrace struct {
	Stage                   ProtocolOperationStage
	Role                    protocolsession.Role
	ProtocolSessionID       protocolsession.ProtocolSessionID
	OperationID             protocolsession.OperationID
	RequestKind             protocolsession.MessageKind
	ResponseKind            protocolsession.MessageKind
	HasResponse             bool
	Lane                    LaneIdentity
	HasLane                 bool
	HasSend                 bool
	SendSettled             bool
	SendAdmitted            bool
	SendOutcome             protocolsession.SendOutcome
	ResponseCount           uint64
	DeadlineRemainingMillis uint64
	HasDeadline             bool
	OperationElapsedMillis  uint64
	UsableLanesAtSelection  uint32
	UsableLanesAtSettlement uint32
	Cause                   ProtocolOperationCause
}

type ProtocolOperationTracer interface {
	TraceProtocolOperation(ProtocolOperationTrace)
}

type ProtocolOperationTraceFunc func(ProtocolOperationTrace)

func (function ProtocolOperationTraceFunc) TraceProtocolOperation(event ProtocolOperationTrace) {
	if function != nil {
		function(event)
	}
}

func newOperationCall(
	id protocolsession.OperationID,
	kind protocolsession.MessageKind,
	started time.Time,
	deadlineMillis uint64,
	hasDeadline bool,
	traceEnabled bool,
) *operationCall {
	return &operationCall{
		id: id, requestKind: kind, traceEnabled: traceEnabled, traceStarted: started,
		traceDeadlineMillis: deadlineMillis, traceHasDeadline: hasDeadline,
		messages: make(chan operationResponse, operationResponseFrames), done: make(chan struct{}),
	}
}

func (call *operationCall) setProtocolTraceLane(lane LaneIdentity, usable uint32) {
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

func (call *operationCall) recordProtocolTraceSend(completion protocolsession.SendCompletion) {
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

func (call *operationCall) recordProtocolTraceFailure(err error) {
	if call == nil || !call.traceEnabled {
		return
	}
	cause := protocolOperationCause(err)
	if cause == ProtocolOperationCauseNone {
		return
	}
	call.stateMu.Lock()
	if call.traceCause == ProtocolOperationCauseNone {
		call.traceCause = cause
	}
	call.stateMu.Unlock()
}

func (call *operationCall) protocolOperationTrace(now time.Time) (ProtocolOperationTrace, bool) {
	if call == nil || !call.traceEnabled {
		return ProtocolOperationTrace{}, false
	}
	// continuationLane takes laneMu before stateMu, so trace snapshotting follows
	// the same order and cannot invert locks during concurrent shutdown.
	call.laneMu.Lock()
	lane := call.lane
	call.stateMu.Lock()
	if call.traceEmitted {
		call.stateMu.Unlock()
		call.laneMu.Unlock()
		return ProtocolOperationTrace{}, false
	}
	call.traceEmitted = true
	stage := ProtocolOperationReceiverEnded
	if call.traceCause != ProtocolOperationCauseNone {
		stage = ProtocolOperationReceiverFailed
	} else if call.traceHasFinalResponse {
		stage = ProtocolOperationReceiverCompleted
	}
	event := ProtocolOperationTrace{
		Stage: stage, OperationID: call.id, RequestKind: call.requestKind,
		ResponseKind: call.traceResponseKind, HasResponse: call.traceHasResponse,
		Lane: lane, HasLane: lane.valid(true), HasSend: call.traceHasSend,
		SendSettled: call.traceSendSettled, SendAdmitted: call.traceSendAdmitted,
		SendOutcome: call.traceSendOutcome, ResponseCount: call.traceResponseCount,
		DeadlineRemainingMillis: call.traceDeadlineMillis, HasDeadline: call.traceHasDeadline,
		OperationElapsedMillis: durationMillis(now.Sub(call.traceStarted)),
		UsableLanesAtSelection: call.traceUsableAtSelection, Cause: call.traceCause,
	}
	call.stateMu.Unlock()
	call.laneMu.Unlock()
	return event, true
}

func (runtime *runtimeCore) traceProtocolOperation(event ProtocolOperationTrace) {
	if runtime == nil || runtime.protocolTracer == nil || !retainProtocolOperationTrace(event) {
		return
	}
	if event.ProtocolSessionID.IsZero() {
		event.ProtocolSessionID = runtime.sessionID
	}
	event.Role = runtime.role
	if runtime.lanes != nil {
		event.UsableLanesAtSettlement = runtime.lanes.usableCount()
	}
	// Diagnostics must never gain authority over a protocol session. In
	// particular, a UI observer panic cannot strand operation cleanup.
	defer func() { _ = recover() }()
	runtime.protocolTracer.TraceProtocolOperation(event)
}

func (runtime *runtimeCore) protocolOperationTracingEnabled() bool {
	return runtime != nil && runtime.protocolTracer != nil
}

func retainProtocolOperationTrace(event ProtocolOperationTrace) bool {
	// Block operations and streaming responses are the transfer hot path. Their
	// successful milestones add no failure evidence and can turn diagnostics into
	// a second data stream, so retain only exceptional outcomes at those boundaries.
	if event.Cause != ProtocolOperationCauseNone ||
		(event.HasSend && (!event.SendSettled || !event.SendAdmitted ||
			event.SendOutcome != protocolsession.SendOutcomeDelivered)) ||
		(event.HasResponse && event.ResponseKind == protocolsession.MessageOperationError) {
		return true
	}
	if event.RequestKind == protocolsession.MessageRequestBlocks {
		return false
	}
	if event.Stage == ProtocolOperationSenderResponseSettled {
		return senderResponseFinal(event.ResponseKind)
	}
	return true
}

func protocolOperationCause(err error) ProtocolOperationCause {
	switch {
	case err == nil:
		return ProtocolOperationCauseNone
	case errors.Is(err, context.DeadlineExceeded):
		return ProtocolOperationCauseDeadline
	case errors.Is(err, context.Canceled):
		return ProtocolOperationCauseCanceled
	case errors.Is(err, ErrRuntimeClosed):
		return ProtocolOperationCauseRuntimeClosed
	case errors.Is(err, ErrLaneUnavailable):
		return ProtocolOperationCauseLaneUnavailable
	case errors.Is(err, protocolsession.ErrWriterStopped):
		return ProtocolOperationCauseWriterStopped
	case errors.Is(err, ErrOperationMissing):
		return ProtocolOperationCauseOperationClosed
	default:
		return ProtocolOperationCauseProtocolFailure
	}
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
