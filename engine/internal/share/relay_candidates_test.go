package share

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/engine/relayprofile"

	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/engine/internal/task"
	"github.com/windshare/windshare/transport/relayv2"
)

func TestEveryRelayReceivesOnlyItsOwnRegistrationCredential(t *testing.T) {
	candidates := []relayprofile.Registration{
		{PublicURL: "https://one.example", Authorization: relayset.StaticKey("private-one")},
		{PublicURL: "https://two.example", Authorization: relayset.StaticKey("private-two")},
	}
	dependencies := nativeDependencies(t)
	assembled := make(chan relayprofile.Registration, len(candidates))
	dependencies.Relay = func(config relayset.SenderLifecycleConfig) (Relay, error) {
		assembled <- relayprofile.Registration{PublicURL: config.Candidate.PublicURL, Authorization: config.Candidate.Authorization}
		return &readyEndpoint{testRelays: &testRelays{step: func(string) {}, stopped: make(chan struct{})}}, nil
	}
	ctx, stop := context.WithCancelCause(t.Context())
	defer stop(task.ErrShareStopped)
	root := t.TempDir()
	done := make(chan task.Completion[Result], 1)
	go func() {
		done <- Run(ctx, Request{Source: nativeSource([]string{root}), Relays: candidates}, dependencies)
	}()
	ready, err := dependencies.Controller.Ready(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ready.Capability.Relays, []string{candidates[0].PublicURL, candidates[1].PublicURL}) {
		t.Fatal("capability did not retain exactly the public candidate set")
	}
	for range candidates {
		select {
		case candidate := <-assembled:
			if !slices.Contains(candidates, candidate) {
				t.Fatal("registration authority moved across candidate boundaries")
			}
		case <-time.After(testWaitTimeout):
			t.Fatal("candidate was not assembled")
		}
	}
	stop(task.ErrShareStopped)
	if result := awaitResult(t, done); result.Err != nil {
		t.Fatal(result.Err)
	}
}

func TestCanaryRegistersTheSamePreparedShareUsingItsOwnCredential(t *testing.T) {
	active := relayprofile.Registration{PublicURL: "https://active.example", Authorization: relayset.StaticKey("active-token")}
	canary := relayprofile.Registration{PublicURL: "https://canary.example", Authorization: relayset.StaticKey("canary-token")}
	dependencies := nativeDependencies(t)
	dependencies.Relays = PlannedRelays(relayplan.Plan{Network: []relayplan.Candidate{{URL: active.PublicURL}},
		Standby: []relayplan.Candidate{{URL: canary.PublicURL, Canary: true}}}, nil)
	attempts := make(chan relayset.SenderLifecycleConfig, 2)
	dependencies.Relay = func(config relayset.SenderLifecycleConfig) (Relay, error) {
		attempts <- config
		if config.Candidate.PublicURL == active.PublicURL {
			return nil, errors.New("relay refused registration")
		}
		return &readyEndpoint{testRelays: &testRelays{step: func(string) {}, stopped: make(chan struct{})}}, nil
	}
	ctx, stop := context.WithCancelCause(t.Context())
	defer stop(task.ErrShareStopped)
	request := Request{Source: nativeSource([]string{t.TempDir()}), Relays: []relayprofile.Registration{active}, Standby: []relayprofile.Registration{canary}}
	done := make(chan task.Completion[Result], 1)
	go func() { done <- Run(ctx, request, dependencies) }()
	wait, cancel := context.WithTimeout(ctx, testWaitTimeout)
	defer cancel()
	ready, err := dependencies.Controller.Ready(wait)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ready.Registrations.Candidates, []string{canary.PublicURL}) {
		t.Fatal(ready.Registrations)
	}
	first, second := <-attempts, <-attempts
	if first.Candidate.Authorization != active.Authorization || second.Candidate.Authorization != canary.Authorization ||
		first.Fresh.ShareID != second.Fresh.ShareID || first.Fresh.ShareInstance != second.Fresh.ShareInstance ||
		first.Fresh.DescriptorDigest != second.Fresh.DescriptorDigest || first.ResumeToken == second.ResumeToken {
		t.Fatal("fallback changed the share identity or reused endpoint-specific authority")
	}
	stop(task.ErrShareStopped)
	if result := awaitResult(t, done); result.Err != nil {
		t.Fatal(result.Err)
	}
}

type deferredRelayEndpoint struct {
	*readyEndpoint
	ready    chan struct{}
	observed chan struct{}
	mu       sync.Mutex
	observer func(relayset.RegistrationState)
}

func (e *deferredRelayEndpoint) WaitReady(ctx context.Context) error {
	select {
	case <-e.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *deferredRelayEndpoint) ObserveRegistration(observe func(relayset.RegistrationState)) {
	e.mu.Lock()
	e.observer = observe
	e.mu.Unlock()
	observe(relayset.RegistrationPending)
	close(e.observed)
}

func (e *deferredRelayEndpoint) admit() {
	e.mu.Lock()
	observer := e.observer
	e.mu.Unlock()
	if observer != nil {
		observer(relayset.RegistrationAvailable)
	}
	close(e.ready)
}

func (*deferredRelayEndpoint) CompleteObservations() relayv2.LifecycleObservationCompletion {
	return relayv2.LifecycleObservationCompletion{}
}

func TestSharePublicationRetiresUnverifiedCommunityRouteBeforeRegistration(t *testing.T) {
	const (
		officialURL = "https://official.example"
		warmURL     = "https://comm-warm.example"
		coldURL     = "https://comm-cold.example"
	)
	coldEntered := make(chan struct{})
	coldCheckDone := make(chan struct{})
	var coldPrepared atomic.Int32
	warmEndpoint := &deferredRelayEndpoint{
		readyEndpoint: &readyEndpoint{testRelays: &testRelays{step: func(string) {}, stopped: make(chan struct{})}},
		ready:         make(chan struct{}),
		observed:      make(chan struct{}),
	}

	coldRoute := &relayprofile.CommunityRoute{
		Client: &http.Client{},
		Origin: "https://front.example",
		Check: func(ctx context.Context) error {
			close(coldEntered)
			<-ctx.Done()
			close(coldCheckDone)
			// Return nil even after ctx cancellation to exercise the post-Check ctx guard.
			return nil
		},
	}
	warmRoute := &relayprofile.CommunityRoute{
		Client: &http.Client{},
		Origin: "https://front.example",
		Check:  func(context.Context) error { return nil },
	}
	warmRoute.Ready.Store(true)

	stream, err := diagnostics.NewStream(64, nil)
	if err != nil {
		t.Fatal(err)
	}
	dependencies := nativeDependencies(t)
	dependencies.PollNetwork = func(ctx context.Context, _ func()) { <-ctx.Done() }
	dependencies.Relays = PlannedRelays(relayplan.Plan{
		Network: []relayplan.Candidate{
			{URL: officialURL, Source: relayplan.Official, Zone: "cn"},
			{URL: warmURL, Source: relayplan.Community, Verified: true},
			{URL: coldURL, Source: relayplan.Community},
		},
	}, nil)
	dependencies.Relay = func(config relayset.SenderLifecycleConfig) (Relay, error) {
		switch config.Candidate.PublicURL {
		case officialURL:
			awaitShareSignal(t, coldEntered)
			return &readyEndpoint{testRelays: &testRelays{step: func(string) {}, stopped: make(chan struct{})}}, nil
		case warmURL:
			return warmEndpoint, nil
		case coldURL:
			coldPrepared.Add(1)
			return &readyEndpoint{testRelays: &testRelays{step: func(string) {}, stopped: make(chan struct{})}}, nil
		default:
			return nil, errors.New("unexpected relay " + config.Candidate.PublicURL)
		}
	}

	ctx, stop := context.WithCancelCause(t.Context())
	defer stop(task.ErrShareStopped)
	request := Request{
		Source: nativeSource([]string{t.TempDir()}),
		Relays: []relayprofile.Registration{
			{PublicURL: officialURL, Authorization: relayset.StaticKey("official-token")},
			{PublicURL: warmURL, Community: warmRoute},
			{PublicURL: coldURL, Community: coldRoute},
		},
		Diagnostics: stream.Sink(),
	}
	done := make(chan task.Completion[Result], 1)
	go func() { done <- Run(ctx, request, dependencies) }()

	wait, cancel := context.WithTimeout(ctx, testWaitTimeout)
	defer cancel()
	ready, err := dependencies.Controller.Ready(wait)
	if err != nil {
		t.Fatal(err)
	}
	awaitShareSignal(t, coldCheckDone)
	awaitShareSignal(t, warmEndpoint.observed)

	if !slices.Equal(ready.Registrations.Candidates, []string{warmURL, officialURL}) {
		t.Fatalf("published candidates = %v, want [%s %s]", ready.Registrations.Candidates, warmURL, officialURL)
	}
	if len(ready.Registrations.Rejected) != 0 {
		t.Fatalf("retired unverified route polluted Rejected: %v", ready.Registrations.Rejected)
	}
	if status, ok := dependencies.Controller.RelayStatus(); !ok || status.Availability.Total != 2 || status.Availability.Available != 1 {
		t.Fatalf("relay status after publication = (%+v, %v), want Total=2 Available=1", status, ok)
	}

	// Verified community route stays alive and finishes registering after publication.
	warmEndpoint.admit()
	deadline := time.Now().Add(testWaitTimeout)
	for {
		status, ok := dependencies.Controller.RelayStatus()
		if ok && status.Availability.Available == 2 && status.Availability.Total == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("verified community route did not admit after publication: (%+v, %v)", status, ok)
		}
		time.Sleep(time.Millisecond)
	}

	stop(task.ErrShareStopped)
	if result := awaitResult(t, done); result.Err != nil {
		t.Fatal(result.Err)
	}
	if coldPrepared.Load() != 0 || coldRoute.Ready.Load() {
		t.Fatalf("retired unverified community route progressed after cancellation: prepared=%d ready=%v",
			coldPrepared.Load(), coldRoute.Ready.Load())
	}

	stream.Close()
	var retiredRelays []string
	for record := range stream.Records() {
		if record.Name() != "share_publication_retired_unverified_routes" {
			continue
		}
		raw, err := record.Payload().Get("retired_relays").MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &retiredRelays); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(retiredRelays, []string{coldURL}) {
		t.Fatalf("retired diagnostic = %v, want [%s]", retiredRelays, coldURL)
	}
}
