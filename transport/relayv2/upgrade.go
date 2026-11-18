package relayv2

import (
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

func senderUpgradeFailure(address string, cause error) error {
	if errors.Is(cause, errUpgradeNotFound) {
		if hint, ok := networkendpoint.WebSocketBaseHint(address); ok {
			return &WebSocketRouteError{BaseHint: hint, cause: cause}
		}
	}
	return cause
}
