package share

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/windshare/windshare/engine/relayprofile"

	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/engine/internal/task"
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
