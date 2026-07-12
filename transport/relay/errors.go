package relay

import (
	"errors"
	"fmt"
)

var ErrReconnectGraceExpired = errors.New("relay: reconnect grace expired")

var ErrSessionIngressOverflow = errors.New("relay: session inbound queue full")

var ErrSenderSessionHistoryFull = errors.New("relay: sender session history full")

// IngressKind identifies the bounded local queue that rejected peer traffic.
// Keeping the category typed lets connection policy react without parsing the
// English diagnostic retained for operators.
type IngressKind string

const (
	IngressFrames        IngressKind = "frames"
	IngressSignals       IngressKind = "signals"
	IngressSessionEvents IngressKind = "session_events"
)

// SessionIngressOverflow is local backpressure containment for one relay
// session. It never classifies the shared WebSocket connection as failed.
type SessionIngressOverflow struct {
	Kind IngressKind
}

func (e *SessionIngressOverflow) Error() string {
	return fmt.Sprintf("relay: session inbound %s queue is full", e.Kind)
}

func (e *SessionIngressOverflow) Unwrap() error { return ErrSessionIngressOverflow }

// SenderSessionHistoryOverflow bounds the active-plus-terminal session registry
// for one physical sender link. Recycling that link is safer than forgetting a
// tombstone and allowing late traffic to revive a completed session.
type SenderSessionHistoryOverflow struct {
	Limit int
}

func (e *SenderSessionHistoryOverflow) Error() string {
	return fmt.Sprintf("relay: sender session history reached its limit of %d", e.Limit)
}

func (e *SenderSessionHistoryOverflow) Unwrap() error { return ErrSenderSessionHistoryFull }

// ProtocolViolationKind keeps peer-behavior classification independent from
// English diagnostics so callers and hostile tests do not parse error text.
type ProtocolViolationKind string

const (
	ProtocolViolationRegisteredShareMismatch ProtocolViolationKind = "registered_share_mismatch"
	ProtocolViolationUnexpectedMessage       ProtocolViolationKind = "unexpected_message"
	ProtocolViolationUnexpectedBinary        ProtocolViolationKind = "unexpected_binary"
	ProtocolViolationMalformedMessage        ProtocolViolationKind = "malformed_message"
	ProtocolViolationManifestSequence        ProtocolViolationKind = "manifest_sequence"
	ProtocolViolationMalformedManifest       ProtocolViolationKind = "malformed_manifest"
	ProtocolViolationForeignSession          ProtocolViolationKind = "foreign_session"
	ProtocolViolationMalformedFrame          ProtocolViolationKind = "malformed_frame"
)

// ProtocolViolation reports invalid relay behavior while retaining any codec
// cause for errors.Is/errors.As inspection.
type ProtocolViolation struct {
	Kind   ProtocolViolationKind
	detail string
	cause  error
}

func (e *ProtocolViolation) Error() string {
	message := "relay: protocol violation " + string(e.Kind)
	if e.detail != "" {
		message += ": " + e.detail
	}
	if e.cause != nil {
		message += ": " + e.cause.Error()
	}
	return message
}

func (e *ProtocolViolation) Unwrap() error { return e.cause }

func protocolViolation(kind ProtocolViolationKind, detail string, cause error) error {
	return &ProtocolViolation{Kind: kind, detail: detail, cause: cause}
}
