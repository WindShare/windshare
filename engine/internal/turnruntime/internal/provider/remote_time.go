package provider

import (
	"fmt"
	"net/http"
	"time"
)

const httpDateResolution = time.Second

type remoteGrantResponse struct {
	grant       remoteGrant
	headers     http.Header
	requestedAt time.Time
}

func (reply remoteGrantResponse) localExpiry(now time.Time) (time.Time, error) {
	serverDate, err := http.ParseTime(reply.headers.Get("Date"))
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: managed TURN response requires a valid HTTP Date", ErrConfiguration)
	}
	// Expiry and Date share the issuer's clock. The upper end of Date's
	// one-second precision cannot overstate the remaining credential lifetime.
	remaining := time.UnixMilli(reply.grant.ExpiresAt).Sub(serverDate.Add(httpDateResolution))
	if remaining > ManagedLeaseLifetime {
		return time.Time{}, fmt.Errorf("%w: managed TURN lifetime %s exceeds %s", ErrConfiguration, remaining, ManagedLeaseLifetime)
	}
	// Anchor to request start, retaining Go's monotonic clock and conservatively
	// charging the entire response trip instead of assuming synchronized clocks.
	expires := reply.requestedAt.Add(remaining)
	if remaining <= 0 || !expires.After(now) {
		return time.Time{}, fmt.Errorf("%w: managed TURN usable_for=%s request_elapsed=%s", ErrExpired, remaining, now.Sub(reply.requestedAt))
	}
	return expires, nil
}
