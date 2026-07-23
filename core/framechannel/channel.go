// Package framechannel defines the transport-neutral byte channel consumed by
// suite-02 protocol sessions.
package framechannel

import (
	"context"
	"errors"
)

const MaxFrameSize = 64 * 1024

type ChannelState uint8

const (
	Connecting ChannelState = iota
	Open
	Closed
)

type Frame []byte

// SendDisposition describes whether a transport acquired ownership of a send
// before returning an error. Unclassified errors are deliberately treated as
// accepted: once ownership is uncertain, a protocol caller must not erase a
// potentially observed frame or retry it as though no transport saw it.
type SendDisposition uint8

const (
	SendAccepted SendDisposition = iota + 1
	SendRejected
	SendRetired
)

var (
	ErrSendRejected = errors.New("frame channel rejected send before transport acceptance")
	ErrSendRetired  = errors.New("frame channel retired before transport acceptance")
)

type sendFailure struct {
	disposition SendDisposition
	cause       error
}

func (failure *sendFailure) Error() string { return failure.cause.Error() }
func (failure *sendFailure) Unwrap() error { return failure.cause }

func (failure *sendFailure) Is(target error) bool {
	switch target {
	case ErrSendRejected:
		return failure.disposition == SendRejected
	case ErrSendRetired:
		return failure.disposition == SendRetired
	default:
		return false
	}
}

// RejectSend reports a local or caller-controlled rejection that either never
// reached transport ownership or was transactionally retracted before exposure.
// It is not evidence that the channel retired.
func RejectSend(cause error) error {
	return classifySendFailure(SendRejected, ErrSendRejected, cause)
}

// RetireSend reports that channel retirement won the transport admission race.
// Session owners may treat this exact disposition as natural lane completion;
// they must continue surfacing every accepted or unclassified delivery error.
func RetireSend(cause error) error {
	return classifySendFailure(SendRetired, ErrSendRetired, cause)
}

func classifySendFailure(disposition SendDisposition, fallback, cause error) error {
	if cause == nil {
		cause = fallback
	}
	observed := SendDispositionOf(cause)
	if observed == SendRejected || observed == SendRetired {
		return cause
	}
	return &sendFailure{disposition: disposition, cause: cause}
}

// SendDispositionOf returns the conservative ownership interpretation of err.
// A nil result means successful transport acceptance; a raw non-nil error is
// also accepted because the transport did not prove pre-acceptance rejection.
func SendDispositionOf(err error) SendDisposition {
	var failure *sendFailure
	if errors.As(err, &failure) {
		return failure.disposition
	}
	return SendAccepted
}

// Channel exposes only ordered frame delivery and lifecycle. Relay, WebRTC, and
// in-memory tests implement it without importing session or content semantics.
type Channel interface {
	Send(context.Context, Frame) error
	SendTerminal(context.Context, Frame) error
	Recv() <-chan Frame
	State() ChannelState
	Close() error
}
