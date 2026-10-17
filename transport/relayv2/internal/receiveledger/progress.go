package receiveledger

import (
	"math"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

const (
	minimumEnvelopeBytes = v2.OpaqueRouteHeaderBytes + 1
	maximumEnvelopeBytes = v2.OpaqueRouteHeaderBytes + v2.MaxOpaqueCiphertextBytes
)

// Receipt carries the exact received prefix without retaining payloads or a
// second per-frame queue. The owner retains it alongside the corresponding frame.
type Receipt struct {
	session Session
	before  Counts
	after   Counts
}

// Receive reserves capacity before the owner retains a parsed WS2O. Retirement
// still accepts in-flight envelopes until the ordered WS2N freezes the prefix.
func (l *Ledger) Receive(session Session, encodedBytes uint64) (Receipt, error) {
	f := session.flow
	if err := l.check(session); err != nil {
		return Receipt{}, l.reject(ActionReceive, f, err)
	}
	if f.Phase >= Draining {
		return Receipt{}, l.reject(ActionReceive, f, ErrFrozenPrefix)
	}
	if encodedBytes < minimumEnvelopeBytes || encodedBytes > maximumEnvelopeBytes {
		return Receipt{}, l.reject(ActionReceive, f, ErrFrameSize)
	}
	if f.Received.Frames == math.MaxUint64 || encodedBytes > math.MaxUint64-f.Received.EncodedBytes {
		return Receipt{}, l.reject(ActionReceive, f, ErrCounterExhausted)
	}
	if l.usage.Outstanding.Frames == l.config.Capacity.Frames ||
		encodedBytes > l.config.Capacity.EncodedBytes-l.usage.Outstanding.EncodedBytes {
		return Receipt{}, l.reject(ActionReceive, f, ErrCapacity)
	}
	size := Counts{1, encodedBytes}
	receipt := Receipt{session: session, before: f.Received, after: f.Received.add(size)}
	f.Received = receipt.after
	l.usage.Held = l.usage.Held.add(size)
	l.usage.Outstanding = l.usage.Outstanding.add(size)
	return receipt, nil
}

// Consume follows FIFO application handoff, not network arrival. A download's
// application-level cancellation still consumes its frames while the relay
// session serves other work; Discard is for relay-session retirement only.
func (l *Ledger) Consume(receipt Receipt) error {
	return l.complete(receipt, ActionConsume)
}

func (l *Ledger) Discard(receipt Receipt) error {
	return l.complete(receipt, ActionDiscard)
}

func (l *Ledger) complete(receipt Receipt, action Action) error {
	f := receipt.session.flow
	if err := l.check(receipt.session); err != nil {
		return l.reject(action, f, err)
	}
	if receipt.before != f.completed || receipt.after.Frames <= receipt.before.Frames {
		return l.reject(action, f, ErrReceipt)
	}
	if action == ActionDiscard && f.Phase == Active {
		return l.reject(action, f, ErrRetirement)
	}
	size := receipt.after.subtract(receipt.before)
	f.completed = receipt.after
	l.usage.Held = l.usage.Held.subtract(size)
	if action == ActionDiscard {
		f.Discarded = f.Discarded.add(size)
		l.trace(ActionDiscard, f, nil)
	} else {
		f.Consumed = f.Consumed.add(size)
		if f.Discarded.Frames == 0 {
			f.ConsumedPrefix = f.completed
			l.usage.PendingConsumed = l.usage.PendingConsumed.add(size)
		}
	}
	return nil
}

// Retire keeps both accounting and admission slots until final settlement.
// It does not assert that the relay has stopped sending.
func (l *Ledger) Retire(session Session) error {
	f := session.flow
	if err := l.check(session); err != nil {
		return l.reject(ActionRetire, f, err)
	}
	if f.Phase == Active {
		f.Phase = Retiring
		l.trace(ActionRetire, f, nil)
	}
	return nil
}

// Notice is called in socket order after every preceding WS2O has been recorded.
// Equal totals prove ordering, not application consumption.
func (l *Ledger) Notice(session Session, notice v2.SettlementNotice) error {
	f := session.flow
	if err := l.check(session); err != nil {
		return l.reject(ActionNotice, f, err)
	}
	if notice.RelaySessionID != f.id ||
		notice.TotalFrames != f.Received.Frames || notice.TotalEncodedBytes != f.Received.EncodedBytes {
		return l.reject(ActionNotice, f, ErrSettlement)
	}
	if f.Phase != Settled {
		f.Phase = Draining
	}
	f.ackPending = true
	l.trace(ActionNotice, f, nil)
	return nil
}
