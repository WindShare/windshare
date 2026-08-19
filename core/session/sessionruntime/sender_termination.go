package sessionruntime

import (
	"sync"

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
