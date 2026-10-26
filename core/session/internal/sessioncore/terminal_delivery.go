package sessioncore

import (
	"context"
	"errors"
	"github.com/windshare/windshare/core/framechannel"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
)

// TerminalSendObservation carries only physical evidence, never operation or
// replay authority. Role observers cannot participate in send settlement.
type TerminalSendObservation struct {
	ProtocolSessionID    protocolsession.ProtocolSessionID
	Lane                 sessioncontract.LaneIdentity
	Settled              bool
	Outcome              protocolsession.SendOutcome
	TransportDisposition framechannel.SendDisposition
	NaturallyRetired     bool
}

func (outbound *Responses) observeTerminal(event TerminalSendObservation) {
	if outbound.terminalObserver == nil {
		return
	}
	func() {
		defer func() { _ = recover() }()
		outbound.terminalObserver(event)
	}()
}

func (outbound *Responses) sendTerminalAll(
	callerContext context.Context,
	body []byte,
) error {
	return outbound.sendTerminalRecipients(
		callerContext,
		body,
		outbound.runtime.lanes.snapshot(),
	)
}

func (outbound *Responses) sendTerminalRecipients(
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
		prepared, err := outbound.prepareControl(
			outbound.controlBinding(lane.identity),
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
		outbound.observeTerminal(TerminalSendObservation{
			ProtocolSessionID: outbound.runtime.sessionID,
			Lane:              settled.lane, Settled: completion.Settled, Outcome: completion.Outcome,
			TransportDisposition: completion.TransportDisposition, NaturallyRetired: naturallyRetired,
		})
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
