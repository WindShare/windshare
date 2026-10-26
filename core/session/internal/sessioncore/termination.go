package sessioncore

import (
	"sync"
	"sync/atomic"

	"github.com/windshare/windshare/core/diagnosticerror"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type TerminationCause uint8

const (
	TerminationGracefulStop TerminationCause = iota + 1
	TerminationForcedClose
	TerminationPeerTerminal
	TerminationPathsExhausted
	TerminationFailed
)

func (cause TerminationCause) valid() bool {
	switch cause {
	case TerminationGracefulStop, TerminationForcedClose, TerminationPeerTerminal,
		TerminationPathsExhausted, TerminationFailed:
		return true
	default:
		return false
	}
}

// Termination records the winning lifecycle cause independently of role-specific
// diagnostics. Later send or cleanup failures cannot rewrite this root.
type Termination struct {
	ProtocolSessionID protocolsession.ProtocolSessionID
	Cause             TerminationCause
	Failure           diagnosticerror.Snapshot
}

type TerminationObserver interface {
	ObserveTermination(Termination)
}

type TerminationObserverFunc func(Termination)

func (observe TerminationObserverFunc) ObserveTermination(event Termination) { observe(event) }

// TerminationClaim retains the winner across an owner's lock boundary. Publishing
// is idempotent and bound to the originating runtime; a zero claim does nothing.
type TerminationClaim struct {
	runtime *Runtime
	cause   TerminationCause
	failure diagnosticerror.Snapshot
}

func (claim TerminationClaim) Publish() {
	runtime := claim.runtime
	if runtime == nil || !runtime.termination.published.CompareAndSwap(false, true) ||
		runtime.terminationObserver == nil {
		return
	}
	event := Termination{ProtocolSessionID: runtime.sessionID, Cause: claim.cause, Failure: claim.failure}
	// Observers run after state transitions and outside owner locks, so diagnostic
	// panics and reentrant stop requests cannot change lifecycle authority.
	func() {
		defer func() { _ = recover() }()
		runtime.terminationObserver.ObserveTermination(event)
	}()
}

type runtimeTerminationArbiter struct {
	mu        sync.Mutex
	claimed   bool
	cause     TerminationCause
	published atomic.Bool
}

func (runtime *Runtime) claimTermination(cause TerminationCause) TerminationClaim {
	if runtime == nil || !cause.valid() {
		return TerminationClaim{}
	}
	arbiter := &runtime.termination
	arbiter.mu.Lock()
	defer arbiter.mu.Unlock()
	if arbiter.claimed {
		return TerminationClaim{}
	}
	arbiter.claimed = true
	arbiter.cause = cause
	return TerminationClaim{runtime: runtime, cause: cause}
}

func (runtime *Runtime) cancelContext() {
	if runtime != nil && runtime.cancelLifecycle != nil {
		runtime.cancelLifecycle()
	}
}

func (runtime *Runtime) terminate(cause TerminationCause) {
	if runtime == nil {
		return
	}
	if cause == TerminationFailed {
		runtime.terminateWithFailure(cause, runtime.Err(), runtimeFailureSourceRuntime)
		return
	}
	claim := runtime.claimTermination(cause)
	runtime.cancelContext()
	claim.Publish()
}

func (runtime *Runtime) terminateRuntimeFailed(err error) {
	runtime.terminateWithFailure(TerminationFailed, err, runtimeFailureSourceRuntime)
}

func (runtime *Runtime) terminateWithFailure(cause TerminationCause, err error, source FailureSource) {
	if runtime == nil {
		return
	}
	claim := runtime.claimTermination(cause)
	if err != nil {
		runtime.recordError(err)
	}
	// Error inspection and diagnostic consumers must never delay cancellation.
	runtime.cancelContext()
	if claim.runtime != nil && runtime.terminationObserver != nil {
		claim.failure = diagnosticerror.Capture(err, string(source))
	}
	claim.Publish()
}
