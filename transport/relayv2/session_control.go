package relayv2

import (
	"context"

	"github.com/windshare/windshare/core/framechannel"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

type sendWindow struct{ frames, bytes int }

func (l *link) senderWindowLocked(id v2.RelaySessionID) *sendWindow {
	window := l.windows[id]
	if window == nil {
		window = &sendWindow{frames: v2.SenderWindowFrames, bytes: v2.SenderWindowBytes}
		l.windows[id] = window
	}
	return window
}

func (l *link) replenishCredit(credit v2.SessionCredit) bool {
	l.channelMu.Lock()
	defer l.channelMu.Unlock()
	// Credit can cross local retirement. It must never recreate a channel or
	// its budget after the authoritative owner has released it.
	if l.channels[credit.RelaySessionID] == nil {
		return true
	}
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	window := l.senderWindowLocked(credit.RelaySessionID)
	if int(credit.Frames) > v2.SenderWindowFrames-window.frames ||
		int(credit.Bytes) > v2.SenderWindowBytes-window.bytes {
		return false
	}
	window.frames += int(credit.Frames)
	window.bytes += int(credit.Bytes)
	select {
	case l.writeWake <- struct{}{}:
	default:
	}
	return true
}

// ConfirmAdmission retains a provisional relay session only after its application
// owner has authenticated the E2E handshake. It is sender-only and carries no
// opaque payload, so admission cannot be inferred from arbitrary receiver bytes.
func (c *Channel) ConfirmAdmission(ctx context.Context) error {
	if c.link.fixed {
		return framechannel.RejectSend(ErrProtocol)
	}
	encoded, err := (v2.SessionAdmitted{RelaySessionID: c.id}).MarshalBinary()
	if err != nil {
		return framechannel.RejectSend(err)
	}
	return c.link.enqueue(ctx, c, &sendRequest{
		kind: sendSessionAdmission, data: encoded, receipt: make(chan error, 1),
		channelID: c.id, operationID: c.link.nextOperationID(),
	})
}
