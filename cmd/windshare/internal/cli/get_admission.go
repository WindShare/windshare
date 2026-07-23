package cli

import (
	"errors"
	"sync"
	"time"

	"github.com/windshare/windshare/core/transfer"
)

const receiverRelayAdmissionWindow = 8 * time.Second

var (
	ErrInvalidReceiverAdmission      = errors.New("receiver content admission signal is invalid")
	errReceiverAdmissionResumePanics = errors.New("receiver content admission resume panicked")
)

type receiverPeerSignal uint8

const (
	receiverPeerReady receiverPeerSignal = iota + 1
	receiverPeerFailed
	receiverPeerDetached
	receiverPeerSessionFatal
	receiverPeerRuntimeTerminal
)

type receiverContentSuspension interface {
	Resume() error
}

type receiverAdmissionState uint8

const (
	receiverAdmissionPending receiverAdmissionState = iota
	receiverAdmissionQueued
	receiverAdmissionExecuting
	receiverAdmissionDecided
	receiverAdmissionRevoked
)

type receiverAdmissionTrigger string

const (
	receiverAdmissionTriggerNone         receiverAdmissionTrigger = "none"
	receiverAdmissionTriggerDeadline     receiverAdmissionTrigger = "deadline"
	receiverAdmissionTriggerSmall        receiverAdmissionTrigger = "small_selection"
	receiverAdmissionTriggerPeerFailed   receiverAdmissionTrigger = "peer_failed"
	receiverAdmissionTriggerPeerDetached receiverAdmissionTrigger = "peer_detached"
)

type receiverAdmissionTerminalOwner string

const (
	receiverAdmissionTerminalNone         receiverAdmissionTerminalOwner = "none"
	receiverAdmissionTerminalLifecycle    receiverAdmissionTerminalOwner = "lifecycle_close"
	receiverAdmissionTerminalPeerFatal    receiverAdmissionTerminalOwner = "peer_session_fatal"
	receiverAdmissionTerminalRuntime      receiverAdmissionTerminalOwner = "runtime_terminal"
	receiverAdmissionTerminalResumeFailed receiverAdmissionTerminalOwner = "resume_failure"
)

type receiverAdmissionAuthorityResult string

const (
	receiverAdmissionAuthorityClaimed           receiverAdmissionAuthorityResult = "claimed"
	receiverAdmissionAuthorityRevoked           receiverAdmissionAuthorityResult = "queued_revoked"
	receiverAdmissionAuthorityUnissued          receiverAdmissionAuthorityResult = "unissued"
	receiverAdmissionAuthorityExecutionRetained receiverAdmissionAuthorityResult = "execution_retained"
	receiverAdmissionAuthorityAlreadyDecided    receiverAdmissionAuthorityResult = "already_decided"
	receiverAdmissionAuthoritySettled           receiverAdmissionAuthorityResult = "settled"
	receiverAdmissionAuthorityResumeFailed      receiverAdmissionAuthorityResult = "resume_failed"
)

type receiverAdmissionAuthority struct {
	generation uint64
	trigger    receiverAdmissionTrigger
	workerDone chan struct{}
}

type receiverAdmissionAuthorityTrace struct {
	Sequence      uint64
	Generation    uint64
	Trigger       receiverAdmissionTrigger
	TerminalOwner receiverAdmissionTerminalOwner
	Result        receiverAdmissionAuthorityResult
}

type receiverAdmissionExecution struct {
	// A channel gate makes the queued/revoked interleaving deterministic without
	// placing an injectable scheduler inside the authority lock. Production is nil.
	claimGate <-chan struct{}
}

type receiverAdmissionDecision struct {
	Cause         error
	TerminalOwner receiverAdmissionTerminalOwner
}

type receiverAdmissionTimer interface {
	C() <-chan time.Time
	Stop() bool
}

type receiverAdmissionClock interface {
	Now() time.Time
	NewTimer(time.Duration) receiverAdmissionTimer
}

type wallReceiverAdmissionClock struct{}

func (wallReceiverAdmissionClock) Now() time.Time { return time.Now() }
func (wallReceiverAdmissionClock) NewTimer(delay time.Duration) receiverAdmissionTimer {
	return wallReceiverAdmissionTimer{Timer: time.NewTimer(delay)}
}

type wallReceiverAdmissionTimer struct{ *time.Timer }

func (timer wallReceiverAdmissionTimer) C() <-chan time.Time { return timer.Timer.C }

func (a *App) admissionClock() receiverAdmissionClock {
	if a.receiverClock != nil {
		return a.receiverClock
	}
	return wallReceiverAdmissionClock{}
}

type relayContentAdmission struct {
	relay receiverContentSuspension
	timer receiverAdmissionTimer
	exec  receiverAdmissionExecution

	done         chan struct{}
	finished     chan struct{}
	decisions    chan receiverAdmissionDecision
	decisionDone chan struct{}
	closeOnce    sync.Once

	mu             sync.Mutex
	state          receiverAdmissionState
	resumeError    error
	authority      *receiverAdmissionAuthority
	terminalOwner  receiverAdmissionTerminalOwner
	nextGeneration uint64
	traceSequence  uint64
	traces         []receiverAdmissionAuthorityTrace
}

func newRelayContentAdmission(
	downloadT0 time.Time,
	clock receiverAdmissionClock,
	relay receiverContentSuspension,
) (*relayContentAdmission, error) {
	return newRelayContentAdmissionWithExecution(
		downloadT0,
		clock,
		relay,
		receiverAdmissionExecution{},
	)
}

func newRelayContentAdmissionWithExecution(
	downloadT0 time.Time,
	clock receiverAdmissionClock,
	relay receiverContentSuspension,
	execution receiverAdmissionExecution,
) (*relayContentAdmission, error) {
	if downloadT0.IsZero() || clock == nil || relay == nil {
		return nil, ErrInvalidReceiverAdmission
	}
	delay := max(downloadT0.Add(receiverRelayAdmissionWindow).Sub(clock.Now()), 0)
	timer := clock.NewTimer(delay)
	if timer == nil || timer.C() == nil {
		// Timer construction is part of admission setup. Roll back the suspension
		// so a broken injected clock cannot strand every subsequent content fetch.
		return nil, errors.Join(ErrInvalidReceiverAdmission, resumeReceiverContent(relay))
	}
	admission := &relayContentAdmission{
		relay: relay, timer: timer, exec: execution,
		done: make(chan struct{}), finished: make(chan struct{}),
		decisions:     make(chan receiverAdmissionDecision, 1),
		decisionDone:  make(chan struct{}),
		terminalOwner: receiverAdmissionTerminalNone,
	}
	go admission.runDeadline()
	return admission, nil
}

func (admission *relayContentAdmission) runDeadline() {
	defer close(admission.finished)
	select {
	case <-admission.done:
		return
	case <-admission.timer.C():
		admission.beginDecision(receiverAdmissionTriggerDeadline)
	}
}

func (admission *relayContentAdmission) ObserveSelection(class transfer.SelectionClass) error {
	switch class {
	case transfer.SelectionSmall:
		admission.beginDecision(receiverAdmissionTriggerSmall)
		return nil
	case transfer.SelectionUnknown, transfer.SelectionLarge:
		return nil
	default:
		return ErrInvalidReceiverAdmission
	}
}

func (admission *relayContentAdmission) ObservePeer(signal receiverPeerSignal) error {
	switch signal {
	case receiverPeerFailed:
		admission.beginDecision(receiverAdmissionTriggerPeerFailed)
		return nil
	case receiverPeerDetached:
		admission.beginDecision(receiverAdmissionTriggerPeerDetached)
		return nil
	case receiverPeerReady:
		return nil
	case receiverPeerSessionFatal:
		admission.close(receiverAdmissionTerminalPeerFatal)
		return nil
	case receiverPeerRuntimeTerminal:
		admission.close(receiverAdmissionTerminalRuntime)
		return nil
	default:
		return ErrInvalidReceiverAdmission
	}
}

func (admission *relayContentAdmission) beginDecision(trigger receiverAdmissionTrigger) {
	admission.mu.Lock()
	defer admission.mu.Unlock()
	if admission.terminalOwner != receiverAdmissionTerminalNone || admission.state != receiverAdmissionPending {
		return
	}
	admission.nextGeneration++
	authority := &receiverAdmissionAuthority{
		generation: admission.nextGeneration,
		trigger:    trigger,
		workerDone: make(chan struct{}),
	}
	admission.authority = authority
	admission.state = receiverAdmissionQueued
	// Starting the owned worker while publication is locked prevents Close from
	// returning before the exact revocable capability has a completion path.
	go admission.completeDecision(authority)
}

func (admission *relayContentAdmission) completeDecision(authority *receiverAdmissionAuthority) {
	defer close(authority.workerDone)
	if admission.exec.claimGate != nil {
		select {
		case <-admission.exec.claimGate:
		case <-admission.done:
		}
	}
	if !admission.claimDecision(authority) {
		return
	}
	// Resume is synchronous and bounded in production, but remains outside the
	// authority lock because injected implementations may reenter or panic.
	err := resumeReceiverContent(admission.relay)
	admission.mu.Lock()
	defer admission.mu.Unlock()
	if admission.authority != authority || admission.state != receiverAdmissionExecuting {
		return
	}
	admission.resumeError = err
	if err != nil && admission.terminalOwner == receiverAdmissionTerminalNone {
		// First terminal ownership decides who closes the runtime. A Resume error
		// that loses to fatal/runtime closure remains diagnostic, not new authority.
		admission.terminalOwner = receiverAdmissionTerminalResumeFailed
	}
	admission.state = receiverAdmissionDecided
	result := receiverAdmissionAuthoritySettled
	if err != nil {
		result = receiverAdmissionAuthorityResumeFailed
	}
	admission.recordTraceLocked(authority, admission.terminalOwner, result)
	admission.decisions <- receiverAdmissionDecision{
		Cause:         err,
		TerminalOwner: admission.terminalOwner,
	}
	close(admission.decisions)
	close(admission.decisionDone)
}

func (admission *relayContentAdmission) claimDecision(
	authority *receiverAdmissionAuthority,
) bool {
	admission.mu.Lock()
	defer admission.mu.Unlock()
	if admission.terminalOwner != receiverAdmissionTerminalNone ||
		admission.state != receiverAdmissionQueued || admission.authority != authority {
		return false
	}
	admission.state = receiverAdmissionExecuting
	admission.recordTraceLocked(
		authority,
		receiverAdmissionTerminalNone,
		receiverAdmissionAuthorityClaimed,
	)
	return true
}

func resumeReceiverContent(relay receiverContentSuspension) (err error) {
	defer func() {
		if recover() != nil {
			err = errReceiverAdmissionResumePanics
		}
	}()
	return relay.Resume()
}

func (admission *relayContentAdmission) Decision() <-chan receiverAdmissionDecision {
	return admission.decisions
}

func (admission *relayContentAdmission) Wait() {
	if admission == nil {
		return
	}
	<-admission.decisionDone
	admission.mu.Lock()
	var workerDone <-chan struct{}
	if admission.state == receiverAdmissionDecided && admission.authority != nil {
		workerDone = admission.authority.workerDone
	}
	admission.mu.Unlock()
	if workerDone != nil {
		// Decision publication precedes the worker's final defer. Joining that
		// exact generation prevents Wait from overstating lifecycle completion.
		<-workerDone
	}
}

func (admission *relayContentAdmission) Err() error {
	if admission == nil {
		return nil
	}
	admission.mu.Lock()
	defer admission.mu.Unlock()
	return admission.resumeError
}

func (admission *relayContentAdmission) Traces() []receiverAdmissionAuthorityTrace {
	if admission == nil {
		return nil
	}
	admission.mu.Lock()
	defer admission.mu.Unlock()
	return append([]receiverAdmissionAuthorityTrace(nil), admission.traces...)
}

func (admission *relayContentAdmission) decisionWorkerDone() <-chan struct{} {
	if admission == nil {
		return nil
	}
	admission.mu.Lock()
	defer admission.mu.Unlock()
	if admission.authority == nil {
		return nil
	}
	return admission.authority.workerDone
}

func (admission *relayContentAdmission) Close() {
	if admission == nil {
		return
	}
	admission.close(receiverAdmissionTerminalLifecycle)
}

func (admission *relayContentAdmission) close(owner receiverAdmissionTerminalOwner) {
	if admission == nil {
		return
	}
	admission.closeOnce.Do(func() {
		var joinableWorkerDone <-chan struct{}
		admission.mu.Lock()
		if admission.terminalOwner == receiverAdmissionTerminalNone {
			admission.terminalOwner = owner
		}
		result := receiverAdmissionAuthorityUnissued
		switch admission.state {
		case receiverAdmissionPending:
			admission.state = receiverAdmissionRevoked
			admission.finishWithoutDecisionLocked()
		case receiverAdmissionQueued:
			// Revocation and worker claim share this lock, so returning from Close
			// proves a worker that had not started can never call Resume later.
			admission.state = receiverAdmissionRevoked
			admission.finishWithoutDecisionLocked()
			joinableWorkerDone = admission.authority.workerDone
			result = receiverAdmissionAuthorityRevoked
		case receiverAdmissionExecuting:
			// A claimed external call cannot be canceled safely. Close revokes all
			// future authority; Wait is the exact completion barrier for this one.
			result = receiverAdmissionAuthorityExecutionRetained
		case receiverAdmissionDecided:
			joinableWorkerDone = admission.authority.workerDone
			result = receiverAdmissionAuthorityAlreadyDecided
		case receiverAdmissionRevoked:
			result = receiverAdmissionAuthorityAlreadyDecided
		}
		admission.recordTraceLocked(admission.authority, admission.terminalOwner, result)
		admission.mu.Unlock()
		admission.timer.Stop()
		close(admission.done)
		<-admission.finished
		if joinableWorkerDone != nil {
			// Revoked workers cannot call external code, so joining them here makes
			// Close leak-free without risking a Resume reentrancy deadlock. A
			// decided worker has likewise already returned from external code.
			<-joinableWorkerDone
		}
	})
}

func (admission *relayContentAdmission) finishWithoutDecisionLocked() {
	close(admission.decisions)
	close(admission.decisionDone)
}

func (admission *relayContentAdmission) recordTraceLocked(
	authority *receiverAdmissionAuthority,
	owner receiverAdmissionTerminalOwner,
	result receiverAdmissionAuthorityResult,
) {
	admission.traceSequence++
	trace := receiverAdmissionAuthorityTrace{
		Sequence:      admission.traceSequence,
		Trigger:       receiverAdmissionTriggerNone,
		TerminalOwner: owner,
		Result:        result,
	}
	if authority != nil {
		trace.Generation = authority.generation
		trace.Trigger = authority.trigger
	}
	admission.traces = append(admission.traces, trace)
}

func (a *App) logReceiverAdmissionTraces(sessionID []byte, admission *relayContentAdmission) {
	for _, trace := range admission.Traces() {
		a.logf(
			"get: relay admission authority session_id=%x sequence=%d admission_generation=%d trigger=%s terminal_owner=%s result=%s",
			sessionID,
			trace.Sequence,
			trace.Generation,
			trace.Trigger,
			trace.TerminalOwner,
			trace.Result,
		)
	}
}
