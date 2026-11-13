package senderrelay

import "context"

// AttemptControl limits individual handshakes, leaving retry clocks and resume
// authority with the endpoint. Waiting for capacity must not consume a dial timeout.
type AttemptControl interface {
	Begin(context.Context) (AttemptPermit, error)
	Unavailable()
}

type AttemptPermit interface {
	Admit() bool
	Release()
}
