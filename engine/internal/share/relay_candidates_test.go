package share

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/engine/internal/task"
)

func TestEveryRelayReceivesOnlyItsOwnRegistrationCredential(t *testing.T) {
	candidates := []senderrelay.Candidate{
		{PublicURL: "https://one.example", RegistrationToken: "private-one"},
		{PublicURL: "https://two.example", RegistrationToken: "private-two"},
	}
	dependencies := nativeDependencies(t)
	assembled := make(chan senderrelay.Candidate, len(candidates))
	dependencies.Relay = func(config senderrelay.Config) (Relay, error) {
		assembled <- config.Candidate
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
