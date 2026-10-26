package sessioncore

import (
	"context"
	"errors"
	"github.com/windshare/windshare/core/diagnosticerror"
	"github.com/windshare/windshare/core/framechannel"
	sessioncontract "github.com/windshare/windshare/core/session"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
	"sync"
)

type runtimeTerminationCause uint8

const (
	RuntimeTerminationGracefulStop runtimeTerminationCause = iota + 1
	RuntimeTerminationForcedClose
	runtimeTerminationPeerTerminal
	RuntimeTerminationPathsExhausted
	runtimeTerminationFailed
)

func (cause runtimeTerminationCause) senderTerminalPair() (
	sessionobs.SenderSessionTerminalTrigger,
	sessionobs.SenderSessionTerminalProvenance,
	bool,
) {
	switch cause {
	case RuntimeTerminationGracefulStop:
		return sessionobs.SenderSessionTerminalTriggerGracefulStop, sessionobs.SenderSessionTerminalProvenanceNormalStop, true
	case RuntimeTerminationForcedClose:
		return sessionobs.SenderSessionTerminalTriggerForcedClose, sessionobs.SenderSessionTerminalProvenanceCallerStop, true
	case runtimeTerminationPeerTerminal:
		return sessionobs.SenderSessionTerminalTriggerPeerTerminal, sessionobs.SenderSessionTerminalProvenanceRemoteClose, true
	case RuntimeTerminationPathsExhausted:
		return sessionobs.SenderSessionTerminalTriggerPathsExhausted, sessionobs.SenderSessionTerminalProvenanceLaneRetirement, true
	case runtimeTerminationFailed:
		return sessionobs.SenderSessionTerminalTriggerRuntimeFailed, sessionobs.SenderSessionTerminalProvenanceLocalFault, true
	default:
		return "", "", false
	}
}

type runtimeTerminationClaim struct {
	cause   runtimeTerminationCause
	won     bool
	failure diagnosticerror.Snapshot
}

type runtimeTerminationArbiter struct {
	mu      sync.Mutex
	claimed bool
	cause   runtimeTerminationCause
}

func (arbiter *runtimeTerminationArbiter) claim(cause runtimeTerminationCause) runtimeTerminationClaim {
	if arbiter == nil {
		return runtimeTerminationClaim{}
	}
	if _, _, valid := cause.senderTerminalPair(); !valid {
		return runtimeTerminationClaim{}
	}
	arbiter.mu.Lock()
	defer arbiter.mu.Unlock()
	if arbiter.claimed {
		return runtimeTerminationClaim{}
	}
	arbiter.claimed = true
	arbiter.cause = cause
	return runtimeTerminationClaim{cause: cause, won: true}
}

func (runtime *Runtime) claimTermination(cause runtimeTerminationCause) runtimeTerminationClaim {
	if runtime == nil {
		return runtimeTerminationClaim{}
	}
	return runtime.termination.claim(cause)
}

func (runtime *Runtime) publishTermination(claim runtimeTerminationClaim) {
	if runtime == nil || !claim.won || runtime.sessionTerminalObserver == nil {
		return
	}
	trigger, provenance, valid := claim.cause.senderTerminalPair()
	if !valid {
		return
	}
	terminated := sessionobs.SenderSessionTerminated{
		ProtocolSessionID: runtime.sessionID,
		Trigger:           trigger,
		Provenance:        provenance,
		Failure:           claim.failure,
	}
	if !terminated.Valid() {
		return
	}
	// Diagnostics must remain outside lifecycle authority. A faulty observer can
	// lose its evidence, but it cannot alter cancellation, settlement, or cleanup.
	func() {
		defer func() { _ = recover() }()
		runtime.sessionTerminalObserver.ObserveSenderSessionTerminated(terminated)
	}()
}

func (runtime *Runtime) cancelContext() {
	if runtime != nil && runtime.cancelLifecycle != nil {
		runtime.cancelLifecycle()
	}
}

func (runtime *Runtime) completeTermination(
	claim runtimeTerminationClaim,
	cancel bool,
) {
	if cancel {
		runtime.cancelContext()
	}
	runtime.publishTermination(claim)
}

func (runtime *Runtime) terminate(cause runtimeTerminationCause) {
	if runtime == nil {
		return
	}
	if cause == runtimeTerminationFailed {
		runtime.terminateWithFailure(cause, runtime.Err(), runtimeFailureSourceRuntime)
		return
	}
	claim := runtime.claimTermination(cause)
	runtime.completeTermination(claim, true)
}

func (runtime *Runtime) terminateRuntimeFailed(err error) {
	runtime.terminateWithFailure(runtimeTerminationFailed, err, runtimeFailureSourceRuntime)
}

func (runtime *Runtime) terminateWithFailure(
	cause runtimeTerminationCause, err error, source FailureSource,
) {
	if runtime == nil {
		return
	}
	claim := runtime.claimTermination(cause)
	if err != nil {
		runtime.recordError(err)
	}
	// Cancellation cannot depend on error inspection or a diagnostic consumer.
	runtime.cancelContext()
	if claim.won && runtime.sessionTerminalObserver != nil {
		claim.failure = diagnosticerror.Capture(err, string(source))
	}
	runtime.publishTermination(claim)
}

func (outbound Responses) sendTerminalAll(
	callerContext context.Context,
	body []byte,
) error {
	return outbound.sendTerminalRecipients(
		callerContext,
		body,
		outbound.runtime.lanes.snapshot(),
	)
}

func (outbound Responses) sendTerminalRecipients(
	callerContext context.Context,
	body []byte,
	lanes []selectedLane,
) error {
	if len(lanes) == 0 {
		// Last-lane detach removes terminal recipients before it publishes core
		// cancellation. No writer receipt was admitted in this state, so there is
		// no terminal delivery lifecycle to fail. Only the caller's cancellation
		// remains an operation failure; lifecycle cancellation merely reports the
		// same naturally ended transport that produced the empty snapshot.
		return callerContext.Err()
	}
	type terminalReceipt struct {
		lane    sessioncontract.LaneIdentity
		receipt protocolsession.SendReceipt
	}
	type terminalCompletion struct {
		lane       sessioncontract.LaneIdentity
		completion protocolsession.SendCompletion
	}
	receipts := make([]terminalReceipt, 0, len(lanes))
	var combined error
	var hardAdmissionError error
	onlyStoppedBeforeAdmission := true
	for _, lane := range lanes {
		prepared, err := protocolsession.PrepareSenderControl(
			outbound.privateKey,
			outbound.runtime.senderControlBase(lane.identity),
			protocolsession.MessageSessionTerminal,
			nil,
			body,
		)
		if err == nil {
			var receipt protocolsession.SendReceipt
			receipt, err = lane.writer.TrySenderControl(prepared)
			if err == nil {
				receipts = append(receipts, terminalReceipt{lane: lane.identity, receipt: receipt})
			}
		}
		if err != nil && !errorTreeContainsOnly(err, protocolsession.ErrWriterStopped) {
			onlyStoppedBeforeAdmission = false
			hardAdmissionError = errors.Join(hardAdmissionError, err)
		}
		combined = errors.Join(combined, err)
	}
	if len(receipts) == 0 {
		if err := callerContext.Err(); err != nil {
			return errors.Join(combined, err)
		}
		if onlyStoppedBeforeAdmission && !outbound.runtime.lanes.hasUsable() {
			// A snapshot can retain immutable writer references after natural lane
			// completion. If every writer rejected admission because it had already
			// stopped and no replacement is usable, no terminal lifecycle was born.
			return nil
		}
		return combined
	}
	// Lane shutdown cancels physical writers, but their admitted receipts still
	// own the final acceptance result. Only the caller may abandon this wait;
	// observing lifecycle cancellation as a send result loses late success.
	completions := make([]terminalCompletion, 0, len(receipts))
	for _, pending := range receipts {
		completions = append(completions, terminalCompletion{
			lane:       pending.lane,
			completion: pending.receipt.Await(callerContext),
		})
	}
	delivered := false
	noUsableReplacement := !outbound.runtime.lanes.hasUsable()
	for _, settled := range completions {
		completion := settled.completion
		naturallyRetired := terminalCompletionNaturallyRetired(completion, noUsableReplacement)
		observeSenderTerminalSend(
			outbound.observer,
			outbound.runtime.sessionID,
			settled.lane,
			completion,
			naturallyRetired,
		)
		if naturallyRetired {
			// An admitted receipt can settle after channel retirement or while its
			// owning writer publishes last-lane completion. Neither path reached the
			// wire, so an absent replacement makes the lifecycle naturally complete.
			continue
		}
		if completion.Err == nil && completion.Outcome == protocolsession.SendOutcomeTransportConfirmed {
			// Once any attached lane delivers terminal, the peer's monotonic
			// session stop may close its siblings before their receipts settle.
			// Every lane was admitted before waiting, so that close is success,
			// not evidence that terminal fanout was skipped.
			delivered = true
		}
		if completion.Err != nil &&
			(completion.Outcome == protocolsession.SendOutcomeTransportConfirmed ||
				(completion.Outcome == protocolsession.SendOutcomeDropped &&
					!errorTreeContainsOnly(
						completion.Err,
						protocolsession.ErrWriterStopped,
						context.Canceled,
						context.DeadlineExceeded,
					))) {
			hardAdmissionError = errors.Join(hardAdmissionError, completion.Err)
		}
		combined = errors.Join(combined, completion.Err)
	}
	if delivered {
		// Delivery on one lane makes sibling receipt failures harmless, but it
		// cannot erase caller cancellation or a local preparation/admission fault.
		return errors.Join(callerContext.Err(), hardAdmissionError)
	}
	return errors.Join(combined, callerContext.Err())
}

func terminalCompletionNaturallyRetired(
	completion protocolsession.SendCompletion,
	noUsableReplacement bool,
) bool {
	if completion.TransportDisposition == framechannel.SendRetired {
		return true
	}
	if !noUsableReplacement || !completion.Settled ||
		completion.Outcome != protocolsession.SendOutcomeDropped || completion.Err == nil {
		return false
	}
	switch completion.TransportDisposition {
	case 0:
		return errorTreeContainsOnly(
			completion.Err,
			protocolsession.ErrWriterStopped,
			context.Canceled,
			context.DeadlineExceeded,
		)
	case framechannel.SendRejected:
		// SessionWriter sends with its lane lifecycle context. Once the last
		// writer is unusable, an exact cancellation rejection is the claimed-send
		// side of lane retirement and still proves that transport acquired nothing.
		return errorTreeContainsOnly(completion.Err, context.Canceled)
	default:
		return false
	}
}

func errorTreeContainsOnly(err error, allowed ...error) bool {
	if err == nil {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if child != nil && !errorTreeContainsOnly(child, allowed...) {
				return false
			}
		}
		return true
	}
	if wrapped := errors.Unwrap(err); wrapped != nil {
		return errorTreeContainsOnly(wrapped, allowed...)
	}
	for _, candidate := range allowed {
		if errors.Is(err, candidate) {
			return true
		}
	}
	return false
}
