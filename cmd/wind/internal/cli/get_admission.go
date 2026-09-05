package cli

import (
	"errors"
	"sync"
)

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
	ObservePeer(receiverPeerSignal) error
	AdmitRelayOnly() error
	Decision() <-chan receiverAdmissionDecision
	Close()
	Wait()
	Err() error
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
	receiverAdmissionTriggerPeerFailed   receiverAdmissionTrigger = "peer_failed"
	receiverAdmissionTriggerPeerDetached receiverAdmissionTrigger = "peer_detached"
	receiverAdmissionTriggerRelayOnly    receiverAdmissionTrigger = "relay_only_policy"
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

type receiverAdmissionAuthority struct {
	generation uint64
	trigger    receiverAdmissionTrigger
	workerDone chan struct{}
}

type receiverAdmissionExecution struct {
	// The gate keeps content suspended until destination authority and the transfer
	// job exist. onClaim publishes the path decision before Resume can expose that
	// path to content requests, while both remain outside the authority lock.
	claimGate <-chan struct{}
	onClaim   func(receiverAdmissionTrigger)
}

type receiverAdmissionDecision struct {
	Cause         error
	Trigger       receiverAdmissionTrigger
	TerminalOwner receiverAdmissionTerminalOwner
}

type relayContentAdmission struct {
	relay receiverContentSuspension
	exec  receiverAdmissionExecution

	done         chan struct{}
	decisions    chan receiverAdmissionDecision
	decisionDone chan struct{}
	closeOnce    sync.Once

	mu             sync.Mutex
	state          receiverAdmissionState
	resumeError    error
	authority      *receiverAdmissionAuthority
	terminalOwner  receiverAdmissionTerminalOwner
	nextGeneration uint64
}

func newReceiverContentAdmissionWithExecution(
	mode receiverRelayContentMode,
	relay receiverContentSuspension,
	execution receiverAdmissionExecution,
) (receiverContentAdmission, error) {
	switch mode {
	case receiverRelayContentImmediate:
		return newRelayContentAdmissionWithExecution(relay, execution)
	case receiverRelayContentProhibited:
		return newP2POnlyContentAdmission(relay)
	default:
		return nil, ErrInvalidReceiverAdmission
	}
}

func newRelayContentAdmission(
	relay receiverContentSuspension,
) (*relayContentAdmission, error) {
	return newRelayContentAdmissionWithExecution(
		relay,
		receiverAdmissionExecution{},
	)
}

func newRelayContentAdmissionWithExecution(
	relay receiverContentSuspension,
	execution receiverAdmissionExecution,
) (*relayContentAdmission, error) {
	if relay == nil {
		return nil, ErrInvalidReceiverAdmission
	}
	return &relayContentAdmission{
		relay: relay, exec: execution, done: make(chan struct{}),
		decisions: make(chan receiverAdmissionDecision, 1), decisionDone: make(chan struct{}),
		terminalOwner: receiverAdmissionTerminalNone,
	}, nil
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
	if admission.exec.onClaim != nil {
		admission.exec.onClaim(authority.trigger)
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
		case receiverAdmissionExecuting:
			// A claimed external call cannot be canceled safely. Close revokes all
			// future authority; Wait is the exact completion barrier for this one.
		case receiverAdmissionDecided:
			joinableWorkerDone = admission.authority.workerDone
		case receiverAdmissionRevoked:
		}
		admission.mu.Unlock()
		close(admission.done)
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

type p2pOnlyContentAdmission struct {
	// Retaining the exact suspension documents ownership of the permanent hold.
	// Releasing it would violate the user-visible promise even after a reconnect.
	relayHold receiverContentSuspension

	finishOnce sync.Once
	finished   chan struct{}
	decisions  chan receiverAdmissionDecision

	mu    sync.Mutex
	cause error
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
	return admission, nil
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
		admission.stop()
		return nil
	case receiverPeerRuntimeTerminal:
		admission.stop()
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

func (admission *p2pOnlyContentAdmission) stop() {
	admission.finishOnce.Do(func() {
		close(admission.decisions)
		close(admission.finished)
	})
}

func (admission *p2pOnlyContentAdmission) Decision() <-chan receiverAdmissionDecision {
	return admission.decisions
}

func (admission *p2pOnlyContentAdmission) Close() {
	if admission != nil {
		admission.stop()
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
