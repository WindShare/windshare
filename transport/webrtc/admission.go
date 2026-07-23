package webrtc

import (
	"context"

	"github.com/windshare/windshare/core/framechannel"
)

type sendOperation uint8

const (
	sendOrdinary sendOperation = iota
	sendTerminal
)

type sendAdmissionState uint8

const (
	sendAdmissionPending sendAdmissionState = iota
	sendAdmissionAccepted
	sendAdmissionRefused
)

// sendAdmission gives cancellation and channel retirement the same serialized
// decision point as physical send ownership. Without a durable record, a later
// lifecycle observation can rewrite which event actually won.
type sendAdmission struct {
	id        uint64
	operation sendOperation
	ctx       context.Context
	done      chan struct{}

	// The lifecycle mutex protects every field below.
	state            sendAdmissionState
	err              error
	stopCancellation func() bool
}

func (l *channelLifecycle) beginSendAdmission(ctx context.Context, operation sendOperation) *sendAdmission {
	admission := &sendAdmission{
		operation: operation,
		ctx:       ctx,
		done:      make(chan struct{}),
		state:     sendAdmissionPending,
	}

	l.mu.Lock()
	l.nextSendID++
	admission.id = l.nextSendID
	if err := l.preAdmissionErrorLocked(ctx); err != nil {
		l.resolveSendAdmissionLocked(admission, sendAdmissionRefused, err)
		l.mu.Unlock()
		return admission
	}
	l.pendingSends[admission] = struct{}{}
	admission.stopCancellation = context.AfterFunc(ctx, func() {
		l.cancelSendAdmission(admission)
	})
	l.mu.Unlock()
	return admission
}

func (l *channelLifecycle) cancelSendAdmission(admission *sendAdmission) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if admission.state != sendAdmissionPending {
		return
	}
	err := admission.ctx.Err()
	if err == nil {
		return
	}
	l.resolveSendAdmissionLocked(admission, sendAdmissionRefused, framechannel.RejectSend(err))
}

func (l *channelLifecycle) sendAdmissionError(admission *sendAdmission) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if admission.state == sendAdmissionPending {
		if err := l.preAdmissionErrorLocked(admission.ctx); err != nil {
			l.resolveSendAdmissionLocked(admission, sendAdmissionRefused, err)
		}
	}
	if admission.state == sendAdmissionRefused {
		return admission.err
	}
	return nil
}

func (l *channelLifecycle) admitLocalTerminal(admission *sendAdmission) error {
	l.mu.Lock()
	if admission.state == sendAdmissionPending {
		if err := l.preAdmissionErrorLocked(admission.ctx); err != nil {
			l.resolveSendAdmissionLocked(admission, sendAdmissionRefused, err)
		} else {
			l.terminal = terminalLocalPending
			l.resolveSendAdmissionLocked(admission, sendAdmissionAccepted, nil)
			// Terminal ownership is a natural retirement boundary for every send
			// that was still waiting; already-completed cancellation wins first.
			l.resolvePendingSendsLocked(framechannel.RetireSend(ErrChannelClosed))
			close(l.localTerminalAdmitted)
		}
	}
	err := admission.err
	accepted := admission.state == sendAdmissionAccepted
	l.mu.Unlock()
	if accepted {
		l.signalStateChange()
		return nil
	}
	return err
}

func (l *channelLifecycle) transmitSendAdmission(
	admission *sendAdmission,
	send func() error,
) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if admission.state == sendAdmissionPending {
		if err := l.preAdmissionErrorLocked(admission.ctx); err != nil {
			l.resolveSendAdmissionLocked(admission, sendAdmissionRefused, err)
		} else {
			// Holding the lifecycle lock across the provider call makes invocation
			// the irreversible ownership transition relative to every close callback.
			err := send()
			l.resolveSendAdmissionLocked(admission, sendAdmissionAccepted, nil)
			if err != nil {
				l.traceAcceptedTransportFailureLocked(admission)
			}
			return true, err
		}
	}
	if admission.state == sendAdmissionRefused {
		return false, admission.err
	}
	return false, nil
}

func (l *channelLifecycle) preAdmissionErrorLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return framechannel.RejectSend(err)
	}
	if l.state == framechannel.Connecting {
		return framechannel.RejectSend(ErrChannelNotOpen)
	}
	if l.state == framechannel.Closed {
		err := l.closedErrorLocked()
		if l.reason == nil {
			return framechannel.RetireSend(err)
		}
		return framechannel.RejectSend(err)
	}
	if l.terminationPending {
		return framechannel.RejectSend(l.pendingTerminationErrorLocked())
	}
	if l.terminal != terminalNone || l.remoteIntentSeen {
		return framechannel.RetireSend(ErrChannelClosed)
	}
	return nil
}

func (l *channelLifecycle) resolvePendingSendsLocked(fallback error) {
	for admission := range l.pendingSends {
		err := fallback
		// A cancellation that completed before this lifecycle transition owns
		// the refusal even if its AfterFunc has not yet been scheduled.
		if cancellation := admission.ctx.Err(); cancellation != nil {
			err = framechannel.RejectSend(cancellation)
		}
		l.resolveSendAdmissionLocked(admission, sendAdmissionRefused, err)
	}
}

func (l *channelLifecycle) resolveSendAdmissionLocked(
	admission *sendAdmission,
	state sendAdmissionState,
	err error,
) {
	if admission.state != sendAdmissionPending {
		return
	}
	delete(l.pendingSends, admission)
	admission.state = state
	admission.err = err
	if admission.stopCancellation != nil {
		admission.stopCancellation()
	}
	close(admission.done)
	l.traceSendResolutionLocked(admission)
}
