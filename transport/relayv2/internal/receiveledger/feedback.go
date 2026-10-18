package receiveledger

import v2 "github.com/windshare/windshare/relay/protocol/v2"

// FeedbackReady never waits for more input: the relay may have granted less
// than this connection's capacity, including a single remaining envelope.
// The ordered writer coalesces all consumption accumulated before its next turn.
func (l *Ledger) FeedbackReady() bool {
	return !l.closed && l.usage.PendingConsumed.Frames != 0
}

// TakeConsumed hands a cumulative control to an ordered, connection-owned writer.
// The adapter must submit returned controls in order or close this generation on
// write failure; it must not drop them or enqueue them behind content credit.
func (l *Ledger) TakeConsumed() (v2.DeliveryConsumed, bool) {
	if !l.FeedbackReady() {
		return v2.DeliveryConsumed{}, false
	}
	for range len(l.order) {
		f := l.order[l.cursor]
		l.cursor = (l.cursor + 1) % len(l.order)
		if f.Phase == Settled || f.ConsumedPrefix == f.reported {
			continue
		}
		delta := f.ConsumedPrefix.subtract(f.reported)
		l.usage.Outstanding = l.usage.Outstanding.subtract(delta)
		l.usage.PendingConsumed = l.usage.PendingConsumed.subtract(delta)
		f.reported = f.ConsumedPrefix
		l.trace(ActionFeedback, f, nil)
		return v2.DeliveryConsumed{
			RelaySessionID: f.id, TotalFrames: f.reported.Frames, TotalEncodedBytes: f.reported.EncodedBytes,
		}, true
	}
	return v2.DeliveryConsumed{}, false
}

// TakeSettlement has the same ordered-writer obligation as TakeConsumed. It
// refunds only the suffix not already reported through WS2B. Duplicate notices
// can request another exact WS2S without refunding anything twice.
func (l *Ledger) TakeSettlement(session Session) (v2.SettlementAck, bool) {
	if l.check(session) != nil {
		return v2.SettlementAck{}, false
	}
	f := session.flow
	if !f.ackPending || f.completed != f.Received {
		return v2.SettlementAck{}, false
	}
	if f.Phase != Settled {
		l.usage.Outstanding = l.usage.Outstanding.subtract(f.Received.subtract(f.reported))
		l.usage.PendingConsumed = l.usage.PendingConsumed.subtract(f.ConsumedPrefix.subtract(f.reported))
		f.Phase = Settled
	}
	f.ackPending = false
	l.trace(ActionSettlement, f, nil)
	return v2.SettlementAck{
		RelaySessionID: f.id, TotalFrames: f.Received.Frames, TotalEncodedBytes: f.Received.EncodedBytes,
	}, true
}

// Forget releases metadata only after the final acknowledgement was issued.
// The writer must retain that control independently before the owner forgets.
// Late callbacks retain stale Session tokens and cannot recreate accounting.
func (l *Ledger) Forget(session Session) error {
	f := session.flow
	if err := l.check(session); err != nil {
		return l.reject(ActionForget, f, err)
	}
	if f.Phase != Settled || f.ackPending {
		return l.reject(ActionForget, f, ErrSettlementPending)
	}
	delete(l.sessions, f.id)
	for index, candidate := range l.order {
		if candidate == f {
			copy(l.order[index:], l.order[index+1:])
			l.order[len(l.order)-1] = nil
			l.order = l.order[:len(l.order)-1]
			if l.cursor > index {
				l.cursor--
			}
			if l.cursor >= len(l.order) {
				l.cursor = 0
			}
			break
		}
	}
	if len(l.order) == 0 {
		l.order = nil
	}
	l.usage.RetainedSessions--
	l.trace(ActionForget, f, nil)
	return nil
}
