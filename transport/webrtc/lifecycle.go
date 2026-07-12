package webrtc

import (
	"errors"
	"fmt"
	"sync"

	"github.com/windshare/windshare/core/session"
)

type terminalState uint8

const (
	terminalNone terminalState = iota
	terminalLocalPending
	terminalRemotePending
)

type callbackAdmissionState uint8

const (
	callbackAdmissionClosed callbackAdmissionState = iota
	callbackAdmissionConnecting
	callbackAdmissionOpen
)

type inboundBinaryMode uint8

const (
	inboundBinaryStopped inboundBinaryMode = iota
	inboundBinaryOrdinary
	inboundBinaryRemoteTerminal
	inboundBinaryDiscard
)

type sendLifecycleEffect uint8

const (
	sendWithoutLifecycleEffect sendLifecycleEffect = iota
	sendMarksLocalTerminal
	sendMarksRemoteTerminalAck
)

// channelLifecycle is the single authority for logical channel transitions.
// Transport code can ask semantic questions or request complete transitions, but
// cannot observe a lock and then act later; that boundary preserves terminal and
// Close linearization while keeping Pion's physical close outside the state lock.
type channelLifecycle struct {
	mu       sync.Mutex
	state    session.ChannelState
	terminal terminalState

	remoteIntentSeen        bool
	remoteTerminalPublished bool
	remoteTerminalAckSent   bool
	localTerminalSent       bool
	localTerminalAck        bool
	terminationPending      bool
	terminationBase         error
	reason                  error

	opened                chan struct{}
	done                  chan struct{}
	stop                  chan struct{}
	terminalAck           chan struct{}
	stateWake             chan struct{}
	localTerminalAdmitted chan struct{}
}

func newChannelLifecycle() *channelLifecycle {
	return &channelLifecycle{
		state:                 session.Connecting,
		opened:                make(chan struct{}),
		done:                  make(chan struct{}),
		stop:                  make(chan struct{}),
		terminalAck:           make(chan struct{}),
		stateWake:             make(chan struct{}, 1),
		localTerminalAdmitted: make(chan struct{}),
	}
}

func (l *channelLifecycle) openedSignal() <-chan struct{} { return l.opened }

func (l *channelLifecycle) doneSignal() <-chan struct{} { return l.done }

func (l *channelLifecycle) stopSignal() <-chan struct{} { return l.stop }

func (l *channelLifecycle) terminalAckSignal() <-chan struct{} { return l.terminalAck }

func (l *channelLifecycle) stateWakeSignal() <-chan struct{} { return l.stateWake }

func (l *channelLifecycle) localTerminalSignal() <-chan struct{} {
	return l.localTerminalAdmitted
}

func (l *channelLifecycle) channelState() session.ChannelState {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state
}

func (l *channelLifecycle) channelError() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reason
}

func (l *channelLifecycle) callbackAdmission() callbackAdmissionState {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == session.Closed || l.terminationPending {
		return callbackAdmissionClosed
	}
	if l.state == session.Connecting {
		return callbackAdmissionConnecting
	}
	return callbackAdmissionOpen
}

func (l *channelLifecycle) publishOpen() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == session.Connecting && !l.terminationPending {
		l.state = session.Open
		close(l.opened)
	}
}

func (l *channelLifecycle) admitLocalTerminal() error {
	l.mu.Lock()
	if l.state == session.Connecting {
		l.mu.Unlock()
		return ErrChannelNotOpen
	}
	if l.state == session.Closed {
		err := l.closedErrorLocked()
		l.mu.Unlock()
		return err
	}
	if l.terminationPending {
		err := l.pendingTerminationErrorLocked()
		l.mu.Unlock()
		return err
	}
	if l.terminal != terminalNone || l.remoteIntentSeen {
		l.mu.Unlock()
		return ErrChannelClosed
	}
	l.terminal = terminalLocalPending
	close(l.localTerminalAdmitted)
	l.mu.Unlock()
	l.signalStateChange()
	return nil
}

func (l *channelLifecycle) requireSendState(required terminalState) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.requireSendStateLocked(required)
}

func (l *channelLifecycle) transmit(
	required terminalState,
	send func() error,
) (bool, error) {
	return l.transmitWithEffect(required, sendWithoutLifecycleEffect, send)
}

func (l *channelLifecycle) transmitLocalTerminal(send func() error) (bool, error) {
	return l.transmitWithEffect(terminalLocalPending, sendMarksLocalTerminal, send)
}

func (l *channelLifecycle) transmitRemoteTerminalAck(send func() error) (bool, error) {
	return l.transmitWithEffect(terminalRemotePending, sendMarksRemoteTerminalAck, send)
}

// transmitWithEffect makes the final state check and raw send one decision.
// Callback transitions use the same mutex, so terminal or close cannot interleave.
func (l *channelLifecycle) transmitWithEffect(
	required terminalState,
	effect sendLifecycleEffect,
	send func() error,
) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.requireSendStateLocked(required); err != nil {
		return false, err
	}
	if err := send(); err != nil {
		return true, err
	}
	switch effect {
	case sendMarksLocalTerminal:
		l.localTerminalSent = true
	case sendMarksRemoteTerminalAck:
		l.remoteTerminalAckSent = true
	}
	return true, nil
}

func (l *channelLifecycle) closeIfIdle() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	closeNow := l.state != session.Closed &&
		l.terminal == terminalNone &&
		!l.remoteIntentSeen
	if !closeNow {
		return false
	}
	var reason error
	if l.terminationPending {
		reason = l.classifyTerminationLocked(l.terminationBase)
	}
	l.transitionClosedLocked(reason)
	return true
}

func (l *channelLifecycle) terminalOutcome() (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.localTerminalAck, l.reason
}

func (l *channelLifecycle) reserveRemoteIntent() bool {
	l.mu.Lock()
	valid := l.state == session.Open &&
		!l.terminationPending &&
		l.terminal == terminalNone &&
		!l.remoteIntentSeen
	if valid {
		l.remoteIntentSeen = true
	}
	l.mu.Unlock()
	if valid {
		l.signalStateChange()
	}
	return valid
}

func (l *channelLifecycle) acceptRemoteIntent() bool {
	l.mu.Lock()
	valid := l.state == session.Open &&
		l.terminal == terminalNone &&
		l.remoteIntentSeen
	if valid {
		l.terminal = terminalRemotePending
	}
	l.mu.Unlock()
	if valid {
		l.signalStateChange()
	}
	return valid
}

// acknowledgeLocalTerminal owns the whole logical success transition. The
// supplied hook starts physical closure before the waiter can observe ACK success,
// while the transport call itself remains outside the lifecycle mutex.
func (l *channelLifecycle) acknowledgeLocalTerminal(requestPhysicalClose func()) bool {
	l.mu.Lock()
	valid := l.state == session.Open &&
		!l.terminationPending &&
		l.terminal == terminalLocalPending &&
		l.localTerminalSent &&
		!l.localTerminalAck
	if valid {
		l.localTerminalAck = true
		l.transitionClosedLocked(nil)
	}
	l.mu.Unlock()
	if !valid {
		return false
	}
	requestPhysicalClose()
	close(l.terminalAck)
	return true
}

func (l *channelLifecycle) beginTermination(reason error) bool {
	l.mu.Lock()
	if l.state == session.Closed || l.terminationPending {
		l.mu.Unlock()
		return false
	}
	l.terminationPending = true
	l.terminationBase = reason
	l.mu.Unlock()
	l.signalStateChange()
	return true
}

func (l *channelLifecycle) classifyTermination(base error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.classifyTerminationLocked(base)
}

func (l *channelLifecycle) inboundBinaryMode() inboundBinaryMode {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state != session.Open {
		return inboundBinaryStopped
	}
	switch l.terminal {
	case terminalNone:
		return inboundBinaryOrdinary
	case terminalRemotePending:
		return inboundBinaryRemoteTerminal
	case terminalLocalPending:
		return inboundBinaryDiscard
	default:
		return inboundBinaryStopped
	}
}

func (l *channelLifecycle) markRemoteTerminalPublished() {
	l.mu.Lock()
	l.remoteTerminalPublished = true
	l.mu.Unlock()
}

func (l *channelLifecycle) finish(reason error) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == session.Closed {
		return false
	}
	if reason != nil &&
		l.terminal == terminalLocalPending &&
		!l.localTerminalAck &&
		!errors.Is(reason, ErrTerminalNotAcknowledged) {
		reason = errors.Join(ErrTerminalNotAcknowledged, reason)
	}
	l.transitionClosedLocked(reason)
	return true
}

func (l *channelLifecycle) complete() { close(l.done) }

func (l *channelLifecycle) requireSendStateLocked(required terminalState) error {
	if l.state == session.Connecting {
		return ErrChannelNotOpen
	}
	if l.state == session.Closed {
		return l.closedErrorLocked()
	}
	if l.terminationPending {
		return l.pendingTerminationErrorLocked()
	}
	if l.terminal != required || (l.remoteIntentSeen && required != terminalRemotePending) {
		return ErrChannelClosed
	}
	return nil
}

func (l *channelLifecycle) closedError() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closedErrorLocked()
}

func (l *channelLifecycle) closedErrorLocked() error {
	if l.reason != nil {
		return l.reason
	}
	if l.terminationPending {
		return l.pendingTerminationErrorLocked()
	}
	return ErrChannelClosed
}

func (l *channelLifecycle) pendingTerminationErrorLocked() error {
	base := l.terminationBase
	if base == nil {
		base = ErrChannelClosed
	}
	// Remote terminal completeness is classified only when ordered termination
	// reaches the ingress owner; outbound waiters need only the immediate freeze.
	if l.terminal == terminalLocalPending && !l.localTerminalAck {
		return errors.Join(ErrTerminalNotAcknowledged, base)
	}
	return base
}

func (l *channelLifecycle) classifyTerminationLocked(base error) error {
	if base == nil {
		base = ErrChannelClosed
	}
	switch {
	case l.remoteTerminalAckSent:
		return nil
	case l.terminal == terminalLocalPending:
		if l.localTerminalAck {
			return nil
		}
		return errors.Join(ErrTerminalNotAcknowledged, base)
	case l.remoteTerminalPublished:
		return base
	case l.terminal == terminalRemotePending || l.remoteIntentSeen:
		return errors.Join(ErrPeerProtocol, fmt.Errorf("terminal intent had no final frame: %w", base))
	default:
		return base
	}
}

func (l *channelLifecycle) transitionClosedLocked(reason error) {
	l.state = session.Closed
	l.reason = reason
	close(l.stop)
}

func (l *channelLifecycle) signalStateChange() {
	select {
	case l.stateWake <- struct{}{}:
	default:
	}
}
