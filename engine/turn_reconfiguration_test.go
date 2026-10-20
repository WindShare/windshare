package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/windshare/windshare/connectivity/turnpath"
	"github.com/windshare/windshare/engine/internal/task"
	"github.com/windshare/windshare/engine/turnconfig"
)

func TestTURNConfigurationUpdatesWhileApplicationTaskRemainsRunning(t *testing.T) {
	application := newTestEngine(t, Config{})
	owner := application.turn
	started := make(chan struct{})
	current, err := start(application, t.Context(), func(ctx context.Context, _ task.Control) task.Completion[int] {
		close(started)
		<-ctx.Done()
		return task.Completion[int]{Settlement: task.Settlement{Outcome: task.OutcomeSuccess}}
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	config := turnconfig.Config{
		Accounts: []turnconfig.Account{{ID: "owner", Provider: "standard", Issuer: "static",
			LocalBudgetBytes: 100, Credentials: json.RawMessage(`{"username":"old","password":"password"}`)}},
		Profiles: []turnconfig.Profile{{ID: "primary", Account: "owner", URLs: []string{"turn:localhost:3478"}}},
	}
	if err = application.ConfigureTURN(config); err != nil {
		t.Fatal(err)
	}
	old, err := owner.Acquire(t.Context(), "primary", turnpath.Subject{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(old.Close)
	old.Charge(turnpath.Usage{TransportSent: 40})
	config.Accounts[0].Credentials = json.RawMessage(`{"username":"new","password":"password"}`)
	config.Accounts[0].LocalBudgetBytes = 200
	config.Profiles[0].URLs[0] = "turn:localhost:3479"
	if err = application.ConfigureTURN(config); err != nil {
		t.Fatal(err)
	}
	next, err := owner.Acquire(t.Context(), "primary", turnpath.Subject{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(next.Close)
	if application.turn != owner || old.Loss() != nil || next.Relay().Credentials().Username() != "new" ||
		next.Relay().URLs()[0] != "turn:localhost:3479" || application.TURNStatus()[0].ChargedBytes != 40 {
		t.Fatal("configuration update replaced the owner, interrupted a lease, or lost settings/usage")
	}
	select {
	case <-current.Done():
		t.Fatal("TURN settings stopped an unrelated application task")
	default:
	}
}
