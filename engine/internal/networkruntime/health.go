package networkruntime

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/transport/relayv2"
)

const (
	failureCountWindow = 10 * time.Second
	failureDecay       = 3 * time.Minute
)

type nodeHealth struct {
	mu         sync.Mutex
	generation uint64
	entries    map[string]healthEntry
}

type healthEntry struct {
	relayplan.Observation
	lastCountedAt time.Time
	failures      int
}

// Late callbacks and concurrent plans cannot roll the observed generation back.
func (h *nodeHealth) advanceLocked(generation uint64) bool {
	if generation > h.generation || h.entries == nil {
		h.generation = generation
		h.entries = make(map[string]healthEntry)
	}
	return generation == h.generation
}

func (r *Runtime) ServerRetryAt(identity string) time.Time {
	generation := r.generation()
	r.health.mu.Lock()
	defer r.health.mu.Unlock()
	if r.health.generation != generation {
		return time.Time{}
	}
	return r.health.entries[identity].ServerRetryAt
}

func (h *nodeHealth) Snapshot(generation uint64, now time.Time) []relayplan.Observation {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.advanceLocked(generation) {
		return nil
	}
	var facts []relayplan.Observation
	for identity, entry := range h.entries {
		if now.Sub(entry.At) > relayplan.ObservationAge {
			delete(h.entries, identity)
		} else {
			facts = append(facts, entry.Observation)
		}
	}
	return facts
}

// RecordAttempt returns a diagnostic update only when a local or server
// deadline changed. Each share owns its own retry clock even without a sink.
func (h *nodeHealth) RecordAttempt(identity string, started, generation uint64, now time.Time, attempt senderrelay.Attempt) (healthEntry, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.advanceLocked(generation) || started != generation {
		return healthEntry{}, false
	}
	entry := h.entries[identity]
	if entry.At.After(now) {
		// Out-of-order completion cannot overwrite a newer sample, but its
		// explicit node deadline still binds concurrent registrations.
		if attempt.State == senderrelay.AttemptFailed && attempt.ServerRetryAt.After(entry.ServerRetryAt) &&
			failureReason(attempt.Failure) == relayplan.CooldownServerBackoff {
			entry.ServerRetryAt = attempt.ServerRetryAt
			h.entries[identity] = entry
			return entry, true
		}
		return healthEntry{}, false
	}
	changed := false
	switch attempt.State {
	case senderrelay.AttemptSucceeded, senderrelay.AttemptParked:
		// A concurrent success clears soft avoidance but cannot revoke an
		// explicit HTTP deadline received by another registration.
		server := entry.ServerRetryAt
		entry = healthEntry{}
		if server.After(now) {
			entry.ServerRetryAt = server
		}
		entry.Admitted, entry.Duration = true, attempt.HandshakeDuration
	case senderrelay.AttemptFailed:
		reason := failureReason(attempt.Failure)
		if reason == relayplan.CooldownNone {
			return healthEntry{}, false
		}
		previousCooldown := entry.CooldownUntil
		counted := entry.countFailure(now, reason)
		changed = entry.CooldownUntil.After(previousCooldown)
		if reason == relayplan.CooldownServerBackoff && attempt.ServerRetryAt.After(entry.ServerRetryAt) {
			entry.ServerRetryAt = attempt.ServerRetryAt
			changed = true
		}
		if !counted && !changed {
			return healthEntry{}, false
		}
		entry.Admitted, entry.Duration = false, 0
	default:
		return healthEntry{}, false
	}
	entry.Identity, entry.NetworkGeneration, entry.At = identity, strconv.FormatUint(generation, 10), now
	if _, known := h.entries[identity]; !known && len(h.entries) >= nodedirectory.MaximumNodes {
		var oldest string
		for id, fact := range h.entries {
			if oldest == "" || fact.At.Before(h.entries[oldest].At) {
				oldest = id
			}
		}
		delete(h.entries, oldest)
	}
	h.entries[identity] = entry
	return entry, changed
}

func (e *healthEntry) countFailure(now time.Time, reason relayplan.CooldownReason) bool {
	latest := e.lastCountedAt
	if e.CooldownUntil.After(latest) {
		latest = e.CooldownUntil
	}
	if e.ServerRetryAt.After(latest) {
		latest = e.ServerRetryAt
	}
	if now.Sub(latest) >= failureDecay {
		e.failures = 0
		e.CooldownUntil, e.ServerRetryAt = time.Time{}, time.Time{}
		e.CooldownReason = relayplan.CooldownNone
	}
	if e.failures > 0 && now.Sub(e.lastCountedAt) < failureCountWindow {
		return false
	}
	e.failures++
	e.lastCountedAt = now
	delay := cooldownDelay(reason, e.failures)
	if delay > 0 && now.Add(delay).After(e.CooldownUntil) {
		e.CooldownReason, e.CooldownUntil = reason, now.Add(delay)
	}
	return true
}

func cooldownDelay(reason relayplan.CooldownReason, failures int) time.Duration {
	switch reason {
	case relayplan.CooldownNodeRejected:
		steps := [...]time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute}
		return steps[min(failures, len(steps))-1]
	case relayplan.CooldownTransportFailure:
		failures--
		if failures == 0 {
			return 0
		}
	}
	steps := [...]time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute}
	return steps[min(failures, len(steps))-1]
}

func failureReason(err error) relayplan.CooldownReason {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, senderrelay.ErrStopped) || errors.Is(err, relayv2.ErrProtocol) {
		return relayplan.CooldownNone
	}
	if _, ok := errors.AsType[*relayv2.RelayError](err); ok {
		return relayplan.CooldownNone
	}
	if _, ok := errors.AsType[*senderrelay.AuthorizationError](err); ok {
		return relayplan.CooldownNone
	}
	if route, ok := errors.AsType[*relayv2.WebSocketRouteError](err); ok && route.BaseHint.URL() != "" {
		return relayplan.CooldownNodeRejected
	}
	if upgrade, ok := errors.AsType[*relayv2.WebSocketDialError](err); ok {
		switch upgrade.StatusCode {
		case http.StatusTooManyRequests, http.StatusServiceUnavailable:
			return relayplan.CooldownServerBackoff
		}
		// Client-error responses can describe this request's address or credentials.
		if upgrade.StatusCode >= http.StatusBadRequest && upgrade.StatusCode < http.StatusInternalServerError {
			return relayplan.CooldownNone
		}
	}
	return relayplan.CooldownTransportFailure
}
