package community

import (
	"errors"
	"time"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/serviceauthority"
)

const (
	probeFailureDecay        = 3 * time.Minute
	maxProbeBackoff          = 5 * time.Minute
	minRelaySuppressDuration = 15 * time.Second
	maxFailureScopesPerPeer  = 8
)

var (
	transientProbeBackoff = [...]time.Duration{
		15 * time.Second,
		30 * time.Second,
		time.Minute,
		2 * time.Minute,
		maxProbeBackoff,
	}
	rejectedProbeBackoff = [...]time.Duration{
		time.Minute,
		2 * time.Minute,
		maxProbeBackoff,
	}
)

type probeFailureKind uint8

const (
	probeFailureNone probeFailureKind = iota
	probeFailureTransient
	probeFailureRejected
	probeFailureRelayUnavailable
	probeFailureEvict
)

func (k probeFailureKind) diagnosticName() string {
	switch k {
	case probeFailureTransient:
		return "transient"
	case probeFailureRejected:
		return "rejected"
	case probeFailureRelayUnavailable:
		return "relay_unavailable"
	case probeFailureEvict:
		return "evict"
	default:
		return "unknown"
	}
}

type probeError struct {
	kind         probeFailureKind
	reason       string
	until        time.Time
	issuedAt     uint64
	hostVerified bool
	cause        error
}

func (e *probeError) Error() string {
	if e.cause != nil {
		return e.cause.Error()
	}
	return e.reason
}

func (e *probeError) Unwrap() error { return e.cause }

type probeBackoff struct {
	kind        probeFailureKind
	failures    int
	until       time.Time
	decayAnchor time.Time
}

func (p peer) relaySuppressed(now time.Time) bool {
	return p.relaySuppressedUntil.After(now)
}

func (p peer) failureState(scope Scope, now time.Time) (probeBackoff, bool) {
	b, ok := p.failures[scope]
	if !ok {
		return probeBackoff{}, false
	}
	if !b.until.After(now) && now.Sub(b.decayAnchor) >= probeFailureDecay {
		return probeBackoff{}, false
	}
	return b, true
}

func (p peer) warmBlocked(scope Scope, now time.Time) bool {
	if p.d.PublicRelay == serviceauthority.RelayAnonymous && p.relaySuppressed(now) {
		return true
	}
	if b, ok := p.failureState(scope, now); ok && b.until.After(now) {
		return true
	}
	return false
}

func (p *peer) clearFailure(scope Scope) {
	p.relaySuppressedUntil = time.Time{}
	p.relaySuppressedIssuedAt = 0
	if len(p.failures) == 0 {
		return
	}
	delete(p.failures, scope)
	for s := range p.failures {
		if s.Generation < scope.Generation {
			delete(p.failures, s)
		}
	}
	if len(p.failures) == 0 {
		p.failures = nil
	}
}

func (p *peer) suppressRelay(until time.Time, issuedAt uint64, now time.Time) time.Duration {
	minUntil := now.Add(minRelaySuppressDuration)
	if until.Before(minUntil) {
		until = minUntil
	}
	if maxUntil := now.Add(maxProbeBackoff); until.After(maxUntil) {
		until = maxUntil
	}
	if until.After(p.relaySuppressedUntil) {
		p.relaySuppressedUntil = until
	}
	if issuedAt > p.relaySuppressedIssuedAt {
		p.relaySuppressedIssuedAt = issuedAt
	}
	return max(p.relaySuppressedUntil.Sub(now), 0)
}

func (p *peer) recordFailure(scope Scope, kind probeFailureKind, now time.Time) time.Duration {
	if p.failures == nil {
		p.failures = make(map[Scope]probeBackoff, 1)
	}
	// Drop failures from superseded network generations and decayed scopes so
	// per-peer memory stays bounded without losing active origin isolation.
	for s, existing := range p.failures {
		if s.Generation < scope.Generation || (!existing.until.After(now) && now.Sub(existing.decayAnchor) >= probeFailureDecay) {
			delete(p.failures, s)
		}
	}
	b, existed := p.failures[scope]
	if !existed && len(p.failures) >= maxFailureScopesPerPeer {
		var oldest Scope
		var oldestAnchor time.Time
		first := true
		for s, existing := range p.failures {
			if first || existing.decayAnchor.Before(oldestAnchor) {
				oldest, oldestAnchor, first = s, existing.decayAnchor, false
			}
		}
		delete(p.failures, oldest)
	}
	if existed && (b.kind != kind || (!b.until.After(now) && now.Sub(b.decayAnchor) >= probeFailureDecay)) {
		b.failures = 0
	}
	b.kind = kind
	b.failures++
	delay := probeBackoffDelay(kind, b.failures)
	until := now.Add(delay)
	if b.until.After(until) {
		until = b.until
	}
	b.until = until
	b.decayAnchor = until
	p.failures[scope] = b
	return max(b.until.Sub(now), 0)
}

func probeBackoffDelay(kind probeFailureKind, failures int) time.Duration {
	if kind == probeFailureRejected {
		return rejectedProbeBackoff[min(failures, len(rejectedProbeBackoff))-1]
	}
	return transientProbeBackoff[min(failures, len(transientProbeBackoff))-1]
}

func classifyProbeError(err error) *probeError {
	if pe, ok := errors.AsType[*probeError](err); ok {
		return pe
	}
	return &probeError{kind: probeFailureTransient, reason: "probe_failed", cause: err}
}

func liveRelayReason(live serviceauthority.Descriptor) string {
	switch live.PublicRelay {
	case serviceauthority.RelayBudgetExhausted:
		return "live_budget_exhausted"
	case serviceauthority.RelayUnavailable:
		return "live_unavailable"
	case serviceauthority.RelayAuthenticated:
		return "live_authenticated"
	default:
		if !live.Services.Relay {
			return "live_relay_service_disabled"
		}
		return "live_relay_unavailable"
	}
}

func (p *Pool) recordProbeFailureLocked(identity string, scope Scope, probed serviceauthority.GossipDescriptor, err error, now time.Time) {
	pe := classifyProbeError(err)
	latest, ok := p.peers[identity]
	if !ok || latest.d.NodeKey != probed.NodeKey {
		return
	}
	if pe.kind != probeFailureEvict && (latest.d.PublicRelay != probed.PublicRelay || latest.d.Services != probed.Services) {
		return
	}
	switch pe.kind {
	case probeFailureEvict:
		if pe.issuedAt >= latest.d.IssuedAt {
			delete(p.peers, identity)
			p.trace("community_peer_evicted", identity, pe.reason)
			return
		}
		// Keep a fresher gossip entry, but avoid re-probing it immediately in this scope.
		if latest.relay.scope == scope {
			latest.relay = relayObservation{}
		}
		if latest.host.generation == scope.Generation {
			latest.host = hostObservation{}
		}
		backoff := latest.recordFailure(scope, probeFailureRejected, now)
		p.peers[identity] = latest
		p.traceProbeFailure(identity, pe.reason, probeFailureRejected, backoff)
	case probeFailureRelayUnavailable:
		// Live signed unavailability applies to relay service across every frontend
		// origin without discarding independently verified STUN host observations.
		latest.relay = relayObservation{}
		var backoff time.Duration
		kind := probeFailureRelayUnavailable
		if pe.issuedAt >= latest.d.IssuedAt {
			backoff = latest.suppressRelay(pe.until, pe.issuedAt, now)
		} else {
			kind = probeFailureRejected
			backoff = latest.recordFailure(scope, probeFailureRejected, now)
		}
		p.peers[identity] = latest
		p.traceProbeFailure(identity, pe.reason, kind, backoff)
	default:
		if latest.relay.scope == scope {
			latest.relay = relayObservation{}
		}
		if !pe.hostVerified && latest.host.generation == scope.Generation && (latest.relay.scope == (Scope{}) || latest.relay.scope == scope) {
			latest.host = hostObservation{}
		}
		backoff := latest.recordFailure(scope, pe.kind, now)
		p.peers[identity] = latest
		p.traceProbeFailure(identity, pe.reason, pe.kind, backoff)
	}
}

// RecordOriginForbidden invalidates cached relay verification for scope and
// applies origin-scoped rejected backoff when a real relay dial receives HTTP
// 403 after background warming. Other frontend origins and origin-independent
// STUN observations remain unaffected.
func (p *Pool) RecordOriginForbidden(scope Scope, identity string) {
	if p == nil || scope.Origin == "" || identity == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	latest, ok := p.peers[identity]
	if !ok {
		return
	}
	p.recordProbeFailureLocked(identity, scope, latest.d, &probeError{
		kind:         probeFailureRejected,
		reason:       "dial_origin_forbidden",
		hostVerified: true,
	}, p.now())
}

func (p *Pool) traceProbeFailure(identity, reason string, kind probeFailureKind, backoff time.Duration) {
	if !p.sink.Enabled() {
		return
	}
	p.sink.Emit(diagnostics.New("community_probe_failed", diagnostics.Fields{
		"source":     diagnostics.Text(identity),
		"reason":     diagnostics.Text(reason),
		"kind":       diagnostics.Text(kind.diagnosticName()),
		"backoff_ms": diagnostics.Millis(backoff),
	}).At(p.now()))
}
