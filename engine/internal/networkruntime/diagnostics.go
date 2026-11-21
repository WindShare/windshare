package networkruntime

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/senderrelay"
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
		fields := diagnostics.Fields{"endpoint": diagnostics.Text(candidate.URL), "alias": diagnostics.Text(candidate.Alias), "configured": diagnostics.Bool(candidate.Configured)}
		if !candidate.Configured {
			if planned, ok := plannedNetwork[candidate.URL]; ok {
				fields["observation"] = diagnostics.Text(planned.HistoryState())
				if duration, observed := planned.ObservedHandshake(); observed {
					fields["observed_handshake_ms"] = diagnostics.Millis(duration)
				}
			}
		}
		deadlineField(fields, "cooldown_until", candidate.CooldownUntil)
		deadlineField(fields, "server_retry_at", candidate.ServerRetryAt)
		candidates = append(candidates, diagnostics.Object(fields))
	}
	for _, filter := range s.Decision.Filters {
		filters = append(filters, diagnostics.Object(diagnostics.Fields{"endpoint": diagnostics.Text(filter.Endpoint), "reason": diagnostics.Text(filter.Reason)}))
	}
	return diagnostics.New("share_network_plan", diagnostics.Fields{"generation": diagnostics.Uint(s.Decision.DirectoryGeneration), "serial": diagnostics.Uint(s.Decision.DirectorySerial),
		"network_generation": diagnostics.Text(s.Decision.NetworkGeneration), "configured_only": diagnostics.Bool(s.Decision.ConfiguredOnly),
		"cooldown_fallback": diagnostics.Bool(s.Decision.CooldownFallback), "published_candidates": diagnostics.Array(candidates...), "filters": diagnostics.Array(filters...), "frontend": diagnostics.Text(s.Frontend),
		"alternate_frontend": diagnostics.Text(s.AlternateFrontend), "frontend_reason": diagnostics.Text(s.Decision.FrontendReason)})
}

func AdmissionRecord(value relayset.Admission) diagnostics.Record {
	return diagnostics.New("network_relay_admission", diagnostics.Fields{"endpoint": diagnostics.Text(value.Endpoint), "primary": diagnostics.Text(value.Primary),
		"state": diagnostics.Text(value.State), "reason": diagnostics.Text(value.Reason), "admissions": diagnostics.Int(int64(value.Count))})
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

func (r *Runtime) Observer(selection Selection, sink diagnostics.Sink) func(string, senderrelay.Attempt) {
	identities := make(map[string]string, len(selection.Plan.Network))
	for _, candidate := range selection.Plan.Network {
		identities[candidate.URL] = candidate.Identity
	}
	var mu sync.Mutex
	generations := make(map[string]uint64, len(identities))
	return func(url string, attempt senderrelay.Attempt) {
		var started uint64
		var entry healthEntry
		var change relayplan.CooldownReason
		now := r.now()
		if identity, directory := identities[url]; directory {
			mu.Lock()
			if attempt.State == senderrelay.AttemptStarted {
				generations[url] = r.generation()
			}
			started = generations[url]
			mu.Unlock()
			switch attempt.State {
			case senderrelay.AttemptSucceeded, senderrelay.AttemptParked, senderrelay.AttemptFailed:
				if r.shouldRefreshDirectoryOnRefusal(identity, attempt) {
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

func (r *Runtime) shouldRefreshDirectoryOnRefusal(identity string, attempt senderrelay.Attempt) bool {
	if attempt.State != senderrelay.AttemptFailed || attempt.Resume || r.directory == nil {
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

func attemptFields(url string, attempt senderrelay.Attempt, now time.Time) diagnostics.Fields {
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
	if attempt.State == senderrelay.AttemptSucceeded || attempt.State == senderrelay.AttemptParked {
		fields["handshake_ms"] = diagnostics.Millis(attempt.HandshakeDuration)
	}
	return fields
}
