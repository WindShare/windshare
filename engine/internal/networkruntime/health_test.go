package networkruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/networkendpoint"
	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/engine/relayprofile"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

func TestNodeHealthCountsFixedWindowsAndHonorsExplicitExtensions(t *testing.T) {
	var health nodeHealth
	start := time.Now()
	fail := func(elapsed, retry time.Duration) (healthEntry, relayplan.CooldownReason) {
		now := start.Add(elapsed)
		a := senderrelay.Attempt{State: senderrelay.AttemptFailed, Failure: &relayv2.WebSocketDialError{StatusCode: http.StatusServiceUnavailable}}
		if retry > 0 {
			a.ServerRetryAt = now.Add(retry)
		}
		return health.RecordAttempt("a", 1, 1, now, a)
	}
	first, changed := fail(0, time.Minute)
	if changed != relayplan.CooldownServerBackoff || first.failures != 1 || first.CooldownUntil.Sub(start) != 15*time.Second {
		t.Fatal(first)
	}
	extension, changed := fail(9*time.Second, time.Minute)
	if changed != relayplan.CooldownServerBackoff || extension.failures != 1 || extension.lastCountedAt != start || extension.CooldownUntil != first.CooldownUntil {
		t.Fatal(extension)
	}
	if _, changed = fail(9*time.Second, 30*time.Second); changed != relayplan.CooldownNone {
		t.Fatal("duplicate extended local backoff")
	}
	second, changed := fail(10*time.Second, 0)
	if changed != relayplan.CooldownServerBackoff || second.failures != 2 || second.CooldownUntil.Sub(start) != 40*time.Second || second.ServerRetryAt != extension.ServerRetryAt {
		t.Fatal(second)
	}
	// The server deadline is later than both the last count and local cooldown.
	next, _ := fail(extension.ServerRetryAt.Sub(start)+failureDecay-time.Nanosecond, 0)
	if next.failures != 3 {
		t.Fatal("premature decay", next)
	}
	reset, _ := fail(next.CooldownUntil.Sub(start)+failureDecay, 0)
	if reset.failures != 1 || reset.CooldownUntil.Sub(reset.At) != 15*time.Second {
		t.Fatal("missing decay", reset)
	}
}

func TestHealthBackoffLaddersAndSuccessfulPark(t *testing.T) {
	now := time.Now()
	hint, _ := networkendpoint.WebSocketBaseHint("https://relay.example/v2/ws")
	for _, tc := range []struct {
		failure error
		reason  relayplan.CooldownReason
		seconds []int
	}{
		{context.DeadlineExceeded, relayplan.CooldownTransportFailure, []int{0, 15, 30, 60, 120, 120}},
		{&relayv2.WebSocketRouteError{BaseHint: hint}, relayplan.CooldownNodeRejected, []int{60, 120, 300, 300}},
		{&relayv2.RegistrationNotSentError{Cause: &relayv2.WebSocketDialError{StatusCode: 429}}, relayplan.CooldownServerBackoff, []int{15, 30, 60, 120, 120}},
	} {
		var h nodeHealth
		for i, seconds := range tc.seconds {
			at := now.Add(time.Duration(i) * failureCountWindow)
			e, changed := h.RecordAttempt("node", 1, 1, at, senderrelay.Attempt{State: senderrelay.AttemptFailed, Failure: tc.failure})
			if e.failures != i+1 || (changed == tc.reason) != (seconds > 0) || e.Admitted {
				t.Fatal(e)
			}
			if seconds > 0 && (e.CooldownUntil.Sub(at) != time.Duration(seconds)*time.Second || e.CooldownReason != tc.reason) {
				t.Fatal(e)
			}
		}
		at := now.Add(time.Duration(len(tc.seconds)) * failureCountWindow)
		server := at.Add(time.Minute)
		h.RecordAttempt("node", 1, 1, at, senderrelay.Attempt{State: senderrelay.AttemptFailed, Failure: &relayv2.WebSocketDialError{StatusCode: 503}, ServerRetryAt: server})
		e, _ := h.RecordAttempt("node", 1, 1, at, senderrelay.Attempt{State: senderrelay.AttemptParked, Duration: time.Second, HandshakeDuration: 20 * time.Millisecond})
		if !e.Admitted || e.Duration != 20*time.Millisecond || e.failures != 0 || !e.CooldownUntil.IsZero() || e.ServerRetryAt != server {
			t.Fatal(e)
		}
	}
}

func TestRefusalScopeAndHealthRetention(t *testing.T) {
	var h nodeHealth
	now := time.Now()
	for _, err := range []error{nil, context.Canceled, senderrelay.ErrStopped, relayv2.ErrProtocol, &senderrelay.AuthorizationError{State: senderrelay.AuthorizationDenied},
		&relayv2.WebSocketDialError{StatusCode: 403}, &relayv2.WebSocketDialError{StatusCode: 404},
		&relayv2.RelayError{Code: v2.ErrorAccessDenied}, &relayv2.RelayError{Code: v2.ErrorInvalidProof},
		&relayv2.RelayError{Code: v2.ErrorAdmission}, &relayv2.RelayError{Code: v2.ErrorAccessUnavailable, RetryAfter: time.Second}} {
		h.RecordAttempt("ignored", 1, 1, now, senderrelay.Attempt{State: senderrelay.AttemptFailed, Failure: errors.Join(err), ServerRetryAt: now.Add(time.Minute)})
	}
	if len(h.Snapshot(1, now)) != 0 {
		t.Fatal("share refusal poisoned node health")
	}
	for i := range nodedirectory.MaximumNodes + 1 {
		h.RecordAttempt(fmt.Sprint(i), 1, 1, now.Add(time.Duration(i)), senderrelay.Attempt{State: senderrelay.AttemptSucceeded})
	}
	if entries := h.Snapshot(1, now); len(entries) != nodedirectory.MaximumNodes {
		t.Fatal(len(entries))
	}
	if _, ok := h.entries["0"]; ok {
		t.Fatal("oldest node not evicted")
	}
	if len(h.Snapshot(1, now.Add(relayplan.ObservationAge+time.Second))) != 0 {
		t.Fatal("stale entries survived")
	}
}

type monitorSource struct{ state networkstate.State }

func (s *monitorSource) Snapshot(context.Context) (networkstate.State, error) { return s.state, nil }

func TestObserverProjectsOnlyDirectoryAttemptsAcrossNetworkChanges(t *testing.T) {
	r := fixtureRuntime(t)
	source := &monitorSource{}
	r.monitor = networkstate.NewMonitor(source, time.Millisecond)
	intent := relayprofile.Plan([]relayprofile.Candidate{{PublicURL: "https://own.example"}})
	selection, err := r.Plan(t.Context(), intent, "")
	if err != nil {
		t.Fatal(err)
	}
	node, configured := selection.Plan.Network[0], selection.Plan.Configured[0]
	stream, err := diagnostics.NewStream(16, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	observe := r.Observer(selection, stream.Sink())
	start := senderrelay.Attempt{State: senderrelay.AttemptStarted}
	observe(node.URL, start)
	observe(configured.URL, start)
	now := r.now()
	r.monitor.Poll(t.Context(), now)
	fail := senderrelay.Attempt{State: senderrelay.AttemptFailed, Failure: &relayv2.WebSocketDialError{StatusCode: 503}, ServerRetryAt: now.Add(time.Minute)}
	observe(node.URL, fail)
	observe(configured.URL, fail)
	facts := r.health.Snapshot(r.generation(), now)
	if len(facts) != 1 || facts[0].Identity != node.Identity {
		t.Fatal("bootstrap or configured scope", facts)
	}
	<-stream.Records()
	<-stream.Records()
	update := (<-stream.Records()).Payload()
	if update.Get("cooldown_reason").Text() != "server_backoff" || update.Get("http_status").Uint() != 503 || update.Get("consecutive_failures").Uint() != 1 || update.Get("server_retry_at").Text() == "" {
		t.Fatal(update)
	}
	if own := (<-stream.Records()).Payload(); own.Get("http_status").Uint() != 503 || own.Get("cooldown_reason").Present() {
		t.Fatal(own)
	}
	cooled, err := r.Plan(t.Context(), intent, "")
	if err != nil || !cooled.DiagnosticRecord().Payload().Get("cooldown_fallback").Bool() || !cooled.Decision.Candidates[1].ServerRetryAt.Equal(fail.ServerRetryAt) {
		t.Fatal(cooled, err)
	}
	source.state.ResumeSequence++
	r.monitor.Poll(t.Context(), now.Add(time.Millisecond))
	r.monitor.Poll(t.Context(), now.Add(2*time.Millisecond))
	if r.generation() != 2 {
		t.Fatal("test failed to change networks")
	}
	observe(node.URL, senderrelay.Attempt{State: senderrelay.AttemptSucceeded})
	if len(r.health.Snapshot(2, now)) != 0 {
		t.Fatal("late old success survived true network change")
	}
	observe(node.URL, start)
	observe(node.URL, senderrelay.Attempt{State: senderrelay.AttemptSucceeded, HandshakeDuration: time.Millisecond})
	r.health.Snapshot(1, now)
	if newFacts := r.health.Snapshot(2, now); len(newFacts) != 1 || newFacts[0].Duration != time.Millisecond {
		t.Fatal(newFacts)
	}
}

func TestDelayedCallbacksOnlyExtendDeadlinesBeyondTheLatestSample(t *testing.T) {
	var h nodeHealth
	start := time.Now()
	latest := start.Add(time.Second)
	h.RecordAttempt("node", 1, 1, latest, senderrelay.Attempt{State: senderrelay.AttemptSucceeded})
	fail := senderrelay.Attempt{State: senderrelay.AttemptFailed, Failure: &relayv2.WebSocketDialError{StatusCode: 503}}
	for _, elapsed := range []time.Duration{-time.Second, time.Millisecond, time.Second} {
		fail.ServerRetryAt = start.Add(elapsed)
		if _, change := h.RecordAttempt("node", 1, 1, start, fail); change != relayplan.CooldownNone {
			t.Fatalf("expired hint changed a newer success: %+v", fail)
		}
	}
	fail.ServerRetryAt = latest.Add(time.Minute)
	var workers sync.WaitGroup
	for i := range 16 {
		workers.Go(func() {
			attempt := fail
			attempt.ServerRetryAt = attempt.ServerRetryAt.Add(-time.Duration(i) * time.Millisecond)
			h.RecordAttempt("node", 1, 1, start, attempt)
			h.Snapshot(1, latest)
		})
	}
	workers.Wait()
	entry := h.Snapshot(1, latest)[0]
	if !entry.Admitted || entry.At != latest || entry.ServerRetryAt != fail.ServerRetryAt {
		t.Fatal(entry)
	}
	if fact := h.Snapshot(1, fail.ServerRetryAt)[0]; !fact.ServerRetryAt.IsZero() {
		t.Fatal(fact)
	}
}

func TestAttemptDiagnosticsDistinguishLocalReasonFromServerExtension(t *testing.T) {
	r := fixtureRuntime(t)
	now := r.now()
	r.now = func() time.Time { return now }
	selection, err := r.Plan(t.Context(), relayprofile.Plan(nil), "")
	if err != nil {
		t.Fatal(err)
	}
	node := selection.Plan.Network[0]
	stream, _ := diagnostics.NewStream(8, nil)
	defer stream.Close()
	observe := r.Observer(selection, stream.Sink())
	observe(node.URL, senderrelay.Attempt{State: senderrelay.AttemptStarted})
	<-stream.Records()
	hint, _ := networkendpoint.WebSocketBaseHint("https://relay.example/v2/ws")
	observe(node.URL, senderrelay.Attempt{State: senderrelay.AttemptFailed, Failure: &relayv2.WebSocketRouteError{BaseHint: hint}})
	<-stream.Records()
	now = now.Add(time.Second)
	hard := now.Add(30 * time.Second)
	observe(node.URL, senderrelay.Attempt{State: senderrelay.AttemptFailed, Failure: &relayv2.WebSocketDialError{StatusCode: 503}, ServerRetryAt: hard})
	if fields := (<-stream.Records()).Payload(); fields.Get("cooldown_reason").Text() != "server_backoff" || fields.Get("cooldown_ms").Uint() != 30000 {
		t.Fatal(fields)
	}
	now = hard
	if !r.ServerRetryAt(node.Identity).IsZero() {
		t.Fatal("admission retained an expired server deadline")
	}
	observe(node.URL, senderrelay.Attempt{State: senderrelay.AttemptFailed, Failure: context.DeadlineExceeded})
	if fields := (<-stream.Records()).Payload(); fields.Get("server_retry_at").Present() {
		t.Fatal(fields)
	}
}

func TestDiagnosticRecordsIncludeReadableAttemptStatesPlanTimingsAndAdmissionReason(t *testing.T) {
	r := fixtureRuntime(t)
	intent := relayprofile.Plan([]relayprofile.Candidate{{PublicURL: "https://own.example"}})
	initial, err := r.Plan(t.Context(), intent, "")
	if err != nil {
		t.Fatal(err)
	}
	node := initial.Plan.Network[0]
	stream, err := diagnostics.NewStream(16, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	observe := r.Observer(initial, stream.Sink())
	states := []struct {
		state senderrelay.AttemptState
		name  string
	}{
		{senderrelay.AttemptStarted, "started"},
		{senderrelay.AttemptStage, "stage"},
		{senderrelay.AttemptAuthorization, "authorization"},
		{senderrelay.AttemptWaiting, "waiting"},
		{senderrelay.AttemptParked, "parked"},
		{senderrelay.AttemptSucceeded, "succeeded"},
		{senderrelay.AttemptFailed, "failed"},
	}
	for _, tc := range states {
		attempt := senderrelay.Attempt{State: tc.state}
		if tc.state == senderrelay.AttemptSucceeded {
			attempt.HandshakeDuration = 145 * time.Millisecond
		}
		observe(node.URL, attempt)
		record := <-stream.Records()
		if got := record.Payload().Get("state").Text(); got != tc.name {
			t.Fatalf("attempt state %d emitted %q, want %q", tc.state, got, tc.name)
		}
	}
	// Record a fresh admitted sample so the next plan exposes observed_handshake_ms.
	observe(node.URL, senderrelay.Attempt{State: senderrelay.AttemptStarted})
	<-stream.Records()
	observe(node.URL, senderrelay.Attempt{State: senderrelay.AttemptSucceeded, HandshakeDuration: 145 * time.Millisecond})
	<-stream.Records()

	planned, err := r.Plan(t.Context(), intent, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := planned.DiagnosticRecord().Payload().Get("published_candidates").MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var candidates []struct {
		Endpoint            string `json:"endpoint"`
		Configured          bool   `json:"configured"`
		Observation         string `json:"observation"`
		ObservedHandshakeMS string `json:"observed_handshake_ms"`
	}
	if err := json.Unmarshal(raw, &candidates); err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 || !candidates[0].Configured || candidates[0].Observation != "" || candidates[0].ObservedHandshakeMS != "" {
		t.Fatalf("configured candidate carried directory observation: %+v", candidates)
	}
	if candidates[1].Endpoint != node.URL || candidates[1].Observation != "admitted" || candidates[1].ObservedHandshakeMS != "145" {
		t.Fatalf("directory candidate missing observed handshake timing: %+v", candidates[1])
	}

	admission := AdmissionRecord(relayset.Admission{
		Endpoint: node.URL, Primary: "https://primary.example",
		State: relayset.AdmissionParked, Reason: relayset.AdmissionReasonBackupNotChosen, Count: 1,
	})
	if admission.Payload().Get("reason").Text() != relayset.AdmissionReasonBackupNotChosen {
		t.Fatalf("admission record lost reason: %+v", admission.Payload())
	}
}
