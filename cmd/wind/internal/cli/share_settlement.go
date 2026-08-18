package cli

import (
	"errors"
	"reflect"
	"time"
)

var errShareServeJoinTimedOut = errors.New("share session server did not stop after interruption")

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

type shareComponentSettlement struct {
	outcome shareComponentOutcome
	failure error
}

type shareLifecycleSettlement struct {
	trigger  shareShutdownTrigger
	serve    shareComponentSettlement
	stop     shareComponentSettlement
	decision shareSettlementDecision
}

func settleShareLifecycle(
	trigger shareShutdownTrigger,
	interruption error,
	serveErr error,
	stopErr error,
) shareLifecycleSettlement {
	settlement := shareLifecycleSettlement{
		trigger: trigger,
		serve:   settleShareServe(trigger, interruption, serveErr),
	}
	settlement.stop = settleShareStop(stopErr)
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

func settleShareStop(err error) shareComponentSettlement {
	if err != nil {
		// Factory stop owns a background cleanup budget independent of the CLI
		// context. Caller interruption therefore cannot naturalize a non-nil
		// terminal-delivery or durable-route-cleanup result.
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
