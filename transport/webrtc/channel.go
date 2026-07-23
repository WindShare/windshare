package webrtc

import (
	"context"
	"errors"
	"fmt"
	"sync"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/core/framechannel"
)

const (
	receiveQueueFrames = 32
	inboundQueueEvents = 32
)

type dataChannel interface {
	Label() string
	Protocol() string
	Ordered() bool
	MaxPacketLifeTime() *uint16
	MaxRetransmits() *uint16
	Negotiated() bool
	ReadyState() pion.DataChannelState

	BufferedAmount() uint64
	SetBufferedAmountLowThreshold(uint64)
	OnBufferedAmountLow(func())

	OnOpen(func())
	OnClose(func())
	OnError(func(error))
	OnMessage(func(pion.DataChannelMessage))

	Send([]byte) error
	SendText(string) error
	Close() error
	maxMessageSize() uint32
}

type pionDataChannel struct {
	*pion.DataChannel
}

func (dc pionDataChannel) maxMessageSize() uint32 {
	transport := dc.Transport()
	if transport == nil {
		return 0
	}
	return transport.GetCapabilities().MaxMessageSize
}

type inboundEventKind uint8

const (
	inboundBinary inboundEventKind = iota
	inboundTerminalIntent
	inboundTermination
)

type inboundEvent struct {
	kind inboundEventKind
	data []byte
}

type channelRuntime struct {
	// Tests inject a gate to establish callback/worker schedules without sleeps.
	// Production leaves it nil so dispatch begins immediately.
	inboundGate              <-chan struct{}
	remoteTerminalFinishGate <-chan struct{}

	lifecycleTracer LifecycleTracer
}

// Channel is the sole owner of the callbacks installed on its Pion DataChannel.
// A wrapped DataChannel must not be sent through directly after construction.
type Channel struct {
	dc        dataChannel
	flow      flowControlProfile
	lifecycle *channelLifecycle

	callbackMu sync.Mutex
	recv       chan framechannel.Frame

	inbound                  chan inboundEvent
	inboundDone              chan struct{}
	flowWake                 chan struct{}
	sendTurn                 chan struct{}
	inboundGate              <-chan struct{}
	remoteTerminalFinishGate <-chan struct{}

	physicalOnce sync.Once
	physicalDone chan struct{}
	physicalMu   sync.Mutex
	physicalErr  error
	traces       *lifecycleTraceDispatcher
}

var _ framechannel.Channel = (*Channel)(nil)

func NewChannel(dc *pion.DataChannel) (*Channel, error) {
	return NewChannelWithOptions(dc, ChannelOptions{})
}

// NewChannelWithOptions preserves the same transport contract while exposing
// optional structured lifecycle decisions to the owning runtime.
func NewChannelWithOptions(dc *pion.DataChannel, options ChannelOptions) (*Channel, error) {
	if dc == nil {
		return nil, ErrNilDataChannel
	}
	return newChannelWithRuntime(pionDataChannel{DataChannel: dc}, defaultFlowControl, channelRuntime{
		lifecycleTracer: options.LifecycleTracer,
	})
}

func newChannel(dc dataChannel, flow flowControlProfile) (*Channel, error) {
	return newChannelWithRuntime(dc, flow, channelRuntime{})
}

func newChannelWithRuntime(dc dataChannel, flow flowControlProfile, runtime channelRuntime) (*Channel, error) {
	if dc == nil {
		return nil, ErrNilDataChannel
	}
	if err := validateFlowControl(flow); err != nil {
		return nil, err
	}
	if err := validateDataChannel(dc); err != nil {
		return nil, err
	}

	dispatcher := newLifecycleTraceDispatcher(runtime.lifecycleTracer)
	lifecycle := newChannelLifecycle()
	lifecycle.configureTrace(nextLifecycleChannelID.Add(1), dispatcher)
	c := &Channel{
		dc:                       dc,
		flow:                     flow,
		lifecycle:                lifecycle,
		recv:                     make(chan framechannel.Frame, receiveQueueFrames),
		inbound:                  make(chan inboundEvent, inboundQueueEvents),
		inboundDone:              make(chan struct{}),
		flowWake:                 make(chan struct{}, 1),
		sendTurn:                 make(chan struct{}, 1),
		inboundGate:              runtime.inboundGate,
		remoteTerminalFinishGate: runtime.remoteTerminalFinishGate,
		physicalDone:             make(chan struct{}),
		traces:                   dispatcher,
	}
	c.sendTurn <- struct{}{}

	go c.runInbound()
	go c.runFinalizer()

	// Flow callbacks must be authoritative before Opened can become observable.
	dc.SetBufferedAmountLowThreshold(flow.lowWaterBytes)
	dc.OnBufferedAmountLow(c.onBufferedAmountLow)
	dc.OnMessage(c.onMessage)
	dc.OnError(c.onError)
	dc.OnClose(c.onClose)
	dc.OnOpen(c.onOpen)
	switch dc.ReadyState() {
	case pion.DataChannelStateOpen:
		c.onOpen()
	case pion.DataChannelStateClosing, pion.DataChannelStateClosed:
		// Closure can race callback installation. Reconcile the observable state
		// so a channel cannot remain Connecting with leaked workers forever.
		c.onClose()
	}

	return c, nil
}

func (c *Channel) Opened() <-chan struct{} { return c.lifecycle.openedSignal() }

func (c *Channel) Done() <-chan struct{} { return c.lifecycle.doneSignal() }

func (c *Channel) Recv() <-chan framechannel.Frame { return c.recv }

func (c *Channel) State() framechannel.ChannelState {
	return c.lifecycle.channelState()
}

// Err is stable after Done closes. A normal Close or acknowledged terminal has
// no terminal error.
func (c *Channel) Err() error {
	return c.lifecycle.channelError()
}

func (c *Channel) Send(ctx context.Context, frame framechannel.Frame) error {
	if err := validateOutboundFrame(frame); err != nil {
		return framechannel.RejectSend(err)
	}
	admission := c.lifecycle.beginSendAdmission(ctx, sendOrdinary)
	if err := c.lifecycle.sendAdmissionError(admission); err != nil {
		return err
	}
	snapshot := append(framechannel.Frame(nil), frame...)
	if err := c.acquireSendAdmissionTurn(admission); err != nil {
		return err
	}
	defer c.releaseSendTurn()

	if err := c.waitForSendAdmissionCapacity(admission); err != nil {
		return err
	}
	attempted, err := c.lifecycle.transmitSendAdmission(
		admission,
		func() error { return c.dc.Send(snapshot) },
	)
	if err != nil {
		if !attempted {
			return err
		}
		failure := fmt.Errorf("%w: send binary frame: %w", ErrTransport, err)
		c.finish(failure)
		return failure
	}
	return nil
}

func (c *Channel) SendTerminal(ctx context.Context, frame framechannel.Frame) error {
	if err := validateOutboundFrame(frame); err != nil {
		return framechannel.RejectSend(err)
	}
	admission := c.lifecycle.beginSendAdmission(ctx, sendTerminal)
	if err := c.lifecycle.sendAdmissionError(admission); err != nil {
		return err
	}
	snapshot := append(framechannel.Frame(nil), frame...)
	if err := c.lifecycle.admitLocalTerminal(admission); err != nil {
		return err
	}

	if err := c.sendLocalTerminal(ctx, snapshot); err != nil {
		failure := errors.Join(ErrTerminalNotAcknowledged, err)
		c.finish(failure)
		return failure
	}

	select {
	case <-c.lifecycle.terminalAckSignal():
		return nil
	case <-ctx.Done():
		failure := errors.Join(ErrTerminalNotAcknowledged, ctx.Err())
		if !c.finish(failure) {
			acknowledged, _ := c.lifecycle.terminalOutcome()
			if acknowledged {
				return nil
			}
		}
		return failure
	case <-c.lifecycle.doneSignal():
		acknowledged, reason := c.lifecycle.terminalOutcome()
		if acknowledged {
			return nil
		}
		if reason == nil {
			reason = ErrChannelClosed
		}
		return reason
	}
}

func (c *Channel) sendLocalTerminal(ctx context.Context, frame framechannel.Frame) error {
	if err := c.acquireOwnedSendTurn(ctx); err != nil {
		return err
	}
	defer c.releaseSendTurn()

	if err := c.lifecycle.requireSendState(terminalLocalPending); err != nil {
		return err
	}
	if err := c.waitForOwnedCapacity(ctx, terminalLocalPending); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	attempted, err := c.lifecycle.transmit(
		terminalLocalPending,
		func() error { return c.dc.SendText(terminalIntentControl) },
	)
	if err != nil {
		if !attempted {
			return err
		}
		return fmt.Errorf("%w: send terminal intent: %w", ErrTransport, err)
	}

	if err := c.waitForOwnedCapacity(ctx, terminalLocalPending); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.lifecycle.requireSendState(terminalLocalPending); err != nil {
		return err
	}
	attempted, err = c.lifecycle.transmitLocalTerminal(
		func() error { return c.dc.Send(frame) },
	)
	if err != nil {
		if !attempted {
			return err
		}
		return fmt.Errorf("%w: send terminal frame: %w", ErrTransport, err)
	}
	return nil
}

// Close waits behind either admitted terminal direction. This preserves the
// terminal owner's final-frame/ack sequence instead of racing it with Pion close.
func (c *Channel) Close() error {
	closeNow := c.lifecycle.closeIfIdle()
	if closeNow {
		c.requestPhysicalClose()
	}
	<-c.lifecycle.doneSignal()
	// A remote-terminal receiver normally observes the sender's peer close. If
	// its owner explicitly closes first, the ack has already left SendText and
	// Done guarantees Recv is final, so cleanup may safely request physical close.
	c.requestPhysicalClose()
	<-c.physicalDone
	c.physicalMu.Lock()
	err := c.physicalErr
	c.physicalMu.Unlock()
	if err != nil {
		return fmt.Errorf("%w: close DataChannel: %w", ErrTransport, err)
	}
	return nil
}

func validateOutboundFrame(frame framechannel.Frame) error {
	if len(frame) == 0 || len(frame) > framechannel.MaxFrameSize {
		return fmt.Errorf("%w: got %d bytes, want 1..%d", ErrFrameBounds, len(frame), framechannel.MaxFrameSize)
	}
	return nil
}

func (c *Channel) onOpen() {
	c.callbackMu.Lock()
	defer c.callbackMu.Unlock()
	if err := c.reconcileOpen(); err != nil {
		c.enqueueTermination(err)
	}
}

func (c *Channel) reconcileOpen() error {
	switch c.dc.ReadyState() {
	case pion.DataChannelStateOpen:
	case pion.DataChannelStateClosing, pion.DataChannelStateClosed:
		return ErrRemoteClosed
	default:
		return fmt.Errorf("%w: open callback observed state %s", ErrInvalidDataChannel, c.dc.ReadyState())
	}
	if err := validateDataChannelParameters(c.dc); err != nil {
		return err
	}
	if err := validateMessageCapability(c.dc); err != nil {
		return err
	}
	c.lifecycle.publishOpen()
	return nil
}

func (c *Channel) onClose() {
	c.callbackMu.Lock()
	defer c.callbackMu.Unlock()
	c.enqueueTermination(ErrRemoteClosed)
}

func (c *Channel) onError(err error) {
	if err == nil {
		err = errors.New("unspecified DataChannel error")
	}
	c.callbackMu.Lock()
	defer c.callbackMu.Unlock()
	c.enqueueTermination(fmt.Errorf("%w: %w", ErrTransport, err))
}

func (c *Channel) onMessage(message pion.DataChannelMessage) {
	c.callbackMu.Lock()
	defer c.callbackMu.Unlock()

	admission := c.lifecycle.callbackAdmission()
	if admission == callbackAdmissionClosed {
		return
	}
	if admission == callbackAdmissionConnecting {
		if err := c.reconcileOpen(); err != nil {
			c.enqueueTermination(err)
			return
		}
	}

	if message.IsString {
		if len(message.Data) == 0 || len(message.Data) > maximumControlBytes {
			c.enqueuePeerFailure("text control is outside the reserved vocabulary")
			return
		}
		control := parseControl(message.Data)
		switch control {
		case controlTerminalIntent:
			if !c.lifecycle.reserveRemoteIntent() {
				c.enqueuePeerFailure("duplicate or conflicting terminal intent")
				return
			}
			c.enqueueInbound(inboundEvent{kind: inboundTerminalIntent})
		case controlTerminalAck:
			c.acceptTerminalAck()
		default:
			c.enqueuePeerFailure("unknown text control")
		}
		return
	}
	if len(message.Data) == 0 || len(message.Data) > framechannel.MaxFrameSize {
		c.enqueuePeerFailure(fmt.Sprintf("binary message has invalid size %d", len(message.Data)))
		return
	}
	c.enqueueInbound(inboundEvent{kind: inboundBinary, data: append([]byte(nil), message.Data...)})
}

func (c *Channel) acceptTerminalAck() {
	if !c.lifecycle.acknowledgeLocalTerminal(c.requestPhysicalClose) {
		c.enqueuePeerFailure("unsolicited or duplicate terminal acknowledgement")
	}
}

func (c *Channel) enqueuePeerFailure(detail string) {
	c.enqueueTermination(fmt.Errorf("%w: %s", ErrPeerProtocol, detail))
}

func (c *Channel) enqueueTermination(reason error) {
	if !c.lifecycle.beginTermination(reason) {
		return
	}
	c.enqueueInbound(inboundEvent{kind: inboundTermination})
}

// enqueueInbound is called with callbackMu held. Holding admission order until
// the bounded queue accepts the event prevents a later close/error callback from
// overtaking a message callback that has already copied its payload.
func (c *Channel) enqueueInbound(event inboundEvent) {
	select {
	case c.inbound <- event:
	case <-c.lifecycle.stopSignal():
	}
}

func (c *Channel) runInbound() {
	recvOpen := true
	closeRecv := func() {
		if recvOpen {
			close(c.recv)
			recvOpen = false
		}
	}
	defer func() {
		closeRecv()
		close(c.inboundDone)
	}()
	if c.inboundGate != nil {
		select {
		case <-c.inboundGate:
		case <-c.lifecycle.stopSignal():
			return
		}
	}

	for {
		select {
		case <-c.lifecycle.stopSignal():
			return
		case event := <-c.inbound:
			if !c.handleInbound(event, closeRecv) {
				return
			}
		}
	}
}

func (c *Channel) handleInbound(event inboundEvent, closeRecv func()) bool {
	switch event.kind {
	case inboundTerminalIntent:
		return c.handleTerminalIntent()
	case inboundTermination:
		// The lifecycle retained the immutable callback cause when termination
		// won; completion consumes that authority instead of copying it twice.
		c.finish(nil)
		return false
	case inboundBinary:
	default:
		c.failPeer("internal inbound event is invalid")
		return false
	}

	switch c.lifecycle.inboundBinaryMode() {
	case inboundBinaryOrdinary:
		select {
		case c.recv <- framechannel.Frame(event.data):
			return true
		case <-c.lifecycle.localTerminalSignal():
			return true
		case <-c.lifecycle.stopSignal():
			return false
		}
	case inboundBinaryRemoteTerminal:
		select {
		case c.recv <- framechannel.Frame(event.data):
		case <-c.lifecycle.stopSignal():
			return false
		}
		closeRecv()
		c.lifecycle.markRemoteTerminalPublished()
		if err := c.sendTerminalAck(); err != nil {
			c.finish(err)
			return false
		}
		if c.remoteTerminalFinishGate != nil {
			<-c.remoteTerminalFinishGate
		}
		// The terminal sender owns physical close after it observes this ack.
		// Closing here can overtake the acknowledgement still buffered in SCTP.
		c.finishRemoteTerminal(nil)
		return false
	case inboundBinaryDiscard:
		// The peer may already have queued opposite-direction traffic when local
		// terminal admission occurs. Discard it so application backpressure cannot
		// hide the acknowledgement behind a full receive queue.
		return true
	default:
		return false
	}
}

func (c *Channel) handleTerminalIntent() bool {
	if !c.lifecycle.acceptRemoteIntent() {
		c.failPeer("duplicate or conflicting terminal intent")
		return false
	}
	return true
}

func (c *Channel) sendTerminalAck() error {
	ctx := context.Background()
	if err := c.acquireOwnedSendTurn(ctx); err != nil {
		return err
	}
	defer c.releaseSendTurn()
	if err := c.lifecycle.requireSendState(terminalRemotePending); err != nil {
		return err
	}
	if err := c.waitForOwnedCapacity(ctx, terminalRemotePending); err != nil {
		return err
	}
	attempted, err := c.lifecycle.transmitRemoteTerminalAck(
		func() error { return c.dc.SendText(terminalAckControl) },
	)
	if err != nil {
		if !attempted {
			return err
		}
		return fmt.Errorf("%w: send terminal acknowledgement: %w", ErrTransport, err)
	}
	return nil
}

func (c *Channel) failPeer(detail string) {
	c.finish(fmt.Errorf("%w: %s", ErrPeerProtocol, detail))
}

// finish is the only logical close transition. The inbound owner closes Recv
// before the finalizer exposes Done, avoiding send/close panics without blocking
// a Pion callback on application consumption.
func (c *Channel) finish(reason error) bool {
	return c.finishWithPhysicalClose(reason, true)
}

func (c *Channel) finishRemoteTerminal(reason error) bool {
	return c.finishWithPhysicalClose(reason, false)
}

func (c *Channel) finishWithPhysicalClose(reason error, closePhysical bool) bool {
	if !c.lifecycle.finish(reason) {
		if closePhysical {
			c.requestPhysicalClose()
		}
		return false
	}
	if closePhysical {
		c.requestPhysicalClose()
	}
	return true
}

func (c *Channel) runFinalizer() {
	<-c.lifecycle.stopSignal()
	<-c.inboundDone
	c.lifecycle.complete()
	c.traces.shutdown()
}

func (c *Channel) requestPhysicalClose() {
	c.physicalOnce.Do(func() {
		go func() {
			err := c.dc.Close()
			c.physicalMu.Lock()
			c.physicalErr = err
			c.physicalMu.Unlock()
			close(c.physicalDone)
		}()
	})
}
