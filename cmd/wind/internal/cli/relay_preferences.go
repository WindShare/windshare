package cli

import (
	"context"

	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relaysetup"
)

func (app *App) defaultFrontend() string {
	if network := app.engineConfig.Network; network != nil {
		return network.LinkHost
	}
	return "default receiver website"
}

func relayEnabledLabel(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

func (app *App) relayPreferences(ctx context.Context, args []string) int {
	store := app.engineConfig.RelaySetup.Store
	if store == nil {
		return app.writeRelaySetupFailure(relayconfig.ErrConfig)
	}
	var saved relaysetup.Snapshot
	var err error
	switch {
	case args[0] == "list" && len(args) == 1:
		saved, err = store.Load()
	case args[0] != "list" && len(args) == 2:
		change := relayconfig.PreferenceEdit{Action: args[0], Value: args[1]}
		if change.Action == "frontend" && change.Value == "default" {
			change.Value = ""
		}
		saved, err = store.Update(ctx, func(snapshot *relaysetup.Snapshot) error { return change.Apply(&snapshot.Config) })
	default:
		app.writeCompleteLine("%s", relayUsage)
		return ExitUsage
	}
	if err != nil {
		return app.writeRelaySetupFailure(err)
	}
	view := saved.Preferences(app.defaultFrontend())
	app.writeCompleteLine("Relay mode: %s. Share website: %s", view.Mode, view.Frontend)
	for _, relay := range view.Relays {
		app.writeCompleteLine("%s (%s)", relay.URL, relayEnabledLabel(relay.Enabled))
	}
	if view.AtCapacity && view.Mode == relayconfig.Automatic {
		app.writeCompleteLine("All eight relay slots are selected. Disable a relay to make room for the built-in network.")
	}
	return ExitOK
}
