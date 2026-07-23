package relayv2

import (
	"context"

	"github.com/windshare/windshare/core/framechannel"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

type sendRequest struct {
	data        []byte
	receipt     chan error
	operationID uint64
	terminal    bool
}

type sendQueue struct{ requests []*sendRequest }

func (l *link) enqueue(ctx context.Context, channel *Channel, request *sendRequest) error {
	return l.enqueueWithAuthority(ctx, channel, request, nil)
}

func (l *link) enqueueTerminal(
	ctx context.Context,
	channel *Channel,
	request *sendRequest,
	reservation *terminalReservation,
) error {
	return l.enqueueWithAuthority(ctx, channel, request, reservation)
}

func (l *link) enqueueWithAuthority(
	ctx context.Context,
	channel *Channel,
	request *sendRequest,
	reservation *terminalReservation,
) error {
	admission := newContextAdmission(ctx)
	defer admission.abandon()

	l.lockChannel(channel)
	var rejection error
	switch {
	case ctx.Err() != nil:
		rejection = framechannel.RejectSend(ctx.Err())
	case channel.state != framechannel.Open:
		rejection = unavailableSendError(channel.retirement)
	case reservation == nil && channel.terminal != nil:
		rejection = framechannel.RejectSend(ErrClosed)
	case reservation != nil && channel.terminal != reservation:
		rejection = framechannel.RejectSend(ErrClosed)
	case l.lifecycle != linkOpen &&
		(reservation == nil || l.retirement == nil || !l.retirement.natural):
		rejection = unavailableSendError(l.retirement)
	case l.channels[channel.id] != channel:
		rejection = unavailableSendError(l.retirement)
	}
	queue := l.queues[channel.id]
	if rejection == nil && (lenQueue(queue) >= channelSendFrames || l.queued >= connectionSendFrames) {
		rejection = framechannel.RejectSend(ErrEgressOverflow)
	}
	if rejection == nil {
		if err := admission.commit(); err != nil {
			rejection = framechannel.RejectSend(err)
		}
	}
	if rejection != nil {
		transition := l.releaseTerminalLocked(channel, reservation)
		l.unlockChannel(channel)
		l.trace(LifecycleTrace{
			RelaySessionID: channel.id, OperationID: request.operationID,
			Stage: LifecycleSendRejected, Terminal: request.terminal,
			Disposition: framechannel.SendDispositionOf(rejection), Cause: lifecycleCause(rejection),
		})
		l.traceTransition(channel, transition)
		return rejection
	}
	if queue == nil {
		queue = &sendQueue{}
		l.queues[channel.id] = queue
		l.order = append(l.order, channel.id)
	}
	queue.requests = append(queue.requests, request)
	l.queued++
	if reservation != nil {
		reservation.admitted = true
	}
	l.unlockChannel(channel)

	l.trace(LifecycleTrace{
		RelaySessionID: channel.id, OperationID: request.operationID,
		Stage: LifecycleSendAdmitted, Terminal: request.terminal,
		Disposition: framechannel.SendAccepted,
	})
	select {
	case l.writeWake <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		if transition, rolledBack := l.rollbackQueued(channel, request, reservation); rolledBack {
			rejection := framechannel.RejectSend(ctx.Err())
			l.trace(LifecycleTrace{
				RelaySessionID: channel.id, OperationID: request.operationID,
				Stage: LifecycleSendRolledBack, Terminal: request.terminal,
				Disposition: framechannel.SendRejected, Cause: lifecycleCause(ctx.Err()),
			})
			l.traceTransition(channel, transition)
			return rejection
		}
		return ctx.Err()
	case err := <-request.receipt:
		return err
	}
}

func lenQueue(queue *sendQueue) int {
	if queue == nil {
		return 0
	}
	return len(queue.requests)
}

type contextAdmission struct {
	ctx       context.Context
	stop      func() bool
	committed bool
}

func newContextAdmission(ctx context.Context) *contextAdmission {
	return &contextAdmission{ctx: ctx, stop: context.AfterFunc(ctx, func() {})}
}

func (admission *contextAdmission) commit() error {
	// AfterFunc's stop race is the ownership boundary: cancellation that has
	// already completed cannot slip between the final check and queue insertion.
	if !admission.stop() {
		return context.Cause(admission.ctx)
	}
	admission.committed = true
	return nil
}

func (admission *contextAdmission) abandon() {
	if !admission.committed {
		admission.stop()
	}
}

func (l *link) lockChannel(channel *Channel) {
	// Every lifecycle decision uses this order so registration and queue state
	// cannot publish a different winner than the channel retirement record.
	l.lifecycleMu.Lock()
	channel.mu.Lock()
	l.channelMu.Lock()
	l.writeMu.Lock()
}

func (l *link) unlockChannel(channel *Channel) {
	l.writeMu.Unlock()
	l.channelMu.Unlock()
	channel.mu.Unlock()
	l.lifecycleMu.Unlock()
}

func (l *link) rollbackQueued(
	channel *Channel,
	request *sendRequest,
	reservation *terminalReservation,
) (retirementTransition, bool) {
	l.lockChannel(channel)
	queue := l.queues[channel.id]
	if queue == nil {
		l.unlockChannel(channel)
		return retirementTransition{}, false
	}
	for index, queued := range queue.requests {
		if queued != request {
			continue
		}
		copy(queue.requests[index:], queue.requests[index+1:])
		queue.requests[len(queue.requests)-1] = nil
		queue.requests = queue.requests[:len(queue.requests)-1]
		l.queued--
		transition := l.releaseTerminalLocked(channel, reservation)
		l.unlockChannel(channel)
		return transition, true
	}
	l.unlockChannel(channel)
	return retirementTransition{}, false
}

func (l *link) takeRequest() (*sendRequest, bool) {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	if len(l.order) == 0 {
		return nil, false
	}
	for range len(l.order) {
		if l.cursor >= len(l.order) {
			l.cursor = 0
		}
		id := l.order[l.cursor]
		l.cursor++
		queue := l.queues[id]
		if queue == nil || len(queue.requests) == 0 {
			continue
		}
		request := queue.requests[0]
		queue.requests[0] = nil
		queue.requests = queue.requests[1:]
		l.queued--
		return request, true
	}
	return nil, false
}

func (l *link) drainQueueLocked(id v2.RelaySessionID, failure error) {
	if queue := l.queues[id]; queue != nil {
		l.queued -= len(queue.requests)
		for _, request := range queue.requests {
			request.receipt <- failure
		}
		delete(l.queues, id)
	}
	for index, candidate := range l.order {
		if candidate != id {
			continue
		}
		l.order = append(l.order[:index], l.order[index+1:]...)
		if l.cursor > index {
			l.cursor--
		}
		if l.cursor >= len(l.order) {
			l.cursor = 0
		}
		break
	}
}
