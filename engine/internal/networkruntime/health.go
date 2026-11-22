package networkruntime

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/connectivity/relayset"
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
	decayAnchor   time.Time
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
	var until time.Time
	if r.health.generation == generation {
		until = r.health.entries[identity].ServerRetryAt
	}
	r.health.mu.Unlock()
	if !until.IsZero() && until.After(r.now()) {
		return until
	}
	return time.Time{}
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
			facts = append(facts, entry.ActiveAt(now))
		}
	}
	return facts
}

// RecordAttempt returns a diagnostic update only when a local or server
// deadline changed. The newer sample remains authoritative for late callbacks.
func (h *nodeHealth) RecordAttempt(identity string, started, generation uint64, now time.Time, attempt relayset.Attempt) (healthEntry, relayplan.CooldownReason) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.advanceLocked(generation) || started != generation {
		return healthEntry{}, relayplan.CooldownNone
	}
	entry := h.entries[identity]
	if entry.At.After(now) {
		entry.Observation = entry.ActiveAt(entry.At)
		// A late sample may extend the deadline still binding the newer sample.
		if attempt.State == relayset.AttemptFailed && failureReason(attempt.Failure) == relayplan.CooldownServerBackoff && entry.extendServerRetry(attempt.ServerRetryAt, entry.At) {
			h.entries[identity] = entry
			return entry, relayplan.CooldownServerBackoff
		}
		return entry, relayplan.CooldownNone
	}
	entry.Observation = entry.ActiveAt(now)
	change := relayplan.CooldownNone
	switch attempt.State {
	case relayset.AttemptSucceeded, relayset.AttemptParked:
		// A concurrent success clears soft avoidance but cannot revoke an
		// explicit HTTP deadline received by another registration.
		entry = healthEntry{ServerRetryAt: entry.ServerRetryAt, decayAnchor: entry.ServerRetryAt}
		entry.Admitted, entry.Duration = true, attempt.HandshakeDuration
	case relayset.AttemptFailed:
		reason := failureReason(attempt.Failure)
		if reason == relayplan.CooldownNone {
			return healthEntry{}, relayplan.CooldownNone
		}
		previousCooldown := entry.CooldownUntil
		counted := entry.countFailure(now, reason)
		if entry.CooldownUntil.After(previousCooldown) {
			change = reason
		}
		if reason == relayplan.CooldownServerBackoff && entry.extendServerRetry(attempt.ServerRetryAt, now) {
			change = reason
		}
		if !counted && change == relayplan.CooldownNone {
			return healthEntry{}, relayplan.CooldownNone
		}
		entry.Admitted, entry.Duration = false, 0
	default:
		return healthEntry{}, relayplan.CooldownNone
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
	return entry, change
}

func (e *healthEntry) extendServerRetry(until, now time.Time) bool {
	if !until.After(now) || !until.After(e.ServerRetryAt) {
		return false
	}
	e.ServerRetryAt = until
	if until.After(e.decayAnchor) {
		e.decayAnchor = until
	}
	return true
}

func (e *healthEntry) countFailure(now time.Time, reason relayplan.CooldownReason) bool {
	if now.Sub(e.decayAnchor) >= failureDecay {
		e.failures = 0
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
	if until := now.Add(delay); until.After(e.decayAnchor) {
		e.decayAnchor = until
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
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, relayset.ErrStopped) || errors.Is(err, relayv2.ErrProtocol) {
		return relayplan.CooldownNone
	}
	if _, ok := errors.AsType[*relayv2.RelayError](err); ok {
		return relayplan.CooldownNone
	}
	if _, ok := errors.AsType[*relayset.AuthorizationError](err); ok {
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
