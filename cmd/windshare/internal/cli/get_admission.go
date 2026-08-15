package cli

import (
	"errors"
	"sync"
	"time"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/internal/testrun"
)

const receiverRelayAdmissionWindow = 8 * time.Second

var (
	ErrInvalidReceiverAdmission      = errors.New("receiver content admission signal is invalid")
	errReceiverAdmissionResumePanics = errors.New("receiver content admission resume panicked")
	errReceiverP2PPathUnavailable    = errors.New("p2p-only download requires an active direct peer path")
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

type receiverContentAdmission interface {
	ObserveConnectionSize(transfer.ConnectionSizeClass) error
	ObservePeer(receiverPeerSignal) error
	AdmitRelayOnly() error
	Decision() <-chan receiverAdmissionDecision
	Close()
	Wait()
	Err() error
	Traces() []receiverContentAdmissionTrace
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
	receiverAdmissionTriggerNone            receiverAdmissionTrigger = "none"
	receiverAdmissionTriggerDeadline        receiverAdmissionTrigger = "deadline"
	receiverAdmissionTriggerConnectionSmall receiverAdmissionTrigger = "small_connection_size"
	receiverAdmissionTriggerPeerFailed      receiverAdmissionTrigger = "peer_failed"
	receiverAdmissionTriggerPeerDetached    receiverAdmissionTrigger = "peer_detached"
	receiverAdmissionTriggerRelayOnly       receiverAdmissionTrigger = "relay_only_policy"
	receiverAdmissionTriggerP2POnly         receiverAdmissionTrigger = "p2p_only_policy"
)

type receiverAdmissionTerminalOwner string

const (
	receiverAdmissionTerminalNone           receiverAdmissionTerminalOwner = "none"
	receiverAdmissionTerminalLifecycle      receiverAdmissionTerminalOwner = "lifecycle_close"
	receiverAdmissionTerminalPeerFatal      receiverAdmissionTerminalOwner = "peer_session_fatal"
	receiverAdmissionTerminalRuntime        receiverAdmissionTerminalOwner = "runtime_terminal"
	receiverAdmissionTerminalResumeFailed   receiverAdmissionTerminalOwner = "resume_failure"
	receiverAdmissionTerminalP2PUnavailable receiverAdmissionTerminalOwner = "p2p_unavailable"
)

type receiverContentAdmissionResult string

const (
	receiverContentAdmissionClaimed           receiverContentAdmissionResult = "claimed"
	receiverContentAdmissionRevoked           receiverContentAdmissionResult = "queued_revoked"
	receiverContentAdmissionUnissued          receiverContentAdmissionResult = "unissued"
	receiverContentAdmissionExecutionRetained receiverContentAdmissionResult = "execution_retained"
	receiverContentAdmissionAlreadyDecided    receiverContentAdmissionResult = "already_decided"
	receiverContentAdmissionSettled           receiverContentAdmissionResult = "settled"
	receiverContentAdmissionResumeFailed      receiverContentAdmissionResult = "resume_failed"
	receiverContentAdmissionRelayProhibited   receiverContentAdmissionResult = "relay_content_prohibited"
	receiverContentAdmissionP2PUnavailable    receiverContentAdmissionResult = "p2p_unavailable"
)

type receiverAdmissionAuthority struct {
	generation uint64
	trigger    receiverAdmissionTrigger
	workerDone chan struct{}
}

type receiverContentAdmissionTrace struct {
	Sequence      uint64
	Generation    uint64
	Trigger       receiverAdmissionTrigger
	TerminalOwner receiverAdmissionTerminalOwner
	Result        receiverContentAdmissionResult
}

type receiverAdmissionExecution struct {
	// A channel gate makes the queued/revoked interleaving deterministic without
	// placing an injectable scheduler inside the authority lock. Production is nil.
	claimGate <-chan struct{}
}

type receiverAdmissionDecision struct {
	Cause         error
	Trigger       receiverAdmissionTrigger
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
	traces         []receiverContentAdmissionTrace
}

func newReceiverContentAdmissionWithExecution(
	mode receiverRelayContentMode,
	downloadT0 time.Time,
	clock receiverAdmissionClock,
	relay receiverContentSuspension,
	execution receiverAdmissionExecution,
) (receiverContentAdmission, error) {
	switch mode {
	case receiverRelayContentImmediate, receiverRelayContentAdaptive:
		return newRelayContentAdmissionWithExecution(downloadT0, clock, relay, execution)
	case receiverRelayContentProhibited:
		return newP2POnlyContentAdmission(relay)
	default:
		return nil, ErrInvalidReceiverAdmission
	}
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

func (admission *relayContentAdmission) ObserveConnectionSize(size transfer.ConnectionSizeClass) error {
	switch size {
	case transfer.ConnectionSizeSmall:
		admission.beginDecision(receiverAdmissionTriggerConnectionSmall)
		return nil
	case transfer.ConnectionSizeUnknown, transfer.ConnectionSizeLarge:
		return nil
	default:
		return ErrInvalidReceiverAdmission
	}
}

func (admission *relayContentAdmission) AdmitRelayOnly() error {
	admission.beginDecision(receiverAdmissionTriggerRelayOnly)
	return nil
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
	result := receiverContentAdmissionSettled
	if err != nil {
		result = receiverContentAdmissionResumeFailed
	}
	admission.recordTraceLocked(authority, admission.terminalOwner, result)
	admission.decisions <- receiverAdmissionDecision{
		Cause:         err,
		Trigger:       authority.trigger,
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
		receiverContentAdmissionClaimed,
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

func (admission *relayContentAdmission) Traces() []receiverContentAdmissionTrace {
	if admission == nil {
		return nil
	}
	admission.mu.Lock()
	defer admission.mu.Unlock()
	return append([]receiverContentAdmissionTrace(nil), admission.traces...)
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
		result := receiverContentAdmissionUnissued
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
			result = receiverContentAdmissionRevoked
		case receiverAdmissionExecuting:
			// A claimed external call cannot be canceled safely. Close revokes all
			// future authority; Wait is the exact completion barrier for this one.
			result = receiverContentAdmissionExecutionRetained
		case receiverAdmissionDecided:
			joinableWorkerDone = admission.authority.workerDone
			result = receiverContentAdmissionAlreadyDecided
		case receiverAdmissionRevoked:
			result = receiverContentAdmissionAlreadyDecided
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
	result receiverContentAdmissionResult,
) {
	admission.traceSequence++
	trace := receiverContentAdmissionTrace{
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

type p2pOnlyContentAdmission struct {
	// Retaining the exact suspension documents ownership of the permanent hold.
	// Releasing it would violate the user-visible promise even after a reconnect.
	relayHold receiverContentSuspension

	finishOnce sync.Once
	finished   chan struct{}
	decisions  chan receiverAdmissionDecision

	mu       sync.Mutex
	cause    error
	sequence uint64
	traces   []receiverContentAdmissionTrace
}

func newP2POnlyContentAdmission(
	relayHold receiverContentSuspension,
) (*p2pOnlyContentAdmission, error) {
	if relayHold == nil {
		return nil, ErrInvalidReceiverAdmission
	}
	admission := &p2pOnlyContentAdmission{
		relayHold: relayHold,
		finished:  make(chan struct{}),
		decisions: make(chan receiverAdmissionDecision, 1),
	}
	admission.recordTraceLocked(
		receiverAdmissionTriggerP2POnly,
		receiverAdmissionTerminalNone,
		receiverContentAdmissionRelayProhibited,
	)
	return admission, nil
}

func (admission *p2pOnlyContentAdmission) ObserveConnectionSize(
	size transfer.ConnectionSizeClass,
) error {
	switch size {
	case transfer.ConnectionSizeUnknown, transfer.ConnectionSizeSmall, transfer.ConnectionSizeLarge:
		return nil
	default:
		return ErrInvalidReceiverAdmission
	}
}

func (admission *p2pOnlyContentAdmission) ObservePeer(signal receiverPeerSignal) error {
	switch signal {
	case receiverPeerReady:
		return nil
	case receiverPeerFailed:
		admission.fail(receiverAdmissionTriggerPeerFailed)
		return nil
	case receiverPeerDetached:
		admission.fail(receiverAdmissionTriggerPeerDetached)
		return nil
	case receiverPeerSessionFatal:
		admission.stop(receiverAdmissionTerminalPeerFatal)
		return nil
	case receiverPeerRuntimeTerminal:
		admission.stop(receiverAdmissionTerminalRuntime)
		return nil
	default:
		return ErrInvalidReceiverAdmission
	}
}

func (*p2pOnlyContentAdmission) AdmitRelayOnly() error {
	return ErrInvalidReceiverAdmission
}

func (admission *p2pOnlyContentAdmission) fail(trigger receiverAdmissionTrigger) {
	admission.finishOnce.Do(func() {
		admission.mu.Lock()
		admission.cause = errReceiverP2PPathUnavailable
		admission.recordTraceLocked(
			trigger,
			receiverAdmissionTerminalP2PUnavailable,
			receiverContentAdmissionP2PUnavailable,
		)
		admission.mu.Unlock()
		admission.decisions <- receiverAdmissionDecision{
			Cause:         errReceiverP2PPathUnavailable,
			Trigger:       trigger,
			TerminalOwner: receiverAdmissionTerminalP2PUnavailable,
		}
		close(admission.decisions)
		close(admission.finished)
	})
}

func (admission *p2pOnlyContentAdmission) stop(owner receiverAdmissionTerminalOwner) {
	admission.finishOnce.Do(func() {
		admission.mu.Lock()
		admission.recordTraceLocked(
			receiverAdmissionTriggerNone,
			owner,
			receiverContentAdmissionUnissued,
		)
		admission.mu.Unlock()
		close(admission.decisions)
		close(admission.finished)
	})
}

func (admission *p2pOnlyContentAdmission) Decision() <-chan receiverAdmissionDecision {
	return admission.decisions
}

func (admission *p2pOnlyContentAdmission) Close() {
	if admission != nil {
		admission.stop(receiverAdmissionTerminalLifecycle)
	}
}

func (admission *p2pOnlyContentAdmission) Wait() {
	if admission != nil {
		<-admission.finished
	}
}

func (admission *p2pOnlyContentAdmission) Err() error {
	if admission == nil {
		return nil
	}
	admission.mu.Lock()
	defer admission.mu.Unlock()
	return admission.cause
}

func (admission *p2pOnlyContentAdmission) Traces() []receiverContentAdmissionTrace {
	if admission == nil {
		return nil
	}
	admission.mu.Lock()
	defer admission.mu.Unlock()
	return append([]receiverContentAdmissionTrace(nil), admission.traces...)
}

func (admission *p2pOnlyContentAdmission) recordTraceLocked(
	trigger receiverAdmissionTrigger,
	owner receiverAdmissionTerminalOwner,
	result receiverContentAdmissionResult,
) {
	admission.sequence++
	admission.traces = append(admission.traces, receiverContentAdmissionTrace{
		Sequence: admission.sequence, Trigger: trigger, TerminalOwner: owner, Result: result,
	})
}

func (a *App) logReceiverAdmissionTraces(sessionID []byte, admission receiverContentAdmission) {
	for _, trace := range admission.Traces() {
		a.logf(
			"get: content path decision session_id=%x sequence=%d admission_generation=%d trigger=%s terminal_owner=%s result=%s",
			sessionID,
			trace.Sequence,
			trace.Generation,
			trace.Trigger,
			trace.TerminalOwner,
			trace.Result,
		)
		if trace.Result == receiverContentAdmissionSettled {
			a.recordProcessTrace(
				processTraceGetComponent,
				processTraceReceiverRelayContent,
				testrun.OutcomeSucceeded,
			)
		}
	}
}
