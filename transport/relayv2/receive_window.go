package relayv2

import (
	"bytes"

	"github.com/windshare/windshare/core/framechannel"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

// Only a receiver owns the relay's delivery window. Its socket reader remains
// free to process heartbeat/retirement controls while the application is idle.
func (c *Channel) receiveLoop() {
	defer close(c.receiveDone)
	defer close(c.recv)
	for {
		select {
		case <-c.receiveAbort:
			return
		case frame, ok := <-c.incoming:
			if !ok {
				return
			}
			select {
			case <-c.receiveAbort:
				return
			case c.recv <- frame:
				c.link.returnReceiveCredit(c.id, len(frame)+v2.OpaqueRouteHeaderBytes)
			}
		}
	}
}

func (c *Channel) stopReceiving() {
	if c.incoming != nil {
		c.receiveAbortOnce.Do(func() { close(c.receiveAbort) })
	}
}

func (c *Channel) deliverReceived(frame []byte) bool {
	select {
	case c.incoming <- bytes.Clone(frame):
		return true
	default:
		return false
	}
}

func (l *link) returnReceiveCredit(id v2.RelaySessionID, size int) {
	l.writeMu.Lock()
	credit := l.receiveCredits[id]
	if credit == nil {
		l.writeMu.Unlock()
		return
	}
	credit.Frames++
	credit.Bytes += uint32(size)
	ready := credit.Frames >= v2.ReceiveCreditBatchFrames
	l.writeMu.Unlock()
	if ready {
		select {
		case l.writeWake <- struct{}{}:
		default:
		}
	}
}

// Credits bypass application sends: a full outgoing window must never prevent
// the peer from learning that its responses can make progress again.
func (l *link) takeReceiveCredit() ([]byte, error) {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	for id, credit := range l.receiveCredits {
		if credit.Frames < v2.ReceiveCreditBatchFrames {
			continue
		}
		encoded, err := credit.MarshalBinary()
		if err != nil {
			return nil, ErrProtocol
		}
		*credit = v2.ReceiveCredit{RelaySessionID: id}
		return encoded, nil
	}
	return nil, nil
}

func newReceiverChannel(id v2.RelaySessionID, link *link) *Channel {
	c := &Channel{
		id: id, link: link, state: framechannel.Open,
		recv:         make(chan framechannel.Frame),
		incoming:     make(chan framechannel.Frame, v2.ReceiveWindowFrames),
		receiveAbort: make(chan struct{}), receiveDone: make(chan struct{}),
	}
	go c.receiveLoop()
	return c
}
