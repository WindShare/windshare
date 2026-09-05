package sessionruntime

import (
	"context"
	"errors"
	"sync"

	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type SenderSessionTerminalTrigger string

const (
	SenderSessionTerminalTriggerGracefulStop   SenderSessionTerminalTrigger = "graceful_stop"
	SenderSessionTerminalTriggerForcedClose    SenderSessionTerminalTrigger = "forced_close"
	SenderSessionTerminalTriggerPeerTerminal   SenderSessionTerminalTrigger = "peer_terminal"
	SenderSessionTerminalTriggerPathsExhausted SenderSessionTerminalTrigger = "paths_exhausted"
	SenderSessionTerminalTriggerRuntimeFailed  SenderSessionTerminalTrigger = "runtime_failed"
)

type SenderSessionTerminalProvenance string

const (
	SenderSessionTerminalProvenanceNormalStop     SenderSessionTerminalProvenance = "normal_stop"
	SenderSessionTerminalProvenanceCallerStop     SenderSessionTerminalProvenance = "caller_stop"
	SenderSessionTerminalProvenanceRemoteClose    SenderSessionTerminalProvenance = "remote_close"
	SenderSessionTerminalProvenanceLaneRetirement SenderSessionTerminalProvenance = "lane_retirement"
	SenderSessionTerminalProvenanceLocalFault     SenderSessionTerminalProvenance = "local_fault"
)

// SenderSessionTerminated is the single causal root for a sender session. The
// closed pair records who owned the terminal decision without retaining errors,
// terminal text, or provider details.
type SenderSessionTerminated struct {
	ProtocolSessionID protocolsession.ProtocolSessionID
	Trigger           SenderSessionTerminalTrigger
	Provenance        SenderSessionTerminalProvenance
}

func (terminated SenderSessionTerminated) Valid() bool {
	if terminated.ProtocolSessionID.IsZero() {
		return false
	}
	return validSenderSessionTerminalPair(terminated.Trigger, terminated.Provenance)
}

func validSenderSessionTerminalPair(
	trigger SenderSessionTerminalTrigger,
	provenance SenderSessionTerminalProvenance,
) bool {
	switch trigger {
	case SenderSessionTerminalTriggerGracefulStop:
		return provenance == SenderSessionTerminalProvenanceNormalStop
	case SenderSessionTerminalTriggerForcedClose:
		return provenance == SenderSessionTerminalProvenanceCallerStop
	case SenderSessionTerminalTriggerPeerTerminal:
		return provenance == SenderSessionTerminalProvenanceRemoteClose
	case SenderSessionTerminalTriggerPathsExhausted:
		return provenance == SenderSessionTerminalProvenanceLaneRetirement
	case SenderSessionTerminalTriggerRuntimeFailed:
		return provenance == SenderSessionTerminalProvenanceLocalFault
	default:
		return false
	}
}

type SenderSessionTerminalObserver interface {
	ObserveSenderSessionTerminated(SenderSessionTerminated)
}

type SenderSessionTerminalObserverFunc func(SenderSessionTerminated)

func (function SenderSessionTerminalObserverFunc) ObserveSenderSessionTerminated(
	terminated SenderSessionTerminated,
) {
	if function != nil {
		function(terminated)
	}
}

type runtimeTerminationCause uint8

const (
	runtimeTerminationGracefulStop runtimeTerminationCause = iota + 1
	runtimeTerminationForcedClose
	runtimeTerminationPeerTerminal
	runtimeTerminationPathsExhausted
	runtimeTerminationFailed
)

func (cause runtimeTerminationCause) senderTerminalPair() (
	SenderSessionTerminalTrigger,
	SenderSessionTerminalProvenance,
	bool,
) {
	switch cause {
	case runtimeTerminationGracefulStop:
		return SenderSessionTerminalTriggerGracefulStop, SenderSessionTerminalProvenanceNormalStop, true
	case runtimeTerminationForcedClose:
		return SenderSessionTerminalTriggerForcedClose, SenderSessionTerminalProvenanceCallerStop, true
	case runtimeTerminationPeerTerminal:
		return SenderSessionTerminalTriggerPeerTerminal, SenderSessionTerminalProvenanceRemoteClose, true
	case runtimeTerminationPathsExhausted:
		return SenderSessionTerminalTriggerPathsExhausted, SenderSessionTerminalProvenanceLaneRetirement, true
	case runtimeTerminationFailed:
		return SenderSessionTerminalTriggerRuntimeFailed, SenderSessionTerminalProvenanceLocalFault, true
	default:
		return "", "", false
	}
}

type runtimeTerminationClaim struct {
	cause runtimeTerminationCause
	won   bool
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

func (runtime *runtimeCore) claimTermination(cause runtimeTerminationCause) runtimeTerminationClaim {
	if runtime == nil {
		return runtimeTerminationClaim{}
	}
	return runtime.termination.claim(cause)
}

func (runtime *runtimeCore) publishTermination(claim runtimeTerminationClaim) {
	if runtime == nil || !claim.won || runtime.sessionTerminalObserver == nil {
		return
	}
	trigger, provenance, valid := claim.cause.senderTerminalPair()
	if !valid {
		return
	}
	terminated := SenderSessionTerminated{
		ProtocolSessionID: runtime.sessionID,
		Trigger:           trigger,
		Provenance:        provenance,
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

func (runtime *runtimeCore) cancelContext() {
	if runtime != nil && runtime.cancelLifecycle != nil {
		runtime.cancelLifecycle()
	}
}

func (runtime *runtimeCore) completeTermination(
	claim runtimeTerminationClaim,
	cancel bool,
) {
	if cancel {
		runtime.cancelContext()
	}
	runtime.publishTermination(claim)
}

func (runtime *runtimeCore) terminate(cause runtimeTerminationCause) {
	if runtime == nil {
		return
	}
	claim := runtime.claimTermination(cause)
	runtime.completeTermination(claim, true)
}

func (runtime *runtimeCore) terminateRuntimeFailed(err error) {
	if runtime == nil {
		return
	}
	if err != nil {
		runtime.recordError(err)
	}
	runtime.terminate(runtimeTerminationFailed)
}

type senderTerminalObservers struct {
	send    SenderTerminalSendObserver
	session SenderSessionTerminalObserver
}

func newSenderTerminalObservers(
	send SenderTerminalSendObserver,
	session SenderSessionTerminalObserver,
) *senderTerminalObservers {
	if send == nil && session == nil {
		return nil
	}
	return &senderTerminalObservers{send: send, session: session}
}

func (observers *senderTerminalObservers) sendObserver() SenderTerminalSendObserver {
	if observers == nil {
		return nil
	}
	return observers.send
}

func (observers *senderTerminalObservers) sessionObserver() SenderSessionTerminalObserver {
	if observers == nil {
		return nil
	}
	return observers.session
}

func (outbound senderOutbound) sendTerminalAll(
	deliveryContext context.Context,
	callerContext context.Context,
	body []byte,
) error {
	return outbound.sendTerminalRecipients(
		deliveryContext,
		callerContext,
		body,
		outbound.runtime.lanes.snapshot(),
	)
}

func (outbound senderOutbound) sendTerminalRecipients(
	deliveryContext context.Context,
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
		lane    LaneIdentity
		receipt protocolsession.SendReceipt
	}
	type terminalCompletion struct {
		lane       LaneIdentity
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
	completions := make([]terminalCompletion, 0, len(receipts))
	for _, pending := range receipts {
		completions = append(completions, terminalCompletion{
			lane:       pending.lane,
			completion: pending.receipt.Await(deliveryContext),
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
		if completion.Err == nil && completion.Outcome == protocolsession.SendOutcomeDelivered {
			// Once any attached lane delivers terminal, the peer's monotonic
			// session stop may close its siblings before their receipts settle.
			// Every lane was admitted before waiting, so that close is success,
			// not evidence that terminal fanout was skipped.
			delivered = true
		}
		if completion.Err != nil &&
			(completion.Outcome == protocolsession.SendOutcomeDelivered ||
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
