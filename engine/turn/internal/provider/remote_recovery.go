package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"
)

const (
	// A small replay budget recovers missing replies within the original deadline.
	remoteIssueAttempts = 3
	remoteRetryDelay    = 100 * time.Millisecond
)

type remoteFailure struct {
	status int
	code   string
}

func (failure remoteFailure) Error() string {
	return fmt.Sprintf("managed TURN request returned HTTP %d (%s)", failure.status, failure.code)
}

func retryRemoteIssue(err error) bool {
	var failure remoteFailure
	if errors.As(err, &failure) {
		return failure.status >= 500 || (failure.status == 409 && failure.code == "lease_pending")
	}
	var network net.Error
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &network)
}

func (source *Remote) resolve(ctx context.Context, path string, request remoteRequest) (remoteGrant, error) {
	ctx, cancel := context.WithTimeout(ctx, HTTPTimeout)
	defer cancel()
	var err error
	for attempt := 1; attempt <= remoteIssueAttempts; attempt++ {
		var grant remoteGrant
		err = remoteCall(ctx, source.config.Client, source.base+path, source.config.AccessKey, request, &grant)
		if err == nil {
			return grant, nil
		}
		slog.Debug("TURN remote acquisition", "service_id", request.ServiceID, "lease_id", request.LeaseID,
			"session_id", request.SessionID, "path_id", request.PathID, "attempt", attempt, "reason", err)
		if ctx.Err() != nil || !retryRemoteIssue(err) || attempt == remoteIssueAttempts {
			break
		}
		timer := time.NewTimer(remoteRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return remoteGrant{}, ctx.Err()
		case <-timer.C:
		}
	}
	return remoteGrant{}, err
}
