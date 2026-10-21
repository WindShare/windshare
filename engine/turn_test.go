package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/windshare/windshare/connectivity/turnpath"
	"github.com/windshare/windshare/engine/turn"
)

func TestTURNConfigurationKeepsAccountBudgetAndOwnsItsInputs(t *testing.T) {
	for _, initial := range []bool{false, true} {
		t.Run(map[bool]string{false: "configure", true: "new"}[initial], func(t *testing.T) {
			config := turn.ManualSettings{
				Accounts: []turn.AccountSettings{{ID: "owner", Provider: "standard", Issuer: "static",
					LocalBudgetBytes: 100, Credentials: json.RawMessage(`{"username":"user","password":"password"}`)}},
				Profiles: []turn.ProfileSettings{{ID: "primary", Account: "owner", URLs: []string{"turn:localhost"}}},
			}
			saved := config.Clone()
			engineConfig := Config{}
			if initial {
				engineConfig.TURN = turn.Settings{Manual: config}
			}
			application := newTestEngine(t, engineConfig)
			selection, release := application.turn.ForShare(t.Context(), nil)
			defer release()
			if !initial {
				if err := application.ConfigureTURN(turn.Settings{Manual: config}); err != nil {
					t.Fatal(err)
				}
			}
			config.Accounts[0].Credentials[0] = '!'
			config.Profiles[0].URLs[0] = "turn:changed"
			lease, err := selection.Acquire(t.Context(), "primary", turnpath.Subject{SessionID: [16]byte{1}, PathID: [16]byte{2}})
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			if lease.Relay().URLs()[0] != "turn:localhost" || lease.Relay().Credentials().Username() != "user" {
				t.Fatal("caller mutation changed configured authority")
			}
			if lease.Charge(turnpath.Usage{TransportSent: 100}) {
				t.Fatal("account budget was not exhausted")
			}
			if err = application.ConfigureTURN(turn.Settings{Manual: saved}); err != nil {
				t.Fatal(err)
			}
			status := application.TURNStatus()
			if len(status) != 1 || !status[0].Exhausted || status[0].ChargedBytes != 100 {
				t.Fatalf("re-import reset shared budget: %+v", status)
			}
			if _, err = selection.Acquire(t.Context(), "primary", turnpath.Subject{SessionID: [16]byte{3}, PathID: [16]byte{4}}); !errors.Is(err, turn.ErrBudget) {
				t.Fatalf("new session bypassed exhausted account: %v", err)
			}
			saved.Accounts[0].LocalBudgetBytes++
			if err = application.ConfigureTURN(turn.Settings{Manual: saved}); err != nil {
				t.Fatal(err)
			}
			status = application.TURNStatus()
			if status[0].ChargedBytes != 100 || status[0].LocalBudgetBytes != 101 || status[0].Exhausted {
				t.Fatalf("budget update lost accumulated usage: %+v", status)
			}
			if lease.Charge(turnpath.Usage{}) {
				t.Fatal("raising budget revived a revoked lease")
			}
			next, err := selection.Acquire(t.Context(), "primary", turnpath.Subject{SessionID: [16]byte{5}, PathID: [16]byte{6}})
			if err != nil {
				t.Fatal(err)
			}
			if next.Charge(turnpath.Usage{TransportSent: 1}) {
				t.Fatal("new lease bypassed retained usage")
			}
			next.Close()
			lease.Close()
			if err = application.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err = application.ConfigureTURN(turn.Settings{Manual: saved}); !errors.Is(err, ErrClosed) {
				t.Fatalf("closed engine accepted configuration: %v", err)
			}
		})
	}
}
