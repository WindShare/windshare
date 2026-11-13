package networkruntime

import (
	"context"
	"errors"
	"sync"

	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/networkendpoint"
)

func (s Selection) DiagnosticRecord() diagnostics.Record {
	candidates := make([]diagnostics.Value, 0, len(s.Decision.Candidates))
	filters := make([]diagnostics.Value, 0, len(s.Decision.Filters))
	for _, candidate := range s.Decision.Candidates {
		candidates = append(candidates, diagnostics.Object(diagnostics.Fields{
			"endpoint": diagnostics.Text(candidate.URL), "alias": diagnostics.Text(candidate.Alias), "configured": diagnostics.Bool(candidate.Configured)}))
	}
	for _, filter := range s.Decision.Filters {
		filters = append(filters, diagnostics.Object(diagnostics.Fields{"endpoint": diagnostics.Text(filter.Endpoint), "reason": diagnostics.Text(filter.Reason)}))
	}
	return diagnostics.New("share_network_plan", diagnostics.Fields{"generation": diagnostics.Uint(s.Decision.DirectoryGeneration), "serial": diagnostics.Uint(s.Decision.DirectorySerial),
		"network_generation": diagnostics.Text(s.Decision.NetworkGeneration), "configured_only": diagnostics.Bool(s.Decision.ConfiguredOnly),
		"published_candidates": diagnostics.Array(candidates...), "filters": diagnostics.Array(filters...), "frontend": diagnostics.Text(s.Frontend), "frontend_reason": diagnostics.Text(s.Decision.FrontendReason)})
}

func AdmissionRecord(value relayset.Admission) diagnostics.Record {
	return diagnostics.New("network_relay_admission", diagnostics.Fields{"endpoint": diagnostics.Text(value.Endpoint), "primary": diagnostics.Text(value.Primary),
		"state": diagnostics.Text(value.State), "admissions": diagnostics.Int(int64(value.Count))})
}

func (r *Runtime) Observer(sink diagnostics.Sink) func(string, senderrelay.Attempt) {
	var mu sync.Mutex
	generations := map[string]string{}
	return func(url string, attempt senderrelay.Attempt) {
		mu.Lock()
		defer mu.Unlock()
		if attempt.State == senderrelay.AttemptStarted {
			generations[url] = r.generation()
		}
		fields := diagnostics.Fields{"endpoint": diagnostics.Text(url), "attempt": diagnostics.Uint(uint64(attempt.Number)), "state": diagnostics.Uint(uint64(attempt.State)),
			"registration_generation": diagnostics.Uint(attempt.Generation), "timeout": diagnostics.Bool(errors.Is(attempt.Failure, context.DeadlineExceeded)),
			"stage":       diagnostics.Text(attempt.Stage),
			"duration_ms": diagnostics.Millis(attempt.Duration), "resume": diagnostics.Bool(attempt.Resume), "terminal": diagnostics.Bool(attempt.Terminal), "backoff_ms": diagnostics.Millis(attempt.NextDelay)}
		if attempt.Failure != nil {
			fields["failure"] = diagnostics.Text(attempt.Failure.Error())
		}
		sink.Emit(diagnostics.New("relay_registration_attempt", fields).Correlate(diagnostics.Fields{"share_instance_id": diagnostics.Identity(attempt.ShareInstance[:])}))
		if attempt.State != senderrelay.AttemptSucceeded && attempt.State != senderrelay.AttemptFailed && attempt.State != senderrelay.AttemptParked {
			return
		}
		if generations[url] != r.generation() {
			return
		}
		endpoint, err := networkendpoint.NormalizeRelay(url)
		if err != nil {
			return
		}
		r.observations.Record(relayplan.Observation{Identity: endpoint.IdentityURL, NetworkGeneration: generations[url], At: r.now(), Admitted: attempt.State != senderrelay.AttemptFailed, Duration: attempt.Duration})
	}
}
