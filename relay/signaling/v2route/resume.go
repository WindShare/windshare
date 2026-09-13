package v2route

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"sync/atomic"
	"time"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

type routeGeneration struct {
	traceID uint64
}

var nextRouteGeneration atomic.Uint64

func newRouteGeneration() *routeGeneration {
	return &routeGeneration{traceID: nextRouteGeneration.Add(1)}
}

// ResumeAttempt binds credential validation to the exact registration and owner
// observed before the sender challenge. Its opaque fields prevent a caller from
// rebuilding authority against whatever owner happens to exist after the proof.
type ResumeAttempt struct {
	registry   *Registry
	init       v2.RegisterInit
	route      *route
	generation *routeGeneration
	owner      ConnectionRef
}

// BeginResume rejects invalid credentials before spending challenge capacity.
// An absent route still produces an attempt: only an authenticated sender may
// receive the NotFound result that permits publishing the share again.
func (r *Registry) BeginResume(ctx context.Context, init v2.RegisterInit, token v2.ResumeToken) (attempt ResumeAttempt, err error) {
	if r == nil || init.Mode != v2.RegistrationResume || init.Validate() != nil {
		return ResumeAttempt{}, ErrResume
	}
	trace := ResumeTrace{ShareID: init.ShareID, ShareInstance: init.ShareInstance, Phase: ResumeCredentialPhase}
	defer func() { r.traceResume(trace, err) }()
	if err := ctx.Err(); err != nil {
		return ResumeAttempt{}, err
	}
	tokenHash := sha256.Sum256(token[:])
	if subtle.ConstantTimeCompare(tokenHash[:], init.ResumeTokenHash[:]) != 1 {
		return ResumeAttempt{}, ErrResume
	}
	current, stopped, err := r.lockRoute(ctx, init.ShareID)
	defer r.mu.Unlock()
	if err != nil {
		return ResumeAttempt{}, err
	}
	if err := ctx.Err(); err != nil {
		return ResumeAttempt{}, err
	}
	if stopped != nil {
		return ResumeAttempt{}, ErrStopped
	}
	attempt = ResumeAttempt{registry: r, init: init, route: current}
	if current == nil {
		return attempt, nil
	}
	trace.ExpectedGeneration = current.generation.traceID
	trace.ExpectedOwnerGeneration = current.owner.LocalGeneration()
	if current.state == routeStopUncertain {
		return ResumeAttempt{}, ErrStopped
	}
	if current.pendingStop != nil {
		return ResumeAttempt{}, ErrStopping
	}
	if current.init.ShareInstance != init.ShareInstance ||
		subtle.ConstantTimeCompare(current.init.PKHash[:], init.PKHash[:]) != 1 ||
		subtle.ConstantTimeCompare(current.init.DescriptorDigest[:], init.DescriptorDigest[:]) != 1 ||
		subtle.ConstantTimeCompare(current.init.ResumeTokenHash[:], tokenHash[:]) != 1 {
		return ResumeAttempt{}, ErrResume
	}
	switch current.state {
	case routeStarting:
		return ResumeAttempt{}, ErrStarting
	case routeLive, routeGrace:
	default:
		return ResumeAttempt{}, ErrResume
	}
	attempt.generation, attempt.owner = current.generation, current.owner
	return attempt, nil
}

// Resume atomically replaces only the owner bound before authentication. The
// retired participants retain their exact ConnectionRefs, so delayed endpoint
// cleanup cannot disconnect the new sender or its newly admitted sessions.
func (r *Registry) Resume(ctx context.Context, attempt ResumeAttempt, authority v2.SenderAuthority, owner ConnectionRef) (retirement RouteRetirement, err error) {
	if r == nil || attempt.registry != r || !authority.Authorizes(attempt.init) || !owner.Valid() {
		return RouteRetirement{}, ErrResume
	}
	trace := ResumeTrace{
		ShareID: attempt.init.ShareID, ShareInstance: attempt.init.ShareInstance, Phase: ResumeCommitPhase,
		ExpectedOwnerGeneration: attempt.owner.LocalGeneration(), NewOwnerGeneration: owner.LocalGeneration(),
	}
	if attempt.generation != nil {
		trace.ExpectedGeneration = attempt.generation.traceID
	}
	defer func() { r.traceResume(trace, err) }()
	if err := ctx.Err(); err != nil {
		return RouteRetirement{}, err
	}
	current, stopped, err := r.lockRoute(ctx, attempt.init.ShareID)
	defer r.mu.Unlock()
	if err != nil {
		return RouteRetirement{}, err
	}
	if err := ctx.Err(); err != nil {
		return RouteRetirement{}, err
	}
	if stopped != nil {
		return RouteRetirement{}, ErrStopped
	}
	if current != nil {
		trace.CurrentGeneration = current.generation.traceID
		if current.state == routeStopUncertain {
			return RouteRetirement{}, ErrStopped
		}
		if current.pendingStop != nil {
			return RouteRetirement{}, ErrStopping
		}
	}
	if current != attempt.route ||
		(current != nil && (current.generation != attempt.generation || current.owner != attempt.owner)) {
		return RouteRetirement{}, ErrResumeStale
	}
	if current == nil {
		return RouteRetirement{}, ErrNotFound
	}
	if current.owner == owner {
		return RouteRetirement{}, ErrResume
	}
	r.expireSessionTombstones(r.now())
	retirement = RouteRetirement{Owner: current.owner, Sessions: r.dropShareSessions(attempt.init.ShareID)}
	current.owner = owner
	current.generation = newRouteGeneration()
	current.state = routeLive
	current.graceDeadline = time.Time{}
	trace.CurrentGeneration = current.generation.traceID
	trace.RetiredSessions = len(retirement.Sessions)
	return retirement, nil
}
