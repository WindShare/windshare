package relayv2

import (
	"context"
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/networkendpoint"
)

var errUpgradeNotFound = errors.New("WebSocket upgrade returned HTTP 404")

// WebSocketRouteError identifies an observed missing route for a candidate that
// could have been copied from the full WebSocket endpoint.
type WebSocketRouteError struct {
	BaseHint networkendpoint.RelayBaseHint
	cause    error
}

func (err *WebSocketRouteError) Error() string {
	return fmt.Sprintf("WebSocket route returned HTTP 404. Check the advertised relay_url (possible base %q).", err.BaseHint.URL())
}

func (err *WebSocketRouteError) Unwrap() error { return err.cause }

// WebSocketDialError is a handshake or HTTP upgrade failure that is not a
// missing route. Its text is safe: the library status and response body are
// retained only as the unwrapped cause for diagnostics and errors.Is.
type WebSocketDialError struct {
	Cause error
}

func (err *WebSocketDialError) Error() string { return "WebSocket upgrade failed" }

func (err *WebSocketDialError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Cause
}

// SocketError is a connection-level close or I/O failure after the WebSocket
// is established. The public text stays the closed-transport sentinel; the
// library close frame is unwrapped only for errors.Is.
type SocketError struct {
	Cause error
}

func (err *SocketError) Error() string { return ErrClosed.Error() }

func (err *SocketError) Unwrap() []error {
	if err == nil {
		return nil
	}
	return []error{ErrClosed, err.Cause}
}

func upgradeFailure(address string, cause error) error {
	if errors.Is(cause, errUpgradeNotFound) {
		if hint, ok := networkendpoint.WebSocketBaseHint(address); ok {
			return &WebSocketRouteError{BaseHint: hint, cause: cause}
		}
	}
	return cause
}

func dialFailure(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, errUpgradeNotFound) {
		return err
	}
	return &WebSocketDialError{Cause: err}
}

func socketFailure(err error) error {
	if err == nil || retainedSocketCause(err) {
		return err
	}
	return &SocketError{Cause: err}
}

func retainedSocketCause(err error) bool {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, ErrProtocol), errors.Is(err, ErrClosed), errors.Is(err, ErrFrameBounds),
		errors.Is(err, ErrIngressOverflow), errors.Is(err, ErrEgressOverflow),
		errors.Is(err, ErrSessionRetired):
		return true
	}
	var relayErr *RelayError
	var socketErr *SocketError
	return errors.As(err, &relayErr) || errors.As(err, &socketErr)
}
