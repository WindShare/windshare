package sessioncore

import (
	"context"
	"errors"
	"slices"

	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
)

// TerminalSendObservation carries only physical evidence, never operation or
// replay authority. Role observers cannot participate in send settlement.
type TerminalSendObservation struct {
	ProtocolSessionID    protocolsession.ProtocolSessionID
	Lane                 session.LaneIdentity
	Settled              bool
	Outcome              sessionobs.SenderTerminalSendOutcome
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
		return callerContext.Err()
	}
	message, err := protocolsession.NewMessage(protocolsession.MessageSessionTerminal, nil, body)
	if err != nil {
		return err
	}
	type pendingTerminal struct {
		lane session.LaneIdentity
		send *terminalSend
	}
	type settledTerminal struct {
		lane   session.LaneIdentity
		result terminalSendResult
	}
	pending := make([]pendingTerminal, 0, len(lanes))
	var combined error
	var hardAdmissionError error
	onlyStoppedBeforeAdmission := true
	for _, lane := range lanes {
		send, enqueueErr := lane.writer.EnqueueTerminal(message, outbound.runtime.lanes.acceptTerminal)
		if enqueueErr == nil {
			pending = append(pending, pendingTerminal{lane: lane.identity, send: send})
		}
		if enqueueErr != nil && !errorTreeContainsOnly(enqueueErr, protocolsession.ErrWriterStopped, protocolsession.ErrNotSent) {
			onlyStoppedBeforeAdmission = false
			hardAdmissionError = errors.Join(hardAdmissionError, enqueueErr)
		}
		combined = errors.Join(combined, enqueueErr)
	}
	if len(pending) == 0 {
		if err := callerContext.Err(); err != nil {
			return errors.Join(combined, err)
		}
		if onlyStoppedBeforeAdmission && !outbound.runtime.lanes.hasUsable() {
			return nil
		}
		return combined
	}
	settled := make([]settledTerminal, 0, len(pending))
	for _, item := range pending {
		settled = append(settled, settledTerminal{
			lane:   item.lane,
			result: item.send.Await(callerContext),
		})
	}
	delivered := false
	noUsableReplacement := !outbound.runtime.lanes.hasUsable()
	for _, item := range settled {
		result := item.result
		outcome := terminalOutcome(result)
		naturallyRetired := terminalResultNaturallyRetired(result, outcome, noUsableReplacement)
		outbound.observeTerminal(TerminalSendObservation{
			ProtocolSessionID:    outbound.runtime.sessionID,
			Lane:                 item.lane,
			Settled:              result.settled,
			Outcome:              outcome,
			TransportDisposition: result.disposition,
			NaturallyRetired:     naturallyRetired,
		})
		if naturallyRetired {
			continue
		}
		if result.err == nil && outcome == sessionobs.SenderTerminalSendOutcomeDelivered {
			delivered = true
		}
		if result.err != nil &&
			(outcome == sessionobs.SenderTerminalSendOutcomeDelivered ||
				(outcome == sessionobs.SenderTerminalSendOutcomeDropped &&
					!isNaturalTerminalDropError(result.err))) {
			hardAdmissionError = errors.Join(hardAdmissionError, result.err)
		}
		combined = errors.Join(combined, result.err)
	}
	if delivered {
		return errors.Join(callerContext.Err(), hardAdmissionError)
	}
	return errors.Join(combined, callerContext.Err())
}

func terminalOutcome(result terminalSendResult) sessionobs.SenderTerminalSendOutcome {
	if !result.settled {
		return sessionobs.SenderTerminalSendOutcomeUnknown
	}
	if result.err == nil {
		return sessionobs.SenderTerminalSendOutcomeDelivered
	}
	if protocolsession.IsNotSent(result.err) || result.disposition != framechannel.SendAccepted {
		return sessionobs.SenderTerminalSendOutcomeDropped
	}
	return sessionobs.SenderTerminalSendOutcomeUnknown
}

func terminalResultNaturallyRetired(
	result terminalSendResult,
	outcome sessionobs.SenderTerminalSendOutcome,
	noUsableReplacement bool,
) bool {
	if result.disposition == framechannel.SendRetired {
		return true
	}
	if !noUsableReplacement || !result.settled ||
		outcome != sessionobs.SenderTerminalSendOutcomeDropped || result.err == nil {
		return false
	}
	switch result.disposition {
	case 0:
		return isNaturalTerminalDropError(result.err)
	case framechannel.SendRejected:
		return errorTreeContainsOnly(result.err, context.Canceled, protocolsession.ErrNotSent)
	default:
		return false
	}
}

func isNaturalTerminalDropError(err error) bool {
	return errorTreeContainsOnly(
		err,
		protocolsession.ErrWriterStopped,
		protocolsession.ErrNotSent,
		context.Canceled,
		context.DeadlineExceeded,
		framechannel.ErrSendRetired,
	)
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
	if matcher, ok := err.(interface{ Is(error) bool }); ok && slices.ContainsFunc(allowed, matcher.Is) {
		// Unwrap through ErrNotSent wrappers so the underlying cause is also checked.
		if wrapped := errors.Unwrap(err); wrapped != nil && matcher.Is(protocolsession.ErrNotSent) {
			return errorTreeContainsOnly(wrapped, allowed...)
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
