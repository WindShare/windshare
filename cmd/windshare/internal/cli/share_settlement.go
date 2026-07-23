package cli

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

var (
	errShareServeJoinTimedOut      = errors.New("share session server did not stop after interruption")
	errShareTerminalDecisionFailed = errors.New("share terminal settlement failed")
)

type shareShutdownTrigger string

const (
	shareShutdownCallerInterrupted shareShutdownTrigger = "caller_interrupted"
	shareShutdownServeEnded        shareShutdownTrigger = "serve_ended"
)

type shareComponentOutcome string

const (
	shareComponentCompleted   shareComponentOutcome = "completed"
	shareComponentInterrupted shareComponentOutcome = "interrupted"
	shareComponentFailed      shareComponentOutcome = "failed"
)

type shareSettlementDecision string

const (
	shareSettlementClean  shareSettlementDecision = "clean"
	shareSettlementFailed shareSettlementDecision = "failed"
)

type shareServeStopCause interface {
	shareServeStopCause()
}

type shareTerminalSessionState struct {
	observations     int
	delivered        bool
	naturallyRetired bool
	failed           bool
	acceptedFailures int
}

type shareTerminalLedger struct {
	mu       sync.Mutex
	sessions map[protocolsession.ProtocolSessionID]shareTerminalSessionState
}

func newShareTerminalLedger() *shareTerminalLedger {
	return &shareTerminalLedger{
		sessions: make(map[protocolsession.ProtocolSessionID]shareTerminalSessionState),
	}
}

func (ledger *shareTerminalLedger) ObserveSenderTerminal(
	observation sessionruntime.SenderTerminalObservation,
) {
	if ledger == nil {
		return
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	state := ledger.sessions[observation.ProtocolSessionID]
	state.observations++
	switch observation.Decision {
	case sessionruntime.SenderTerminalDecisionDelivered:
		state.delivered = true
	case sessionruntime.SenderTerminalDecisionNaturalRetirement:
		state.naturallyRetired = true
	default:
		state.failed = true
		if observation.TransportDisposition == sessionruntime.SenderTerminalTransportAccepted {
			state.acceptedFailures++
		}
	}
	ledger.sessions[observation.ProtocolSessionID] = state
}

type shareTerminalSummary struct {
	observations             int
	sessions                 int
	deliveredSessions        int
	naturallyRetiredSessions int
	failedSessions           int
	acceptedFailedLanes      int
}

func (ledger *shareTerminalLedger) Snapshot() shareTerminalSummary {
	if ledger == nil {
		return shareTerminalSummary{}
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	result := shareTerminalSummary{sessions: len(ledger.sessions)}
	for _, state := range ledger.sessions {
		result.observations += state.observations
		result.acceptedFailedLanes += state.acceptedFailures
		switch {
		case state.delivered:
			// One delivered lane completes the peer's monotonic terminal transition;
			// sibling lane failures do not make that session fail a second time.
			result.deliveredSessions++
		case state.failed:
			result.failedSessions++
		case state.naturallyRetired:
			result.naturallyRetiredSessions++
		default:
			// An observer entry without a recognized terminal decision cannot prove
			// that transport ownership ended safely.
			result.failedSessions++
		}
	}
	return result
}

type shareComponentSettlement struct {
	outcome shareComponentOutcome
	failure error
}

type shareLifecycleSettlement struct {
	trigger   shareShutdownTrigger
	serve     shareComponentSettlement
	stop      shareComponentSettlement
	terminals shareTerminalSummary
	decision  shareSettlementDecision
}

func settleShareLifecycle(
	trigger shareShutdownTrigger,
	interruption error,
	serveErr error,
	stopErr error,
	terminals shareTerminalSummary,
) shareLifecycleSettlement {
	settlement := shareLifecycleSettlement{
		trigger:   trigger,
		serve:     settleShareServe(trigger, interruption, serveErr),
		terminals: terminals,
	}
	settlement.stop = settleShareStop(stopErr, terminals)
	settlement.decision = shareSettlementClean
	if settlement.serve.failure != nil || settlement.stop.failure != nil {
		settlement.decision = shareSettlementFailed
	}
	return settlement
}

func settleShareServe(
	trigger shareShutdownTrigger,
	interruption error,
	err error,
) shareComponentSettlement {
	switch {
	case err == nil:
		return shareComponentSettlement{outcome: shareComponentCompleted}
	case trigger == shareShutdownCallerInterrupted && errorTreeContainsOnly(err, func(leaf error) bool {
		if exactShareInterruption(leaf, interruption) {
			return true
		}
		_, stopped := leaf.(shareServeStopCause)
		return stopped
	}):
		return shareComponentSettlement{outcome: shareComponentInterrupted}
	default:
		return shareComponentSettlement{outcome: shareComponentFailed, failure: err}
	}
}

func settleShareStop(
	err error,
	terminals shareTerminalSummary,
) shareComponentSettlement {
	if err != nil {
		// Factory stop owns a background cleanup budget independent of the CLI
		// context. Caller interruption therefore cannot naturalize a non-nil
		// terminal-delivery or durable-route-cleanup result.
		return shareComponentSettlement{outcome: shareComponentFailed, failure: err}
	}
	if terminals.failedSessions != 0 {
		err = fmt.Errorf("%w: failed_sessions=%d", errShareTerminalDecisionFailed, terminals.failedSessions)
		return shareComponentSettlement{outcome: shareComponentFailed, failure: err}
	}
	return shareComponentSettlement{outcome: shareComponentCompleted}
}

func (settlement shareLifecycleSettlement) Err() error {
	return errors.Join(settlement.serve.failure, settlement.stop.failure)
}

func shareTriggerAfterServe(ctxErr, serveErr error) shareShutdownTrigger {
	if ctxErr != nil && errorTreeContainsOnly(serveErr, func(leaf error) bool {
		return exactShareInterruption(leaf, ctxErr)
	}) {
		return shareShutdownCallerInterrupted
	}
	return shareShutdownServeEnded
}

func exactShareInterruption(candidate, interruption error) bool {
	if candidate == nil || interruption == nil {
		return false
	}
	candidateValue := reflect.ValueOf(candidate)
	interruptionValue := reflect.ValueOf(interruption)
	// errors.Is is intentionally too broad here: an arbitrary serve failure may
	// advertise cancellation through Is without proving that cancellation is its
	// entire terminal cause. Exact comparable identity keeps that authority on the
	// context's concrete terminal value without risking an interface panic.
	return candidateValue.Type() == interruptionValue.Type() &&
		candidateValue.Comparable() && candidateValue.Equal(interruptionValue)
}

func awaitInterruptedShareServe(serveDone <-chan error, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-serveDone:
		return err
	case <-timer.C:
		return errShareServeJoinTimedOut
	}
}

func errorTreeContainsOnly(err error, allowed func(error) bool) bool {
	if err == nil || allowed == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		observedLeaf := false
		for _, child := range children {
			if child == nil {
				continue
			}
			observedLeaf = true
			if !errorTreeContainsOnly(child, allowed) {
				return false
			}
		}
		return observedLeaf
	}
	if wrapped := errors.Unwrap(err); wrapped != nil {
		return errorTreeContainsOnly(wrapped, allowed)
	}
	// The concrete leaf is deliberate: an error that merely advertises an Is
	// relation has not proven that cancellation is its entire failure tree.
	return allowed(err)
}
