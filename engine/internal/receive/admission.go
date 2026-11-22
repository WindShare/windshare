package receive

import (
	"errors"
	"sync"
)

var (
	ErrInvalidReceiverAdmission   = errors.New("receiver content admission signal is invalid")
	errReceiverP2PPathUnavailable = errors.New("p2p-only download requires an active direct peer path")
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

type receiverAdmissionTrigger string

const (
	receiverAdmissionTriggerAuto         receiverAdmissionTrigger = "auto_policy"
	receiverAdmissionTriggerRelayOnly    receiverAdmissionTrigger = "relay_only_policy"
	receiverAdmissionTriggerPeerFailed   receiverAdmissionTrigger = "peer_failed"
	receiverAdmissionTriggerPeerDetached receiverAdmissionTrigger = "peer_detached"
)

type receiverAdmissionTerminalOwner string

const (
	receiverAdmissionTerminalNone           receiverAdmissionTerminalOwner = "none"
	receiverAdmissionTerminalResumeFailed   receiverAdmissionTerminalOwner = "resume_failure"
	receiverAdmissionTerminalP2PUnavailable receiverAdmissionTerminalOwner = "p2p_unavailable"
)

type receiverAdmissionDecision struct {
	Cause         error
	Trigger       receiverAdmissionTrigger
	TerminalOwner receiverAdmissionTerminalOwner
}

type receiverAdmissionHooks struct {
	// claimGate keeps relay content suspended until destination authority and
	// the transfer job exist; onClaim publishes ownership before Resume exposes
	// the relay lane to content requests.
	claimGate  <-chan struct{}
	onClaim    func(receiverAdmissionTrigger)
	onDecision func(receiverAdmissionDecision)
}

// receiverContentAdmission releases the suspended relay lane once content
// authority is ready (auto and relay-only), or terminates the session on peer
// loss (p2p-only, where LaneSet's ContentRouteDirectOnly already bars relay).
type receiverContentAdmission struct {
	relay        receiverContentSuspension
	relayTrigger receiverAdmissionTrigger
	hooks        receiverAdmissionHooks

	done      chan struct{}
	closeOnce sync.Once

	mu         sync.Mutex
	started    bool
	revoked    bool
	err        error
	workerDone chan struct{}
}

func newReceiverContentAdmission(
	plan receiverConnectivityPlan,
	relay receiverContentSuspension,
	hooks receiverAdmissionHooks,
) (*receiverContentAdmission, error) {
	if !plan.valid() {
		return nil, ErrInvalidReceiverAdmission
	}
	if plan.allowsRelay() && relay == nil {
		return nil, ErrInvalidReceiverAdmission
	}
	return &receiverContentAdmission{
		relay:        relay,
		relayTrigger: plan.relayTrigger,
		hooks:        hooks,
		done:         make(chan struct{}),
	}, nil
}

func (a *receiverContentAdmission) AdmitRelay() error {
	if a.relayTrigger == "" {
		return ErrInvalidReceiverAdmission
	}
	a.mu.Lock()
	if a.revoked || a.started {
		a.mu.Unlock()
		return nil
	}
	a.started = true
	if gateReady(a.hooks.claimGate) {
		a.mu.Unlock()
		a.resumeRelay()
		return nil
	}
	workerDone := make(chan struct{})
	a.workerDone = workerDone
	a.mu.Unlock()
	go func() {
		defer close(workerDone)
		select {
		case <-a.hooks.claimGate:
		case <-a.done:
			return
		}
		a.resumeRelay()
	}()
	return nil
}

func gateReady(gate <-chan struct{}) bool {
	if gate == nil {
		return true
	}
	select {
	case <-gate:
		return true
	default:
		return false
	}
}

func (a *receiverContentAdmission) resumeRelay() {
	a.mu.Lock()
	if a.revoked {
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()
	if a.hooks.onClaim != nil {
		a.hooks.onClaim(a.relayTrigger)
	}
	err := a.relay.Resume()
	a.mu.Lock()
	if a.revoked {
		if err != nil && a.err == nil {
			a.err = err
		}
		a.mu.Unlock()
		return
	}
	a.err = err
	owner := receiverAdmissionTerminalNone
	if err != nil {
		owner = receiverAdmissionTerminalResumeFailed
	}
	a.mu.Unlock()
	if a.hooks.onDecision != nil {
		a.hooks.onDecision(receiverAdmissionDecision{
			Cause:         err,
			Trigger:       a.relayTrigger,
			TerminalOwner: owner,
		})
	}
}

func (a *receiverContentAdmission) ObservePeer(signal receiverPeerSignal) error {
	switch signal {
	case receiverPeerReady:
		return nil
	case receiverPeerFailed:
		if a.relayTrigger == "" {
			a.failP2POnly(receiverAdmissionTriggerPeerFailed)
		}
		return nil
	case receiverPeerDetached:
		if a.relayTrigger == "" {
			a.failP2POnly(receiverAdmissionTriggerPeerDetached)
		}
		return nil
	case receiverPeerSessionFatal, receiverPeerRuntimeTerminal:
		a.Close()
		return nil
	default:
		return ErrInvalidReceiverAdmission
	}
}

func (a *receiverContentAdmission) failP2POnly(trigger receiverAdmissionTrigger) {
	a.mu.Lock()
	if a.revoked {
		a.mu.Unlock()
		return
	}
	a.revoked = true
	a.err = errReceiverP2PPathUnavailable
	a.mu.Unlock()
	a.closeOnce.Do(func() { close(a.done) })
	if a.hooks.onDecision != nil {
		a.hooks.onDecision(receiverAdmissionDecision{
			Cause:         errReceiverP2PPathUnavailable,
			Trigger:       trigger,
			TerminalOwner: receiverAdmissionTerminalP2PUnavailable,
		})
	}
}

func (a *receiverContentAdmission) Close() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.revoked = true
	a.mu.Unlock()
	a.closeOnce.Do(func() { close(a.done) })
}

func (a *receiverContentAdmission) Wait() {
	if a == nil {
		return
	}
	a.mu.Lock()
	workerDone := a.workerDone
	a.mu.Unlock()
	if workerDone != nil {
		<-workerDone
	}
}

func (a *receiverContentAdmission) Err() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.err
}
