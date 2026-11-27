package networkruntime

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/networkendpoint"
	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/engine/networkdirectory"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

func (s Selection) DiagnosticRecord() diagnostics.Record {
	candidates := make([]diagnostics.Value, 0, len(s.Decision.Candidates))
	filters := make([]diagnostics.Value, 0, len(s.Decision.Filters))
	plannedNetwork := make(map[string]relayplan.Candidate, len(s.Plan.Network))
	for _, candidate := range s.Plan.Network {
		plannedNetwork[candidate.URL] = candidate
	}
	for _, candidate := range s.Decision.Candidates {
		fields := diagnostics.Fields{"endpoint": diagnostics.Text(candidate.URL), "alias": diagnostics.Text(candidate.Alias), "source": diagnostics.Text(candidate.Source), "configured": diagnostics.Bool(candidate.Configured)}
		if !candidate.Configured {
			if planned, ok := plannedNetwork[candidate.URL]; ok {
				applyPlannedCandidateFields(fields, planned)
			}
		}
		deadlineField(fields, "cooldown_until", candidate.CooldownUntil)
		deadlineField(fields, "server_retry_at", candidate.ServerRetryAt)
		candidates = append(candidates, diagnostics.Object(fields))
	}
	for _, filter := range s.Decision.Filters {
		filters = append(filters, diagnostics.Object(diagnostics.Fields{"endpoint": diagnostics.Text(filter.Endpoint), "reason": diagnostics.Text(filter.Reason)}))
	}
	standby := make([]diagnostics.Value, 0, len(s.Plan.Standby))
	for _, candidate := range s.Plan.Standby {
		standby = append(standby, diagnostics.Object(standbyCandidateFields(candidate)))
	}
	return diagnostics.New("share_network_plan", diagnostics.Fields{"generation": diagnostics.Uint(s.Decision.DirectoryGeneration), "serial": diagnostics.Uint(s.Decision.DirectorySerial),
		"network_generation": diagnostics.Text(s.Decision.NetworkGeneration), "configured_only": diagnostics.Bool(s.Decision.ConfiguredOnly),
		"cooldown_fallback": diagnostics.Bool(s.Decision.CooldownFallback), "initial_candidates": diagnostics.Array(candidates...), "standby_relays": diagnostics.Array(standby...), "filters": diagnostics.Array(filters...), "frontend": diagnostics.Text(s.Frontend),
		"alternate_frontend": diagnostics.Text(s.AlternateFrontend), "frontend_reason": diagnostics.Text(s.Decision.FrontendReason)})
}

func standbyCandidateFields(candidate relayplan.Candidate) diagnostics.Fields {
	fields := diagnostics.Fields{
		"endpoint": diagnostics.Text(candidate.URL),
		"source":   diagnostics.Text(string(candidate.Source)),
	}
	if candidate.Alias != "" {
		fields["alias"] = diagnostics.Text(candidate.Alias)
	}
	applyPlannedCandidateFields(fields, candidate)
	deadlineField(fields, "cooldown_until", candidate.CooldownUntil)
	deadlineField(fields, "server_retry_at", candidate.ServerRetryAt)
	return fields
}

func applyPlannedCandidateFields(fields diagnostics.Fields, planned relayplan.Candidate) {
	fields["tier"] = diagnostics.Text(string(planned.Tier()))
	fields["observation"] = diagnostics.Text(planned.HistoryState())
	if duration, observed := planned.ObservedHandshake(); observed {
		fields["observed_handshake_ms"] = diagnostics.Millis(duration)
	}
	switch planned.Source {
	case relayplan.Official:
		if planned.Zone != "" {
			fields["zone"] = diagnostics.Text(planned.Zone)
		}
	case relayplan.Community:
		// Verified and IPLiteral are community-only classification signals;
		// omitting them on official/configured routes avoids misleading false values.
		fields["verified"] = diagnostics.Bool(planned.Verified)
		fields["ip_literal"] = diagnostics.Bool(planned.IPLiteral)
	}
}

func AdmissionRecord(value relayset.Admission) diagnostics.Record {
	return diagnostics.New("network_relay_admission", diagnostics.Fields{"endpoint": diagnostics.Text(value.Endpoint), "primary": diagnostics.Text(value.Primary),
		"state": diagnostics.Text(value.State), "reason": diagnostics.Text(value.Reason), "admissions": diagnostics.Int(int64(value.Count))})
}

func FallbackRecord(value relayset.Fallback) diagnostics.Record {
	return diagnostics.New("network_relay_fallback", diagnostics.Fields{
		"reason":        diagnostics.Text(string(value.Reason)),
		"next_tier":     diagnostics.Text(string(value.Tier)),
		"active_relays": textArray(value.Active),
		"next_relays":   textArray(value.Next),
	})
}

func textArray(values []string) diagnostics.Value {
	items := make([]diagnostics.Value, 0, len(values))
	for _, value := range values {
		items = append(items, diagnostics.Text(value))
	}
	return diagnostics.Array(items...)
}

func (r *Runtime) emitDirectoryInitFailure(network string, err error) {
	if !r.diagnostics.Enabled() {
		return
	}
	fields := diagnostics.Fields{"error": diagnostics.Text(err.Error())}
	if network != "" {
		fields["network"] = diagnostics.Text(network)
	}
	if unavailableErr, ok := errors.AsType[*networkdirectory.UnavailableError](err); ok {
		fields["reason"] = diagnostics.Text(string(unavailableErr.Reason))
	}
	r.diagnostics.Emit(diagnostics.New("network_directory_initialization_failed", fields).At(r.now()))
}

func deadlineField(fields diagnostics.Fields, key string, deadline time.Time) {
	if !deadline.IsZero() {
		fields[key] = diagnostics.Text(deadline.UTC().Format(time.RFC3339Nano))
	}
}

func (r *Runtime) Observer(selection Selection, sink diagnostics.Sink) func(string, relayset.Attempt) {
	candidates := selection.Plan.NetworkCandidates()
	identities := make(map[string]relayplan.Candidate, len(candidates))
	for _, candidate := range candidates {
		identities[candidate.URL] = candidate
	}
	var mu sync.Mutex
	generations := make(map[string]uint64, len(identities))
	return func(url string, attempt relayset.Attempt) {
		var started uint64
		var entry healthEntry
		var change relayplan.CooldownReason
		now := r.now()
		if candidate, tracked := identities[url]; tracked {
			identity := candidate.Identity
			mu.Lock()
			if attempt.State == relayset.AttemptStarted {
				generations[url] = r.generation()
			}
			started = generations[url]
			mu.Unlock()
			switch attempt.State {
			case relayset.AttemptSucceeded, relayset.AttemptParked, relayset.AttemptFailed:
				if candidate.Source != relayplan.Community && r.shouldRefreshDirectoryOnRefusal(identity, attempt) {
					r.directory.TriggerRefresh()
				}
				entry, change = r.health.RecordAttempt(identity, started, r.generation(), now, attempt)
			}
		}
		if !sink.Enabled() {
			return
		}
		if entry.At.After(now) {
			now = entry.At
		}
		fields := attemptFields(url, attempt, now)
		if started != 0 {
			fields["network_generation"] = diagnostics.Uint(started)
		}
		if change != relayplan.CooldownNone {
			var delay time.Duration
			if entry.CooldownReason == change {
				delay = entry.CooldownUntil.Sub(now)
			}
			if change == relayplan.CooldownServerBackoff {
				delay = max(delay, entry.ServerRetryAt.Sub(now))
			}
			fields["cooldown_ms"] = diagnostics.Millis(delay)
			fields["cooldown_reason"] = diagnostics.Text(string(change))
			fields["consecutive_failures"] = diagnostics.Int(int64(entry.failures))
			deadlineField(fields, "server_retry_at", entry.ServerRetryAt)
		}
		sink.Emit(diagnostics.New("relay_registration_attempt", fields).Correlate(diagnostics.Fields{"share_instance_id": diagnostics.Identity(attempt.ShareInstance[:])}))
	}
}

func (r *Runtime) shouldRefreshDirectoryOnRefusal(identity string, attempt relayset.Attempt) bool {
	if attempt.State != relayset.AttemptFailed || attempt.Resume || r.directory == nil {
		return false
	}
	refusal, ok := errors.AsType[*relayv2.RelayError](attempt.Failure)
	if !ok || refusal.Code != v2.ErrorAccessUnavailable {
		return false
	}
	// Once the current directory already marks this relay draining or omits it,
	// further retries on a frozen share cannot converge the directory.
	return r.directoryListsSchedulableRelay(identity)
}

func (r *Runtime) directoryListsSchedulableRelay(identity string) bool {
	snapshot, usable := r.directory.Snapshot()
	if !usable {
		return true
	}
	for _, node := range snapshot.Payload().Nodes {
		if !node.HasRole("relay") || node.Status == nodedirectory.Draining {
			continue
		}
		endpoint, err := networkendpoint.NormalizeRelay(node.Relay)
		if err == nil && endpoint.IdentityURL == identity {
			return true
		}
	}
	return false
}

func attemptFields(url string, attempt relayset.Attempt, now time.Time) diagnostics.Fields {
	fields := diagnostics.Fields{"endpoint": diagnostics.Text(url), "attempt": diagnostics.Uint(uint64(attempt.Number)), "state": diagnostics.Text(attempt.State.DiagnosticName()),
		"registration_generation": diagnostics.Uint(attempt.Generation), "timeout": diagnostics.Bool(errors.Is(attempt.Failure, context.DeadlineExceeded)),
		"stage": diagnostics.Text(attempt.Stage), "duration_ms": diagnostics.Millis(attempt.Duration), "resume": diagnostics.Bool(attempt.Resume),
		"terminal": diagnostics.Bool(attempt.Terminal), "backoff_ms": diagnostics.Millis(attempt.NextDelay)}
	if attempt.Failure != nil {
		fields["failure"] = diagnostics.Text(attempt.Failure.Error())
	}
	if upgrade, ok := errors.AsType[*relayv2.WebSocketDialError](attempt.Failure); ok && upgrade.StatusCode != 0 {
		fields["http_status"] = diagnostics.Int(int64(upgrade.StatusCode))
	}
	if attempt.ServerRetryAt.After(now) {
		deadlineField(fields, "server_retry_at", attempt.ServerRetryAt)
	}
	if attempt.State == relayset.AttemptSucceeded || attempt.State == relayset.AttemptParked {
		fields["handshake_ms"] = diagnostics.Millis(attempt.HandshakeDuration)
	}
	return fields
}
