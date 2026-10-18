// Package receiveledger accounts for inbound delivery on one physical connection.
// Both endpoint roles use the same socket-free state machine. The caller serializes
// access, owns payload storage, and admits each non-reused wire session explicitly.
package receiveledger

import (
	"errors"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

var (
	ErrConfig            = errors.New("receive ledger: invalid configuration")
	ErrClosed            = errors.New("receive ledger: connection closed")
	ErrSession           = errors.New("receive ledger: unknown or stale session")
	ErrSessionLimit      = errors.New("receive ledger: retained session limit reached")
	ErrCapacity          = errors.New("receive ledger: connection capacity exceeded")
	ErrFrameSize         = errors.New("receive ledger: invalid full WS2O size")
	ErrCounterExhausted  = errors.New("receive ledger: cumulative counter exhausted; retire session")
	ErrReceipt           = errors.New("receive ledger: stale or out-of-order receipt")
	ErrRetirement        = errors.New("receive ledger: operation requires session retirement")
	ErrFrozenPrefix      = errors.New("receive ledger: received data after settlement notice")
	ErrSettlement        = errors.New("receive ledger: settlement does not match received prefix")
	ErrSettlementPending = errors.New("receive ledger: final settlement has not been issued")
)

// Counts always measures complete WS2O envelopes, including the route header.
type Counts struct {
	Frames       uint64
	EncodedBytes uint64
}

func (a Counts) add(b Counts) Counts {
	return Counts{a.Frames + b.Frames, a.EncodedBytes + b.EncodedBytes}
}

func (a Counts) subtract(b Counts) Counts {
	return Counts{a.Frames - b.Frames, a.EncodedBytes - b.EncodedBytes}
}

// Config separates immutable connection capacity, metadata bounds and batching
// policy. ConnectionID identifies this physical generation in observations.
type Config struct {
	ConnectionID uint64
	Capacity     v2.ReceiveCapacity
	MaxSessions  int
	Batch        v2.ReceiveCapacity
	Observer     Observer
}

// Session is a local lifetime token. Callbacks retain it, never just a wire ID:
// an old worker cannot account against a replacement connection or session.
type Session struct{ flow *flow }

type Phase uint8

const (
	Active Phase = iota
	Retiring
	Draining
	Settled
)

// Progress distinguishes application consumption from retirement discard.
// ConsumedPrefix stops at the first discarded envelope; only it can become WS2B.
type Progress struct {
	Phase          Phase
	Received       Counts
	Consumed       Counts
	Discarded      Counts
	ConsumedPrefix Counts
}

type flow struct {
	id v2.RelaySessionID
	Progress
	completed  Counts
	reported   Counts
	ackPending bool
}

// Usage includes data released locally but not yet reported to the relay.
// Outstanding is refunded only when a control is handed to the ordered writer.
type Usage struct {
	Held             Counts
	Outstanding      Counts
	PendingConsumed  Counts
	RetainedSessions int
}

type Ledger struct {
	config    Config
	threshold Counts
	sessions  map[v2.RelaySessionID]*flow
	order     []*flow
	cursor    int
	usage     Usage
	flushing  bool
	closed    bool
}

// Feedback starts at a fraction of either window axis, even when the configured
// batch is larger than the entire window.
const feedbackWindowDivisor = 4

func New(config Config) (*Ledger, error) {
	if config.ConnectionID == 0 || config.MaxSessions <= 0 ||
		config.Capacity.Validate() != nil || config.Batch.Validate() != nil {
		return nil, ErrConfig
	}
	return &Ledger{
		config: config,
		threshold: Counts{
			Frames:       max(1, min(config.Batch.Frames, config.Capacity.Frames/feedbackWindowDivisor)),
			EncodedBytes: max(1, min(config.Batch.EncodedBytes, config.Capacity.EncodedBytes/feedbackWindowDivisor)),
		},
		sessions: make(map[v2.RelaySessionID]*flow),
	}, nil
}

func (l *Ledger) Open(id v2.RelaySessionID) (Session, error) {
	if l.closed {
		return Session{}, l.reject(ActionOpen, nil, ErrClosed)
	}
	if id == (v2.RelaySessionID{}) || l.sessions[id] != nil {
		return Session{}, l.reject(ActionOpen, l.sessions[id], ErrSession)
	}
	if len(l.sessions) >= l.config.MaxSessions {
		return Session{}, l.reject(ActionOpen, nil, ErrSessionLimit)
	}
	f := &flow{id: id}
	l.sessions[id] = f
	l.order = append(l.order, f)
	l.usage.RetainedSessions++
	l.trace(ActionOpen, f, nil)
	return Session{f}, nil
}

func (l *Ledger) Usage() Usage { return l.usage }

func (l *Ledger) Progress(session Session) (Progress, bool) {
	if l.check(session) != nil {
		return Progress{}, false
	}
	return session.flow.Progress, true
}

func (l *Ledger) check(session Session) error {
	if l.closed {
		return ErrClosed
	}
	if session.flow == nil || l.sessions[session.flow.id] != session.flow {
		return ErrSession
	}
	return nil
}

// Close ends feedback authority; it does not claim consumption or buffer release.
// The owner still disposes of its payloads before dropping this generation.
func (l *Ledger) Close() {
	if !l.closed {
		l.closed = true
		l.trace(ActionClose, nil, nil)
	}
}
