// Package relayv2 adapts the native v2 opaque relay route to core FrameChannel.
package relayv2

import (
	"bytes"
	"context"
	"errors"
	"sync"

	"github.com/windshare/windshare/core/framechannel"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

const (
	channelReceiveFrames = 256
	channelSendFrames    = 64
	connectionSendFrames = 1_024
)

var (
	ErrClosed          = errors.New("relay v2 transport: connection is closed")
	ErrProtocol        = errors.New("relay v2 transport: protocol violation")
	ErrSessionRetired  = errors.New("relay v2 transport: relay session is retired")
	ErrIngressOverflow = errors.New("relay v2 transport: session receive queue is full")
	ErrEgressOverflow  = errors.New("relay v2 transport: session send queue is full")
	ErrFrameBounds     = errors.New("relay v2 transport: frame size is outside bounds")
)

type linkLifecycle uint8

const (
	linkOpen linkLifecycle = iota
	linkRetiring
	linkClosed
)

type retirementRecord struct {
	natural     bool
	source      LifecycleRetirementSource
	cause       error
	operationID uint64
}

func (record retirementRecord) sendError() error {
	return errors.Join(ErrClosed, record.cause)
}

type terminalReservation struct {
	operationID uint64
	done        chan struct{}
	admitted    bool
}

type retirementTransition struct {
	applied     bool
	deferred    bool
	record      retirementRecord
	consequence retirementRecord
	terminal    bool
}

func (l *link) requestChannelRetirement(
	channel *Channel,
	proposed retirementRecord,
) (<-chan struct{}, retirementTransition) {
	l.lockChannel(channel)
	if channel.state == framechannel.Closed {
		var done <-chan struct{}
		if channel.terminal != nil {
			done = channel.terminal.done
		}
		l.unlockChannel(channel)
		return done, retirementTransition{}
	}
	record := l.authoritativeRetirementLocked(channel, proposed)
	if proposed.natural && record.natural && channel.terminal != nil {
		if channel.pendingNatural == nil {
			pending := record
			channel.pendingNatural = &pending
		}
		done := channel.terminal.done
		transition := retirementTransition{
			deferred: true, record: *channel.pendingNatural,
			consequence: *channel.pendingNatural, terminal: true,
		}
		l.unlockChannel(channel)
		return done, transition
	}
	consequence := proposed
	if proposed.natural && record.operationID != proposed.operationID {
		consequence = record
	}
	transition := l.closeChannelLocked(channel, record, consequence)
	var done <-chan struct{}
	if channel.terminal != nil {
		done = channel.terminal.done
		transition.terminal = true
	}
	l.unlockChannel(channel)
	return done, transition
}

func (l *link) authoritativeRetirementLocked(
	channel *Channel,
	proposed retirementRecord,
) retirementRecord {
	if channel.pendingNatural != nil {
		return *channel.pendingNatural
	}
	if l.retirement != nil {
		return *l.retirement
	}
	return proposed
}

func (l *link) closeChannelLocked(
	channel *Channel,
	record retirementRecord,
	consequence retirementRecord,
) retirementTransition {
	if channel.state == framechannel.Closed {
		return retirementTransition{}
	}
	channel.state = framechannel.Closed
	stored := record
	channel.retirement = &stored
	channel.pendingNatural = nil
	close(channel.recv)
	if l.channels[channel.id] == channel {
		delete(l.channels, channel.id)
	}
	l.drainQueueLocked(channel.id, consequence.sendError())
	return retirementTransition{
		applied: true, record: record, consequence: consequence,
		terminal: channel.terminal != nil,
	}
}

func (l *link) releaseTerminalLocked(
	channel *Channel,
	reservation *terminalReservation,
) retirementTransition {
	if reservation == nil || channel.terminal != reservation {
		return retirementTransition{}
	}
	channel.terminal = nil
	close(reservation.done)
	if channel.state != framechannel.Open || channel.pendingNatural == nil {
		return retirementTransition{}
	}
	record := *channel.pendingNatural
	return l.closeChannelLocked(channel, record, record)
}

func (l *link) settleTerminal(
	channel *Channel,
	reservation *terminalReservation,
	sendErr error,
) retirementTransition {
	l.lockChannel(channel)
	if channel.terminal != reservation {
		l.unlockChannel(channel)
		return retirementTransition{}
	}
	channel.terminal = nil
	close(reservation.done)
	if channel.state == framechannel.Closed {
		l.unlockChannel(channel)
		return retirementTransition{}
	}
	record := retirementRecord{
		natural: sendErr == nil, source: LifecycleRetirementTerminal,
		cause: sendErr, operationID: reservation.operationID,
	}
	authoritative := l.authoritativeRetirementLocked(channel, record)
	consequence := record
	if record.natural && authoritative.operationID != record.operationID {
		consequence = authoritative
	}
	transition := l.closeChannelLocked(channel, authoritative, consequence)
	l.unlockChannel(channel)
	return transition
}

type Channel struct {
	id   v2.RelaySessionID
	link *link
	recv chan framechannel.Frame

	mu             sync.Mutex
	state          framechannel.ChannelState
	retirement     *retirementRecord
	pendingNatural *retirementRecord
	terminal       *terminalReservation
}

func newChannel(id v2.RelaySessionID, link *link) *Channel {
	return &Channel{id: id, link: link, recv: make(chan framechannel.Frame, channelReceiveFrames), state: framechannel.Open}
}

func (c *Channel) RelaySessionID() v2.RelaySessionID { return c.id }

func (c *Channel) Send(ctx context.Context, frame framechannel.Frame) error {
	request, err := c.prepareSend(ctx, frame, false)
	if err != nil {
		return err
	}
	return c.link.enqueue(ctx, c, request)
}

func (c *Channel) SendTerminal(ctx context.Context, frame framechannel.Frame) error {
	request, err := c.prepareSend(ctx, frame, true)
	if err != nil {
		return err
	}
	reservation, err := c.reserveTerminal(ctx, request)
	if err != nil {
		return err
	}
	err = c.link.enqueueTerminal(ctx, c, request, reservation)
	if framechannel.SendDispositionOf(err) == framechannel.SendAccepted {
		transition := c.link.settleTerminal(c, reservation, err)
		c.link.trace(LifecycleTrace{
			RelaySessionID: c.id, OperationID: request.operationID,
			Stage: LifecycleTerminalSettled, Terminal: true,
			Disposition: framechannel.SendAccepted, Cause: lifecycleCause(err),
		})
		c.link.traceTransition(c, transition)
	}
	return err
}

func (c *Channel) prepareSend(
	ctx context.Context,
	frame framechannel.Frame,
	terminal bool,
) (*sendRequest, error) {
	operationID := c.link.nextOperationID()
	reject := func(cause error) (*sendRequest, error) {
		err := framechannel.RejectSend(cause)
		c.link.trace(LifecycleTrace{
			RelaySessionID: c.id, OperationID: operationID,
			Stage: LifecycleSendRejected, Terminal: terminal,
			Disposition: framechannel.SendRejected, Cause: lifecycleCause(err),
		})
		return nil, err
	}
	if len(frame) == 0 || len(frame) > framechannel.MaxFrameSize {
		return reject(ErrFrameBounds)
	}
	if err := ctx.Err(); err != nil {
		return reject(err)
	}
	route, err := (v2.OpaqueRoute{RelaySessionID: c.id, Ciphertext: bytes.Clone(frame)}).MarshalBinary()
	if err != nil {
		return reject(err)
	}
	return &sendRequest{
		data: route, receipt: make(chan error, 1),
		operationID: operationID, terminal: terminal,
	}, nil
}

func (c *Channel) reserveTerminal(
	ctx context.Context,
	request *sendRequest,
) (*terminalReservation, error) {
	c.link.lockChannel(c)
	var err error
	switch {
	case ctx.Err() != nil:
		err = framechannel.RejectSend(ctx.Err())
	case c.state != framechannel.Open:
		err = unavailableSendError(c.retirement)
	case c.link.lifecycle != linkOpen:
		err = unavailableSendError(c.link.retirement)
	case c.link.channels[c.id] != c:
		err = unavailableSendError(c.link.retirement)
	case c.terminal != nil:
		err = framechannel.RejectSend(ErrClosed)
	}
	if err != nil {
		c.link.unlockChannel(c)
		c.link.trace(LifecycleTrace{
			RelaySessionID: c.id, OperationID: request.operationID,
			Stage: LifecycleSendRejected, Terminal: true,
			Disposition: framechannel.SendDispositionOf(err), Cause: lifecycleCause(err),
		})
		return nil, err
	}
	reservation := &terminalReservation{
		operationID: request.operationID, done: make(chan struct{}),
	}
	c.terminal = reservation
	c.link.unlockChannel(c)
	c.link.trace(LifecycleTrace{
		RelaySessionID: c.id, OperationID: request.operationID,
		Stage: LifecycleTerminalReserved, Terminal: true,
	})
	return reservation, nil
}

func unavailableSendError(record *retirementRecord) error {
	if record == nil {
		return framechannel.RejectSend(ErrClosed)
	}
	if record.natural {
		return framechannel.RetireSend(record.sendError())
	}
	return framechannel.RejectSend(record.sendError())
}

func (c *Channel) Recv() <-chan framechannel.Frame { return c.recv }

func (c *Channel) State() framechannel.ChannelState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *Channel) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.retirement == nil {
		return nil
	}
	return c.retirement.cause
}

func (c *Channel) Close() error {
	record := retirementRecord{
		natural: true, source: LifecycleRetirementLocalClose,
		operationID: c.link.nextOperationID(),
	}
	done, transition := c.link.requestChannelRetirement(c, record)
	c.link.traceTransition(c, transition)
	if done != nil {
		<-done
	}
	return nil
}

func (c *Channel) deliver(frame []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != framechannel.Open {
		return false
	}
	select {
	case c.recv <- bytes.Clone(frame):
		return true
	default:
		return false
	}
}
