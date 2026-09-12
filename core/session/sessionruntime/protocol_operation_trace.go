package sessionruntime

import (
	"context"
	"errors"
	"time"

	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/core/session/contentflow"
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
	ProtocolOperationReceiverWaitingActiveCapacity
	ProtocolOperationReceiverWaitingRetainedCapacity
	ProtocolOperationReceiverAdmissionReady
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

// ProtocolOperationObservation summarizes one RPC boundary. Content decisions carry
// only opaque coordinator/lease join keys; request bodies, file identities,
// catalog paths, and raw errors remain intentionally absent.
type ProtocolOperationObservation struct {
	observedAt              time.Time
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

type ProtocolObservation interface {
	Correlation() ProtocolObservationCorrelation
	ObservedAt() time.Time
	protocolObservation()
}
type ProtocolObservationCorrelation struct {
	Role              protocolsession.Role
	ProtocolSessionID protocolsession.ProtocolSessionID
	OperationID       protocolsession.OperationID
	// RequestKind may be zero only on ResponseSendNotStarted when setup had no
	// operation route from which to obtain authenticated request context.
	RequestKind protocolsession.MessageKind
}
type ProtocolObservationContext struct {
	Correlation ProtocolObservationCorrelation
	ObservedAt  time.Time
}
type observationContext struct{ context ProtocolObservationContext }

func (fact observationContext) Correlation() ProtocolObservationCorrelation {
	return fact.context.Correlation
}
func (fact observationContext) ObservedAt() time.Time { return fact.context.ObservedAt }
func (observationContext) protocolObservation()       {}

type ProtocolObservationTracer interface{ TraceProtocolObservation(ProtocolObservation) }
type ProtocolObservationTraceFunc func(ProtocolObservation)

func (fn ProtocolObservationTraceFunc) TraceProtocolObservation(fact ProtocolObservation) {
	if fn != nil {
		fn(fact)
	}
}

type responseSendObservation struct {
	observationContext
	sequence uint64
	kind     protocolsession.MessageKind
	content  ProtocolErrorContent
	result   protocolsession.ResponseSendResult
}

func (fact responseSendObservation) ResponseSequence() uint64                   { return fact.sequence }
func (fact responseSendObservation) ResponseKind() protocolsession.MessageKind  { return fact.kind }
func (fact responseSendObservation) Content() ProtocolErrorContent              { return fact.content }
func (fact responseSendObservation) Result() protocolsession.ResponseSendResult { return fact.result }

type ResponseSendNotStarted struct{ responseSendObservation }
type ResponseSendReturned struct{ responseSendObservation }

func NewResponseSendNotStarted(ctx ProtocolObservationContext, sequence uint64, kind protocolsession.MessageKind, content ProtocolErrorContent, result protocolsession.ResponseSendResult) ResponseSendNotStarted {
	return ResponseSendNotStarted{responseSendObservation{observationContext{ctx}, sequence, kind, content, result}}
}
func NewResponseSendReturned(ctx ProtocolObservationContext, sequence uint64, kind protocolsession.MessageKind, content ProtocolErrorContent, result protocolsession.ResponseSendResult) ResponseSendReturned {
	return ResponseSendReturned{responseSendObservation{observationContext{ctx}, sequence, kind, content, result}}
}

type SendAttemptSettled struct {
	observationContext
	kind    protocolsession.MessageKind
	attempt protocolsession.SendAttemptSnapshot
}

func NewSendAttemptSettled(ctx ProtocolObservationContext, kind protocolsession.MessageKind, attempt protocolsession.SendAttemptSnapshot) SendAttemptSettled {
	return SendAttemptSettled{observationContext{ctx}, kind, attempt}
}
func (fact SendAttemptSettled) ResponseKind() protocolsession.MessageKind    { return fact.kind }
func (fact SendAttemptSettled) Attempt() protocolsession.SendAttemptSnapshot { return fact.attempt }

type ReceivedProtocolError struct {
	observationContext
	content ProtocolErrorContent
	lane    LaneIdentity
}

func NewReceivedProtocolError(ctx ProtocolObservationContext, content ProtocolErrorContent, lane LaneIdentity) ReceivedProtocolError {
	return ReceivedProtocolError{observationContext{ctx}, content, lane}
}
func (fact ReceivedProtocolError) Content() ProtocolErrorContent { return fact.content }
func (fact ReceivedProtocolError) Lane() LaneIdentity            { return fact.lane }

type SenderContentDecision struct {
	observationContext
	decision contentflow.SenderDecisionTrace
	lane     LaneIdentity
	hasLane  bool
}

func NewSenderContentDecision(ctx ProtocolObservationContext, decision contentflow.SenderDecisionTrace, lane LaneIdentity, hasLane bool) SenderContentDecision {
	return SenderContentDecision{observationContext{ctx}, decision, lane, hasLane}
}
func (fact SenderContentDecision) Decision() contentflow.SenderDecisionTrace { return fact.decision }
func (fact SenderContentDecision) Lane() (LaneIdentity, bool)                { return fact.lane, fact.hasLane }

func NewProtocolOperationObservation(ctx ProtocolObservationContext, fact ProtocolOperationObservation) ProtocolOperationObservation {
	fact.Role = ctx.Correlation.Role
	fact.ProtocolSessionID = ctx.Correlation.ProtocolSessionID
	fact.OperationID = ctx.Correlation.OperationID
	fact.RequestKind = ctx.Correlation.RequestKind
	fact.observedAt = ctx.ObservedAt
	return fact
}
func (fact ProtocolOperationObservation) Correlation() ProtocolObservationCorrelation {
	return ProtocolObservationCorrelation{fact.Role, fact.ProtocolSessionID, fact.OperationID, fact.RequestKind}
}
func (fact ProtocolOperationObservation) ObservedAt() time.Time { return fact.observedAt }
func (ProtocolOperationObservation) protocolObservation()       {}

func (client *rpcClient) newCall(
	ctx context.Context,
	id protocolsession.OperationID,
	kind protocolsession.MessageKind,
) *operationCall {
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

func (call *operationCall) protocolOperationTrace(now time.Time) (ProtocolOperationObservation, bool) {
	if call == nil || !call.traceEnabled {
		return ProtocolOperationObservation{}, false
	}
	// continuationLane takes laneMu before stateMu, so trace snapshotting follows
	// the same order and cannot invert locks during concurrent shutdown.
	call.laneMu.Lock()
	lane := call.lane
	call.stateMu.Lock()
	if call.traceEmitted {
		call.stateMu.Unlock()
		call.laneMu.Unlock()
		return ProtocolOperationObservation{}, false
	}
	call.traceEmitted = true
	stage := ProtocolOperationReceiverEnded
	if call.traceCause != ProtocolOperationCauseNone {
		stage = ProtocolOperationReceiverFailed
	} else if call.traceHasFinalResponse {
		stage = ProtocolOperationReceiverCompleted
	}
	event := ProtocolOperationObservation{
		Stage: stage, OperationID: call.id, RequestKind: call.requestKind,
		ResponseKind: call.traceResponseKind, HasResponse: call.traceHasResponse,
		Lane: lane, HasLane: lane.valid(true), HasSend: call.traceHasSend,
		SendSettled: call.traceSendSettled, SendAdmitted: call.traceSendAdmitted,
		SendOutcome: call.traceSendOutcome, ResponseCount: call.traceResponseCount,
		DeadlineRemainingMillis: call.traceDeadlineMillis, HasDeadline: call.traceHasDeadline,
		OperationElapsedMillis: durationMillis(now.Sub(call.traceStarted)),
		UsableLanesAtSelection: call.traceUsableAtSelection,
		Cause:                  call.traceCause,
	}
	call.stateMu.Unlock()
	call.laneMu.Unlock()
	return event, true
}

func (runtime *runtimeCore) observationContext(operationID protocolsession.OperationID, kind protocolsession.MessageKind) ProtocolObservationContext {
	return ProtocolObservationContext{Correlation: ProtocolObservationCorrelation{runtime.role, runtime.sessionID, operationID, kind}, ObservedAt: runtime.now()}
}
func (runtime *runtimeCore) traceProtocolOperation(event ProtocolOperationObservation) {
	if !runtime.protocolOperationTracingEnabled() || !retainProtocolOperationObservation(event) {
		return
	}
	event = NewProtocolOperationObservation(runtime.observationContext(event.OperationID, event.RequestKind), event)
	if runtime.lanes != nil {
		event.UsableLanesAtSettlement = runtime.lanes.usableCount()
	}
	runtime.protocolObservations.TryPublish(event)
}
func (runtime *runtimeCore) protocolOperationTracingEnabled() bool {
	return runtime != nil && !runtime.protocolObservations.IsZero()
}
func retainProtocolOperationObservation(event ProtocolOperationObservation) bool {
	if event.Stage == ProtocolOperationReceiverWaitingActiveCapacity || event.Stage == ProtocolOperationReceiverWaitingRetainedCapacity || event.Stage == ProtocolOperationReceiverAdmissionReady {
		return true
	}
	if event.Cause != ProtocolOperationCauseNone ||
		(event.HasSend && (!event.SendSettled || !event.SendAdmitted || event.SendOutcome != protocolsession.SendOutcomeTransportConfirmed)) ||
		(event.HasResponse && event.ResponseKind == protocolsession.MessageOperationError) {
		return true
	}
	return event.RequestKind != protocolsession.MessageRequestBlocks
}
func (runtime *runtimeCore) traceResponseResult(operationID protocolsession.OperationID, requestKind, responseKind protocolsession.MessageKind, sequence uint64, content ProtocolErrorContent, result protocolsession.ResponseSendResult) {
	if !runtime.protocolOperationTracingEnabled() {
		return
	}
	ctx := runtime.observationContext(operationID, requestKind)
	if !result.Started() && !result.IsZero() {
		runtime.protocolObservations.TryPublish(NewResponseSendNotStarted(ctx, sequence, responseKind, content, result))
		return
	}
	if result.AttemptCount() > 1 || result.Evidence() != protocolsession.ResponseSendEvidenceTransportConfirmed ||
		result.Cleanup() == protocolsession.SendCleanupFailed || result.End() != protocolsession.ResponseSendEndTransportConfirmed ||
		!content.IsZero() || (requestKind != protocolsession.MessageRequestBlocks && senderResponseFinal(responseKind)) {
		runtime.protocolObservations.TryPublish(NewResponseSendReturned(ctx, sequence, responseKind, content, result))
	}
}

const receiptObservationCapacity observationstream.Capacity = 64

func (runtime *runtimeCore) startReceiptObservations() {
	if !runtime.protocolOperationTracingEnabled() {
		return
	}
	producer, consumer, _ := observationstream.New[protocolsession.SendAttemptSettlement](receiptObservationCapacity)
	runtime.receiptObservations = producer
	runtime.receiptObservationsDone = make(chan struct{})
	output := runtime.protocolObservations
	done := runtime.receiptObservationsDone
	go func() {
		defer close(done)
		for settled := range consumer {
			correlation := settled.Correlation()
			ctx := ProtocolObservationContext{Correlation: ProtocolObservationCorrelation{
				Role: protocolsession.RoleSender, ProtocolSessionID: correlation.ProtocolSessionID,
				OperationID: correlation.OperationID, RequestKind: correlation.RequestKind}, ObservedAt: settled.ObservedAt()}
			output.TryPublish(NewSendAttemptSettled(ctx, correlation.ResponseKind, settled.Attempt()))
		}
	}()
}
func (runtime *runtimeCore) finishReceiptObservations() {
	if runtime.receiptObservationsDone == nil {
		return
	}
	completion := runtime.receiptObservations.Complete()
	<-runtime.receiptObservationsDone
	runtime.protocolObservations.RecordDropped(completion.CapacityDropped)
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

func (client *rpcClient) waitRequestCapacity(ctx context.Context, call *operationCall) error {
	var observer func(protocolsession.OperationCapacityWaitReason)
	waited := false
	if call.traceEnabled {
		observer = func(reason protocolsession.OperationCapacityWaitReason) {
			waited = true
			stage := ProtocolOperationReceiverWaitingRetainedCapacity
			if reason == protocolsession.OperationWaitingActiveCapacity {
				stage = ProtocolOperationReceiverWaitingActiveCapacity
			}
			client.runtime.traceProtocolOperation(ProtocolOperationObservation{
				Stage: stage, OperationID: call.id, RequestKind: call.requestKind,
				OperationElapsedMillis: durationMillis(client.runtime.now().Sub(call.traceStarted)),
			})
		}
	}
	err := client.runtime.operations.WaitForCapacity(ctx, observer)
	if waited && err == nil {
		client.runtime.traceProtocolOperation(ProtocolOperationObservation{
			Stage: ProtocolOperationReceiverAdmissionReady, OperationID: call.id, RequestKind: call.requestKind,
			OperationElapsedMillis: durationMillis(client.runtime.now().Sub(call.traceStarted)),
		})
	}
	return err
}
