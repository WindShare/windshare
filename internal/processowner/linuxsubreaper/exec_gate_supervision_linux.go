//go:build linux

package linuxsubreaper

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
)

const rootTerminalPublicationJoinLimit = 500 * time.Millisecond

type launchPhase uint8

const (
	launchGateHeld launchPhase = iota
	launchGateReleased
	launchConfirmed
	launchFailed
	launchPrevented
	launchEvidenceLost
)

type supervisionState struct {
	terminationReason  string
	rootTerminal       *terminalResult
	authorityFailure   error
	launchPhase        launchPhase
	execResultObserved bool
	target             ownerprotocol.TargetEvidence
}

func awaitExecGate(
	request ownerprotocol.Request,
	authorityFailure error,
	targetAuthority *executableAuthority,
	starts *startGate,
	startEvidence ownerprotocol.StartEvidence,
	releaseWriter *os.File,
	ready <-chan error,
	execResults <-chan execResult,
	wait <-chan terminalResult,
	control <-chan string,
	deadline <-chan time.Time,
) supervisionState {
	state := supervisionState{
		terminationReason: ownerprotocol.TerminationNatural,
		authorityFailure:  authorityFailure,
	}
	if authorityFailure != nil {
		state.terminationReason = ownerprotocol.TerminationOwnerFailure
		return preventLaunch(state, "OWNER_AUTHORITY_FAILED", authorityFailure)
	}
	select {
	case err := <-ready:
		if err != nil {
			state.authorityFailure = fmt.Errorf("wait for exec gate: %w", err)
			return preventLaunch(state, "EXEC_GATE_NOT_READY", state.authorityFailure)
		}
		var accepted bool
		state, accepted = authorizeExecGateStart(state, starts, startEvidence, request, control, deadline)
		if !accepted {
			return state
		}
		return releaseExecGate(state, targetAuthority, releaseWriter, execResults, wait, control, deadline)
	case outcome := <-control:
		state.terminationReason = outcome
		state = preventLaunchForTermination(state, outcome)
		if outcome == ownerprotocol.TerminationOwnerFailure {
			state.authorityFailure = errors.New("control authority failed before exec-gate readiness")
		}
	case <-deadline:
		state.terminationReason = ownerprotocol.TerminationDeadline
		state.authorityFailure = errors.New("owner deadline expired before exec-gate readiness")
		state = preventLaunch(state, "DEADLINE_BEFORE_LAUNCH", state.authorityFailure)
	case result := <-wait:
		state.rootTerminal = &result
		state.authorityFailure = errors.New("exec gate exited before release")
		state = preventLaunch(state, "EXEC_GATE_EXITED_BEFORE_LAUNCH", state.authorityFailure)
	}
	return state
}

func authorizeExecGateStart(
	state supervisionState,
	starts *startGate,
	evidence ownerprotocol.StartEvidence,
	request ownerprotocol.Request,
	control <-chan string,
	deadline <-chan time.Time,
) (supervisionState, bool) {
	if err := ownerprotocol.ValidateStartEvidenceForRequest(evidence, request); err != nil {
		state.authorityFailure = fmt.Errorf("validate locally derived start evidence: %w", err)
		state.terminationReason = ownerprotocol.TerminationOwnerFailure
		return preventLaunch(state, "START_EVIDENCE_INVALID", state.authorityFailure), false
	}
	decisions, err := starts.publish(evidence)
	if err != nil {
		state.authorityFailure = err
		state.terminationReason = ownerprotocol.TerminationOwnerFailure
		return preventLaunch(state, "START_EVIDENCE_PUBLICATION_FAILED", err), false
	}

	trigger := ""
	var triggerTimer *time.Timer
	var triggerDeadline <-chan time.Time
	recordTrigger := func(outcome string) {
		if trigger != "" {
			return
		}
		trigger = outcome
		control = nil
		deadline = nil
		triggerTimer = time.NewTimer(
			time.Duration(request.TerminationGraceMilliseconds) * time.Millisecond,
		)
		triggerDeadline = triggerTimer.C
	}
	defer func() {
		if triggerTimer != nil {
			triggerTimer.Stop()
		}
	}()

	for {
		select {
		case result := <-decisions:
			if result.err == nil {
				result.err = ownerprotocol.ValidateStartDecisionForEvidence(result.decision, evidence)
			}
			if result.err != nil {
				state.authorityFailure = fmt.Errorf("authenticate start decision: %w", result.err)
				state.terminationReason = ownerprotocol.TerminationOwnerFailure
				return preventLaunch(state, "START_DECISION_INVALID", state.authorityFailure), false
			}
			if trigger == "" {
				select {
				case outcome := <-control:
					recordTrigger(outcome)
				default:
					select {
					case <-deadline:
						recordTrigger(ownerprotocol.TerminationDeadline)
					default:
					}
				}
			}
			if trigger != "" {
				state.terminationReason = trigger
				state = preventLaunchForTermination(state, trigger)
				if trigger == ownerprotocol.TerminationOwnerFailure {
					state.authorityFailure = errors.New("control authority failed during start authorization")
				}
				return state, false
			}
			if result.decision.Outcome == ownerprotocol.StartDecisionRejected {
				state.terminationReason = ownerprotocol.TerminationStartRejected
				rejection := fmt.Errorf(
					"%s: %s",
					result.decision.FailureCode,
					result.decision.FailureMessage,
				)
				return preventLaunch(state, "START_REJECTED", rejection), false
			}
			return state, true
		case outcome := <-control:
			recordTrigger(outcome)
		case <-deadline:
			recordTrigger(ownerprotocol.TerminationDeadline)
		case <-triggerDeadline:
			state.authorityFailure = errors.New(
				"start decision did not arrive within termination grace after a pre-release trigger",
			)
			state.terminationReason = ownerprotocol.TerminationOwnerFailure
			return preventLaunch(state, "START_DECISION_TIMEOUT", state.authorityFailure), false
		}
	}
}

func releaseExecGate(
	state supervisionState,
	targetAuthority *executableAuthority,
	releaseWriter *os.File,
	execResults <-chan execResult,
	wait <-chan terminalResult,
	control <-chan string,
	deadline <-chan time.Time,
) supervisionState {
	if err := targetAuthority.assertLive(); err != nil {
		state.authorityFailure = fmt.Errorf("revalidate held target before release: %w", err)
		state.terminationReason = ownerprotocol.TerminationOwnerFailure
		return preventLaunch(state, "EXECUTABLE_AUTHORITY_FAILED", state.authorityFailure)
	}
	select {
	case outcome := <-control:
		state.terminationReason = outcome
		state = preventLaunchForTermination(state, outcome)
		if outcome == ownerprotocol.TerminationOwnerFailure {
			state.authorityFailure = errors.New("control authority failed before exec-gate release")
		}
		return state
	case <-deadline:
		state.terminationReason = ownerprotocol.TerminationDeadline
		state.authorityFailure = errors.New("owner deadline expired before exec-gate release")
		return preventLaunch(state, "DEADLINE_BEFORE_LAUNCH", state.authorityFailure)
	default:
	}
	written, err := releaseWriter.Write([]byte{1})
	if err != nil || written != 1 {
		if err == nil {
			err = io.ErrShortWrite
		}
		state.authorityFailure = fmt.Errorf("release exec gate: %w", err)
		state.terminationReason = ownerprotocol.TerminationOwnerFailure
		return preventLaunch(state, "EXEC_GATE_RELEASE_FAILED", state.authorityFailure)
	}
	state.launchPhase = launchGateReleased
	if err := releaseWriter.Close(); err != nil {
		state.authorityFailure = fmt.Errorf("release exec gate: %w", err)
		state.terminationReason = ownerprotocol.TerminationOwnerFailure
		return loseLaunchEvidence(state, state.authorityFailure)
	}
	return awaitExecConfirmation(state, execResults, wait, control, deadline)
}

func awaitExecConfirmation(
	state supervisionState,
	execResults <-chan execResult,
	wait <-chan terminalResult,
	control <-chan string,
	deadline <-chan time.Time,
) supervisionState {
	select {
	case result := <-execResults:
		return applyExecResult(state, result)
	case terminal := <-wait:
		state.rootTerminal = &terminal
		return awaitTerminalExecResult(state, execResults, control, deadline)
	case outcome := <-control:
		state = consumeBufferedExecResult(state, execResults)
		state.terminationReason = outcome
		if !state.launchResolved() {
			state = loseLaunchEvidence(state, errors.New("termination was accepted before target start could be confirmed"))
		}
		if outcome == ownerprotocol.TerminationOwnerFailure {
			state.authorityFailure = errors.Join(state.authorityFailure, errors.New("control authority failed during exec confirmation"))
		}
		return state
	case <-deadline:
		state = consumeBufferedExecResult(state, execResults)
		state.terminationReason = ownerprotocol.TerminationDeadline
		if !state.launchResolved() {
			state = loseLaunchEvidence(state, errors.New("owner deadline expired before target start could be confirmed"))
		}
		return state
	}
}

func awaitTerminalExecResult(
	state supervisionState,
	execResults <-chan execResult,
	control <-chan string,
	deadline <-chan time.Time,
) supervisionState {
	timer := time.NewTimer(rootTerminalPublicationJoinLimit)
	defer timer.Stop()
	select {
	case result := <-execResults:
		return applyExecResult(state, result)
	case outcome := <-control:
		state.terminationReason = outcome
		return loseLaunchEvidence(state, errors.New("exec gate terminated before start evidence was observed"))
	case <-deadline:
		state.terminationReason = ownerprotocol.TerminationDeadline
		return loseLaunchEvidence(state, errors.New("owner deadline expired before terminal start evidence was observed"))
	case <-timer.C:
		return loseLaunchEvidence(state, errors.New("terminal exec-result evidence exceeded its bounded drain"))
	}
}

func consumeBufferedExecResult(state supervisionState, execResults <-chan execResult) supervisionState {
	select {
	case result := <-execResults:
		return applyExecResult(state, result)
	default:
		return state
	}
}

func resolveExecResult(
	state supervisionState,
	execResults <-chan execResult,
	reader *os.File,
	deadline time.Time,
) supervisionState {
	if state.execResultObserved {
		return state
	}
	state = consumeBufferedExecResult(state, execResults)
	if state.execResultObserved {
		return state
	}
	if state.launchPhase == launchPrevented || state.launchPhase == launchEvidenceLost {
		_ = reader.Close()
		state.execResultObserved = true
		return state
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		_ = reader.Close()
		state = loseLaunchEvidence(state, errors.New("exec-result evidence did not settle before the cleanup deadline"))
		state.execResultObserved = true
		return state
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case result := <-execResults:
		return applyExecResult(state, result)
	case <-timer.C:
		_ = reader.Close()
		state = loseLaunchEvidence(state, errors.New("exec-result evidence did not settle before the cleanup deadline"))
		state.execResultObserved = true
		return state
	}
}

func applyExecResult(state supervisionState, result execResult) supervisionState {
	state.execResultObserved = true
	if state.launchPhase == launchPrevented || state.launchPhase == launchEvidenceLost {
		return state
	}
	switch {
	case result.started:
		if state.launchPhase != launchGateReleased {
			return loseLaunchEvidence(state, errors.New("exec success evidence arrived outside the released launch phase"))
		}
		state.launchPhase = launchConfirmed
	case result.failure != nil:
		state.launchPhase = launchFailed
		state.target = ownerprotocol.TargetEvidence{
			Outcome: ownerprotocol.TargetSpawnFailed, FailureCode: result.failure.FailureCode,
			FailureMessage: result.failure.FailureMessage,
		}
		if state.terminationReason == ownerprotocol.TerminationNatural {
			state.terminationReason = ownerprotocol.TerminationInitializationFailed
		}
	default:
		state = loseLaunchEvidence(state, result.err)
	}
	return state
}

func (state supervisionState) launched() bool {
	return state.launchPhase == launchConfirmed
}

func (state supervisionState) launchResolved() bool {
	return state.launchPhase == launchConfirmed || state.launchPhase == launchFailed ||
		state.launchPhase == launchPrevented || state.launchPhase == launchEvidenceLost
}

func preventLaunchForTermination(state supervisionState, outcome string) supervisionState {
	switch outcome {
	case ownerprotocol.TerminationParentLost:
		return preventLaunch(state, "PARENT_LOST_BEFORE_LAUNCH", errors.New("parent authority ended before target launch"))
	case ownerprotocol.TerminationDeadline:
		return preventLaunch(state, "DEADLINE_BEFORE_LAUNCH", errors.New("owner deadline expired before target launch"))
	default:
		return preventLaunch(state, "CONTROL_BEFORE_LAUNCH", errors.New("owner control prevented target launch"))
	}
}

func preventLaunch(state supervisionState, code string, cause error) supervisionState {
	state.launchPhase = launchPrevented
	state.target = ownerprotocol.TargetEvidence{
		Outcome: ownerprotocol.TargetNotStarted, FailureCode: code, FailureMessage: boundedDiagnostic(cause),
	}
	return state
}

func loseLaunchEvidence(state supervisionState, cause error) supervisionState {
	if cause == nil {
		cause = errors.New("target start evidence was lost")
	}
	state.launchPhase = launchEvidenceLost
	state.target = ownerprotocol.TargetEvidence{
		Outcome: ownerprotocol.TargetStartEvidenceLost, FailureCode: "TARGET_START_EVIDENCE_LOST",
		FailureMessage: boundedDiagnostic(cause),
	}
	state.authorityFailure = errors.Join(state.authorityFailure, cause)
	if state.terminationReason == ownerprotocol.TerminationNatural {
		state.terminationReason = ownerprotocol.TerminationOwnerFailure
	}
	return state
}
