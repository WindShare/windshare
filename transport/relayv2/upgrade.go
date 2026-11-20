package relayv2

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/windshare/windshare/core/networkendpoint"
)

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

// WebSocketDialError identifies a failure to establish a relay WebSocket.
// The original error remains available to diagnostics through Unwrap.
type WebSocketDialError struct {
	Cause      error
	StatusCode int
	// RetryAfter is the raw server delay. Each recovery policy bounds its own wait.
	RetryAfter time.Duration
}

func (err *WebSocketDialError) Error() string { return "WebSocket upgrade failed" }

func (err *WebSocketDialError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Cause
}

// RetryAfter returns the server's explicit delay for one endpoint's failed handshake.
func RetryAfter(failure error) time.Duration {
	if upgrade, ok := errors.AsType[*WebSocketDialError](failure); ok {
		return upgrade.RetryAfter
	}
	if rejection, ok := errors.AsType[*RelayError](failure); ok {
		return rejection.RetryAfter
	}
	return 0
}

func dialFailure(address string, response *http.Response, err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	failure := &WebSocketDialError{Cause: err}
	if response == nil {
		return failure
	}
	failure.StatusCode = response.StatusCode
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusServiceUnavailable {
		failure.RetryAfter = parseRetryAfter(response.Header.Get("Retry-After"), time.Now())
	}
	// Only a direct 404 can diagnose the supplied address, and retiring the
	// candidate requires a plausible base hint. A 404 during an outage may recover.
	if response.StatusCode == http.StatusNotFound &&
		response.Request != nil && response.Request.Response == nil {
		if hint, ok := networkendpoint.WebSocketBaseHint(address); ok {
			return &WebSocketRouteError{BaseHint: hint, cause: failure}
		}
	}
	return failure
}

func parseRetryAfter(header string, now time.Time) time.Duration {
	value := strings.TrimSpace(header)
	if value != "" && strings.Trim(value, "0123456789") == "" {
		// ParseUint saturates on range overflow. Clamp before converting seconds
		// to nanoseconds so a large valid delay cannot become an immediate retry.
		seconds, _ := strconv.ParseUint(value, 10, 64)
		return time.Duration(min(seconds, uint64(math.MaxInt64/int64(time.Second)))) * time.Second
	}
	date, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	return max(0, date.Sub(now))
}
