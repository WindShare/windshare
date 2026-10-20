package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/windshare/windshare/connectivity/turnpath"
	"github.com/windshare/windshare/engine/turnaccount"
	"github.com/windshare/windshare/engine/turnconfig"
)

func TestTURNConfigurationKeepsAccountBudgetAndOwnsItsInputs(t *testing.T) {
	for _, initial := range []bool{false, true} {
		t.Run(map[bool]string{false: "configure", true: "new"}[initial], func(t *testing.T) {
			config := turnconfig.Config{
				Accounts: []turnconfig.Account{{ID: "owner", Provider: "standard", Issuer: "static",
					LocalBudgetBytes: 100, Credentials: json.RawMessage(`{"username":"user","password":"password"}`)}},
				Profiles: []turnconfig.Profile{{ID: "primary", Account: "owner", URLs: []string{"turn:localhost"}}},
			}
			saved := config.Clone()
			engineConfig := Config{}
			if initial {
				engineConfig.TURN = config
			}
			application := newTestEngine(t, engineConfig)
			if !initial {
				if err := application.ConfigureTURN(config); err != nil {
					t.Fatal(err)
				}
			}
			config.Accounts[0].Credentials[0] = '!'
			config.Profiles[0].URLs[0] = "turn:changed"
			lease, err := application.turn.Acquire(t.Context(), "primary", turnpath.Subject{SessionID: [16]byte{1}, PathID: [16]byte{2}})
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
			if err = application.ConfigureTURN(saved); err != nil {
				t.Fatal(err)
			}
			status := application.TURNStatus()
			if len(status) != 1 || !status[0].Exhausted || status[0].ChargedBytes != 100 {
				t.Fatalf("re-import reset shared budget: %+v", status)
			}
			if _, err = application.turn.Acquire(t.Context(), "primary", turnpath.Subject{SessionID: [16]byte{3}, PathID: [16]byte{4}}); !errors.Is(err, turnaccount.ErrBudget) {
				t.Fatalf("new session bypassed exhausted account: %v", err)
			}
			saved.Accounts[0].LocalBudgetBytes++
			if err = application.ConfigureTURN(saved); err == nil {
				t.Fatal("reconfiguration reset an existing account")
			}
			lease.Close()
			if err = application.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err = application.ConfigureTURN(saved); !errors.Is(err, ErrClosed) {
				t.Fatalf("closed engine accepted configuration: %v", err)
			}
		})
	}
}
