package webrtc

import (
	"context"
	"errors"
	"sync"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/channeltest"
)

var errFakeClosed = errors.New("fake DataChannel is closed")

type fakeSent struct {
	frame    framechannel.Frame
	terminal bool
}

type fakeDataChannel struct {
	mu sync.Mutex

	label             string
	protocol          string
	ordered           bool
	maxPacketLifeTime *uint16
	maxRetransmits    *uint16
	negotiated        bool
	ready             pion.DataChannelState
	maximumMessage    uint32

	buffered      uint64
	lowThreshold  uint64
	sendIncrement uint64
	bufferedRead  chan struct{}

	onLow      func()
	onOpen     func()
	onClose    func()
	onError    func(error)
	onMessage  func(pion.DataChannelMessage)
	setupClose func()

	peerTerminal bool
	closed       bool
	closeCount   int
	sendErr      error
	textErr      error
	closeErr     error

	sent        chan fakeSent
	terminalAck chan struct{}
	onAck       func()
}

func newFakeDataChannel(ready pion.DataChannelState) *fakeDataChannel {
	return &fakeDataChannel{
		label:          ChannelLabel,
		protocol:       ChannelProtocol,
		ordered:        true,
		ready:          ready,
		maximumMessage: framechannel.MaxFrameSize,
		sent:           make(chan fakeSent, 256),
		terminalAck:    make(chan struct{}, 8),
	}
}

func (f *fakeDataChannel) Label() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.label
}

func (f *fakeDataChannel) Protocol() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.protocol
}

func (f *fakeDataChannel) Ordered() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ordered
}

func (f *fakeDataChannel) MaxPacketLifeTime() *uint16 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxPacketLifeTime
}

func (f *fakeDataChannel) MaxRetransmits() *uint16 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxRetransmits
}

func (f *fakeDataChannel) Negotiated() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.negotiated
}

func (f *fakeDataChannel) ReadyState() pion.DataChannelState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ready
}

func (f *fakeDataChannel) BufferedAmount() uint64 {
	f.mu.Lock()
	amount := f.buffered
	observed := f.bufferedRead
	f.mu.Unlock()
	if observed != nil {
		select {
		case observed <- struct{}{}:
		default:
		}
	}
	return amount
}

func (f *fakeDataChannel) SetBufferedAmountLowThreshold(threshold uint64) {
	f.mu.Lock()
	f.lowThreshold = threshold
	f.mu.Unlock()
}

func (f *fakeDataChannel) OnBufferedAmountLow(callback func()) {
	f.mu.Lock()
	f.onLow = callback
	f.mu.Unlock()
}

func (f *fakeDataChannel) OnOpen(callback func()) {
	f.mu.Lock()
	f.onOpen = callback
	f.mu.Unlock()
}

func (f *fakeDataChannel) OnClose(callback func()) {
	f.mu.Lock()
	f.onClose = callback
	f.mu.Unlock()
}

func (f *fakeDataChannel) OnError(callback func(error)) {
	f.mu.Lock()
	f.onError = callback
	f.mu.Unlock()
}

func (f *fakeDataChannel) OnMessage(callback func(pion.DataChannelMessage)) {
	f.mu.Lock()
	f.onMessage = callback
	setupClose := f.setupClose
	f.setupClose = nil
	f.mu.Unlock()
	if setupClose != nil {
		setupClose()
	}
}

func (f *fakeDataChannel) Send(data []byte) error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return errFakeClosed
	}
	if f.sendErr != nil {
		err := f.sendErr
		f.mu.Unlock()
		return err
	}
	terminal := f.peerTerminal
	if terminal {
		f.peerTerminal = false
	}
	f.buffered += f.sendIncrement
	sent := f.sent
	f.mu.Unlock()
	sent <- fakeSent{frame: framechannel.Frame(data), terminal: terminal}
	return nil
}

func (f *fakeDataChannel) SendText(text string) error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return errFakeClosed
	}
	if f.textErr != nil {
		err := f.textErr
		f.mu.Unlock()
		return err
	}
	switch text {
	case terminalIntentControl:
		f.peerTerminal = true
		f.mu.Unlock()
		return nil
	case terminalAckControl:
		ack := f.terminalAck
		hook := f.onAck
		f.mu.Unlock()
		if hook != nil {
			hook()
		}
		select {
		case ack <- struct{}{}:
		default:
		}
		return nil
	default:
		f.mu.Unlock()
		return errors.New("fake received unexpected text")
	}
}

func (f *fakeDataChannel) Close() error {
	f.mu.Lock()
	f.closeCount++
	if f.closed {
		err := f.closeErr
		f.mu.Unlock()
		return err
	}
	f.closed = true
	f.ready = pion.DataChannelStateClosed
	callback := f.onClose
	err := f.closeErr
	f.mu.Unlock()
	if callback != nil {
		callback()
	}
	return err
}

func (f *fakeDataChannel) maxMessageSize() uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maximumMessage
}

func (f *fakeDataChannel) open(maximum uint32) {
	f.mu.Lock()
	f.maximumMessage = maximum
	f.ready = pion.DataChannelStateOpen
	callback := f.onOpen
	f.mu.Unlock()
	if callback != nil {
		callback()
	}
}

func (f *fakeDataChannel) markOpenWithoutCallback(maximum uint32) {
	f.mu.Lock()
	f.maximumMessage = maximum
	f.ready = pion.DataChannelStateOpen
	f.mu.Unlock()
}

func (f *fakeDataChannel) fireOpenCallback() {
	f.mu.Lock()
	callback := f.onOpen
	f.mu.Unlock()
	if callback != nil {
		callback()
	}
}

func (f *fakeDataChannel) remoteClose() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	f.ready = pion.DataChannelStateClosed
	callback := f.onClose
	f.mu.Unlock()
	if callback != nil {
		callback()
	}
}

func (f *fakeDataChannel) fireCloseCallback() {
	f.mu.Lock()
	callback := f.onClose
	f.mu.Unlock()
	if callback != nil {
		callback()
	}
}

func (f *fakeDataChannel) fail(err error) {
	f.mu.Lock()
	callback := f.onError
	f.mu.Unlock()
	if callback != nil {
		callback(err)
	}
}

func (f *fakeDataChannel) deliverBinary(frame framechannel.Frame) {
	f.deliver(pion.DataChannelMessage{Data: frame})
}

func (f *fakeDataChannel) deliverText(text string) {
	f.deliver(pion.DataChannelMessage{Data: []byte(text), IsString: true})
}

func (f *fakeDataChannel) deliver(message pion.DataChannelMessage) {
	f.mu.Lock()
	callback := f.onMessage
	f.mu.Unlock()
	if callback != nil {
		callback(message)
	}
}

func (f *fakeDataChannel) setBuffered(amount uint64) {
	f.mu.Lock()
	f.buffered = amount
	f.mu.Unlock()
}

func (f *fakeDataChannel) observeBufferedReads() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bufferedRead = make(chan struct{}, 1)
	return f.bufferedRead
}

func (f *fakeDataChannel) fireLow() {
	f.mu.Lock()
	callback := f.onLow
	f.mu.Unlock()
	if callback != nil {
		callback()
	}
}

func (f *fakeDataChannel) receiveSent(ctx context.Context) (channeltest.SentFrame, error) {
	select {
	case sent := <-f.sent:
		if sent.terminal {
			f.deliverText(terminalAckControl)
		}
		return channeltest.SentFrame{Frame: sent.frame, Terminal: sent.terminal}, nil
	case <-ctx.Done():
		return channeltest.SentFrame{}, ctx.Err()
	}
}

func (f *fakeDataChannel) deliverTerminal(ctx context.Context, frame framechannel.Frame) error {
	f.deliverText(terminalIntentControl)
	f.deliverBinary(frame)
	select {
	case <-f.terminalAck:
		f.remoteClose()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeDataChannel) closeCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeCount
}
